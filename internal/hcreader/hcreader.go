// Package hcreader は Health Connect のエクスポートZIPを展開し、内部のSQLite DBから
// 種別ごとのレコードを読み出す。
package hcreader

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

// identifierRe は column をSQLへ文字列連結する前の検証に使う。プレースホルダに
// 置けない識別子であり、未検証のまま埋め込むとSQLインジェクションにつながるため、
// config側の検証有無に関わらずここでも通す。
var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// tableNameRe は source_table/series_table の検証に使う。エクスポートDBは series
// レコードの親テーブルの一部を CamelCase で命名している（例: SpeedRecordTable）ため、
// 列名用の identifierRe より大文字を許す。空白・引用符・セミコロンは引き続き弾かれる
// ため、SQLインジェクション対策としての機能は変わらない。
var tableNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// Reader は internal/app の Reader インターフェースを満たす。
type Reader struct {
	TempDir string
}

// Read は zip を TempDir 配下に展開してから読み、一時ファイルを必ず削除する。
func (r *Reader) Read(zipFile *model.ZipFile, cfg *config.Config) (*model.ExportData, error) {
	if err := os.MkdirAll(r.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("hcreader: mkdir %s: %w", r.TempDir, err)
	}

	tmp, err := os.CreateTemp(r.TempDir, "export-*.db")
	if err != nil {
		return nil, fmt.Errorf("hcreader: create temp file: %w", err)
	}
	dbPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(dbPath) }()

	if err := ExtractDB(zipFile.Data, dbPath); err != nil {
		return nil, err
	}

	return ReadDB(dbPath, cfg)
}

// ExtractDB は zipData の中から拡張子 ".db" のエントリ（複数あれば最大サイズのもの）を
// destPath へ書き出す。
func ExtractDB(zipData []byte, destPath string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("hcreader: open zip: %w", err)
	}

	var target *zip.File
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".db") {
			continue
		}
		if target == nil || f.UncompressedSize64 > target.UncompressedSize64 {
			target = f
		}
	}
	if target == nil {
		return fmt.Errorf("hcreader: no .db entry found in zip")
	}

	rc, err := target.Open()
	if err != nil {
		return fmt.Errorf("hcreader: open zip entry %s: %w", target.Name, err)
	}
	defer func() { _ = rc.Close() }()

	if dir := filepath.Dir(destPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("hcreader: mkdir %s: %w", dir, err)
		}
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("hcreader: create %s: %w", destPath, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("hcreader: write %s: %w", destPath, err)
	}
	return nil
}

// ReadDB は展開済みのエクスポートDBを cfg の種別定義に従って読む。
func ReadDB(dbPath string, cfg *config.Config) (*model.ExportData, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", dbPath))
	if err != nil {
		return nil, fmt.Errorf("hcreader: open %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	records := make(map[string][]model.Record, len(cfg.Types))
	for _, key := range cfg.TypeKeys() {
		recs, err := readType(db, cfg.Types[key])
		if err != nil {
			return nil, fmt.Errorf("hcreader: type %q: %w", key, err)
		}
		records[key] = recs
	}

	prios, err := readAppPriorities(db)
	if err != nil {
		return nil, err
	}

	return &model.ExportData{Records: records, Priorities: prios}, nil
}

const priorityTable = "health_data_category_priority_table"

// readAppPriorities は Health Connect の「アプリの優先度」設定を読む。
// app_id_priority_order は application_info_table.row_id をカンマで並べた
// 文字列で、先頭が最優先。テーブルが無いエクスポートもありうるため、
// 存在しなければ空を返す（優先度が無くても取り込みは続けられる）。
func readAppPriorities(db *sql.DB) (model.AppPriorities, error) {
	var exists int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", priorityTable).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("hcreader: look up %s: %w", priorityTable, err)
	}
	if exists == 0 {
		return model.AppPriorities{}, nil
	}

	apps, err := readAppNames(db)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query("SELECT health_data_category, app_id_priority_order FROM " + priorityTable)
	if err != nil {
		return nil, fmt.Errorf("hcreader: query %s: %w", priorityTable, err)
	}
	defer func() { _ = rows.Close() }()

	prios := make(model.AppPriorities)
	for rows.Next() {
		var category int
		var order sql.NullString
		if err := rows.Scan(&category, &order); err != nil {
			return nil, fmt.Errorf("hcreader: scan %s: %w", priorityTable, err)
		}
		var names []string
		for _, field := range strings.Split(order.String, ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			id, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("hcreader: %s: invalid app_id_priority_order %q: %w", priorityTable, order.String, err)
			}
			// 対応するアプリが application_info_table から消えていることがある。
			// 順序だけ残っても使えないため飛ばす。
			if name, ok := apps[id]; ok {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			prios[category] = names
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hcreader: query %s: %w", priorityTable, err)
	}
	return prios, nil
}

func readAppNames(db *sql.DB) (map[int64]string, error) {
	rows, err := db.Query("SELECT row_id, package_name FROM application_info_table")
	if err != nil {
		return nil, fmt.Errorf("hcreader: query application_info_table: %w", err)
	}
	defer func() { _ = rows.Close() }()

	apps := make(map[int64]string)
	for rows.Next() {
		var id int64
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("hcreader: scan application_info_table: %w", err)
		}
		if name.Valid && name.String != "" {
			apps[id] = name.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hcreader: query application_info_table: %w", err)
	}
	return apps, nil
}

func readType(db *sql.DB, tc config.TypeConfig) ([]model.Record, error) {
	if err := validateTableName(tc.SourceTable); err != nil {
		return nil, err
	}

	switch tc.TimeLayout {
	case config.LayoutInstant:
		return readInstant(db, tc)
	case config.LayoutInterval:
		return readInterval(db, tc)
	case config.LayoutSeries:
		if err := validateTableName(tc.SeriesTable); err != nil {
			return nil, err
		}
		return readSeries(db, tc)
	default:
		return nil, fmt.Errorf("hcreader: unknown time_layout %q", tc.TimeLayout)
	}
}

func readInstant(db *sql.DB, tc config.TypeConfig) ([]model.Record, error) {
	exists, err := tableExists(db, tc.SourceTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return []model.Record{}, nil
	}

	names := sortedColumnNames(tc)
	cols, err := valueColumns(tc, names)
	if err != nil {
		return nil, err
	}

	selectCols := []string{"lower(hex(t.uuid))", "t.time", "t.zone_offset", "COALESCE(a.package_name, '')"}
	for _, c := range cols {
		selectCols = append(selectCols, "t."+c)
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s t LEFT JOIN application_info_table a ON a.row_id = t.app_info_id",
		strings.Join(selectCols, ", "), tc.SourceTable,
	)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tc.SourceTable, err)
	}
	defer func() { _ = rows.Close() }()

	recs := []model.Record{}
	for rows.Next() {
		var uuid, appID string
		var t int64
		var zoneOffset int32
		vals := make([]sql.NullFloat64, len(names))

		dest := make([]any, 0, 4+len(vals))
		dest = append(dest, &uuid, &t, &zoneOffset, &appID)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", tc.SourceTable, err)
		}

		recs = append(recs, model.Record{
			UUID:       uuid,
			StartTime:  t,
			EndTime:    t,
			ZoneOffset: zoneOffset,
			AppID:      appID,
			Values:     scanValues(names, tc, vals),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", tc.SourceTable, err)
	}
	return recs, nil
}

func readInterval(db *sql.DB, tc config.TypeConfig) ([]model.Record, error) {
	exists, err := tableExists(db, tc.SourceTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return []model.Record{}, nil
	}

	names := sortedColumnNames(tc)
	cols, err := valueColumns(tc, names)
	if err != nil {
		return nil, err
	}

	selectCols := []string{
		"lower(hex(t.uuid))", "t.start_time", "t.end_time", "t.start_zone_offset", "COALESCE(a.package_name, '')",
	}
	for _, c := range cols {
		selectCols = append(selectCols, "t."+c)
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s t LEFT JOIN application_info_table a ON a.row_id = t.app_info_id",
		strings.Join(selectCols, ", "), tc.SourceTable,
	)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tc.SourceTable, err)
	}
	defer func() { _ = rows.Close() }()

	recs := []model.Record{}
	for rows.Next() {
		var uuid, appID string
		var startTime, endTime int64
		var zoneOffset int32
		vals := make([]sql.NullFloat64, len(names))

		dest := make([]any, 0, 5+len(vals))
		dest = append(dest, &uuid, &startTime, &endTime, &zoneOffset, &appID)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", tc.SourceTable, err)
		}

		recs = append(recs, model.Record{
			UUID:       uuid,
			StartTime:  startTime,
			EndTime:    endTime,
			ZoneOffset: zoneOffset,
			AppID:      appID,
			Values:     scanValues(names, tc, vals),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", tc.SourceTable, err)
	}
	return recs, nil
}

func readSeries(db *sql.DB, tc config.TypeConfig) ([]model.Record, error) {
	sourceExists, err := tableExists(db, tc.SourceTable)
	if err != nil {
		return nil, err
	}
	seriesExists, err := tableExists(db, tc.SeriesTable)
	if err != nil {
		return nil, err
	}
	if !sourceExists || !seriesExists {
		return []model.Record{}, nil
	}

	names := sortedColumnNames(tc)
	cols, err := valueColumns(tc, names)
	if err != nil {
		return nil, err
	}

	selectCols := []string{"lower(hex(p.uuid))", "s.epoch_millis", "p.start_zone_offset", "COALESCE(a.package_name, '')"}
	for _, c := range cols {
		selectCols = append(selectCols, "s."+c)
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s s JOIN %s p ON p.row_id = s.parent_key LEFT JOIN application_info_table a ON a.row_id = p.app_info_id",
		strings.Join(selectCols, ", "), tc.SeriesTable, tc.SourceTable,
	)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tc.SeriesTable, err)
	}
	defer func() { _ = rows.Close() }()

	recs := []model.Record{}
	for rows.Next() {
		var parentUUID, appID string
		var epochMillis int64
		var zoneOffset int32
		vals := make([]sql.NullFloat64, len(names))

		dest := make([]any, 0, 4+len(vals))
		dest = append(dest, &parentUUID, &epochMillis, &zoneOffset, &appID)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", tc.SeriesTable, err)
		}

		recs = append(recs, model.Record{
			// 子行にuuidは無いため、親uuidとepoch_millisを合成して一意なキーにする。
			UUID:       parentUUID + "#" + strconv.FormatInt(epochMillis, 10),
			StartTime:  epochMillis,
			EndTime:    epochMillis,
			ZoneOffset: zoneOffset,
			AppID:      appID,
			Values:     scanValues(names, tc, vals),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", tc.SeriesTable, err)
	}
	return recs, nil
}

func scanValues(names []string, tc config.TypeConfig, vals []sql.NullFloat64) map[string]float64 {
	values := make(map[string]float64, len(names))
	for i, name := range names {
		if vals[i].Valid {
			values[name] = vals[i].Float64 * tc.Columns[name].Scale
		}
	}
	return values
}

func sortedColumnNames(tc config.TypeConfig) []string {
	names := make([]string, 0, len(tc.Columns))
	for name := range tc.Columns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// valueColumns は names に対応する実DB列名を検証したうえで返す。
func valueColumns(tc config.TypeConfig, names []string) ([]string, error) {
	cols := make([]string, len(names))
	for i, name := range names {
		col := tc.Columns[name].Column
		if err := validateIdentifier(col); err != nil {
			return nil, fmt.Errorf("column %q: %w", name, err)
		}
		cols[i] = col
	}
	return cols, nil
}

func validateIdentifier(name string) error {
	if !identifierRe.MatchString(name) {
		return fmt.Errorf("hcreader: invalid identifier %q (must match %s)", name, identifierRe.String())
	}
	return nil
}

func validateTableName(name string) error {
	if !tableNameRe.MatchString(name) {
		return fmt.Errorf("hcreader: invalid table name %q (must match %s)", name, tableNameRe.String())
	}
	return nil
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	return count > 0, nil
}
