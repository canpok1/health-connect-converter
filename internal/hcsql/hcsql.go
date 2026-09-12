// Package hcsql はエクスポートDBを読む定型処理を提供する。
//
// 種別ごとの型もモデルもここには置かない（ADR 0012）。時刻の持ち方の類型ごとに
// 「どの列をどう読むか」の組み立てだけを担い、読んだ値の意味づけ（単位変換・
// フィールド名）は種別ごとのコードが行う。
package hcsql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// identifierRe は列名の検証に使う。プレースホルダに置けない識別子をSQLへ連結
// するため、インジェクションを防ぐにはここを通す必要がある。
var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// tableNameRe はテーブル名の検証に使う。エクスポートDBは series の親テーブルの
// 一部を CamelCase で命名している（例: SpeedRecordTable）ため大文字を許す。
var tableNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// Instant は瞬間の記録1行。
type Instant struct {
	UUID       string
	Time       int64
	ZoneOffset int32
	AppID      string
	// Values は valueCols と同じ順。NULL は Valid=false。
	Values []sql.NullFloat64
}

// Interval は期間の記録1行。
type Interval struct {
	UUID       string
	StartTime  int64
	EndTime    int64
	ZoneOffset int32
	AppID      string
	Values     []sql.NullFloat64
}

// Sample は連続測定（series）の子1行。子に uuid は無いため親の uuid を持つ。
type Sample struct {
	ParentUUID  string
	EpochMillis int64
	ZoneOffset  int32
	AppID       string
	Values      []sql.NullFloat64
}

// Segment は期間と種別を持つ子1行（睡眠ステージなど）。親の終了時刻も返す。
type Segment struct {
	ParentUUID string
	ParentEnd  int64
	StartTime  int64
	EndTime    int64
	Type       int64
	ZoneOffset int32
	AppID      string
}

// ReadInstant は瞬間の記録を読む。テーブルが無ければ空を返す。
func ReadInstant(ctx context.Context, db *sql.DB, table string, valueCols []string) ([]Instant, error) {
	sel, err := selectList(table, "t", valueCols)
	if err != nil {
		return nil, err
	}
	ok, err := TableExists(ctx, db, table)
	if err != nil || !ok {
		return nil, err
	}

	query := fmt.Sprintf(
		"SELECT lower(hex(t.uuid)), t.time, t.zone_offset, COALESCE(a.package_name, '')%s"+
			" FROM %s t LEFT JOIN application_info_table a ON a.row_id = t.app_info_id",
		sel, table,
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("hcsql: query %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Instant{}
	for rows.Next() {
		r := Instant{Values: make([]sql.NullFloat64, len(valueCols))}
		dest := append([]any{&r.UUID, &r.Time, &r.ZoneOffset, &r.AppID}, ptrs(r.Values)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("hcsql: scan %s: %w", table, err)
		}
		out = append(out, r)
	}
	return out, wrapErr(rows, table)
}

// ReadInterval は期間の記録を読む。テーブルが無ければ空を返す。
// ゾーンオフセットは開始側（start_zone_offset）を使う。
func ReadInterval(ctx context.Context, db *sql.DB, table string, valueCols []string) ([]Interval, error) {
	sel, err := selectList(table, "t", valueCols)
	if err != nil {
		return nil, err
	}
	ok, err := TableExists(ctx, db, table)
	if err != nil || !ok {
		return nil, err
	}

	query := fmt.Sprintf(
		"SELECT lower(hex(t.uuid)), t.start_time, t.end_time, t.start_zone_offset, COALESCE(a.package_name, '')%s"+
			" FROM %s t LEFT JOIN application_info_table a ON a.row_id = t.app_info_id",
		sel, table,
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("hcsql: query %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Interval{}
	for rows.Next() {
		r := Interval{Values: make([]sql.NullFloat64, len(valueCols))}
		dest := append([]any{&r.UUID, &r.StartTime, &r.EndTime, &r.ZoneOffset, &r.AppID}, ptrs(r.Values)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("hcsql: scan %s: %w", table, err)
		}
		out = append(out, r)
	}
	return out, wrapErr(rows, table)
}

// ReadSeries は連続測定を親子の結合で読む。どちらかのテーブルが無ければ空を返す。
func ReadSeries(ctx context.Context, db *sql.DB, parentTable, childTable string, valueCols []string) ([]Sample, error) {
	sel, err := selectList(childTable, "s", valueCols)
	if err != nil {
		return nil, err
	}
	if err := validateTable(parentTable); err != nil {
		return nil, err
	}
	okParent, err := TableExists(ctx, db, parentTable)
	if err != nil {
		return nil, err
	}
	okChild, err := TableExists(ctx, db, childTable)
	if err != nil || !okParent || !okChild {
		return nil, err
	}

	query := fmt.Sprintf(
		"SELECT lower(hex(p.uuid)), s.epoch_millis, p.start_zone_offset, COALESCE(a.package_name, '')%s"+
			" FROM %s s JOIN %s p ON p.row_id = s.parent_key"+
			" LEFT JOIN application_info_table a ON a.row_id = p.app_info_id",
		sel, childTable, parentTable,
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("hcsql: query %s: %w", childTable, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Sample{}
	for rows.Next() {
		r := Sample{Values: make([]sql.NullFloat64, len(valueCols))}
		dest := append([]any{&r.ParentUUID, &r.EpochMillis, &r.ZoneOffset, &r.AppID}, ptrs(r.Values)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("hcsql: scan %s: %w", childTable, err)
		}
		out = append(out, r)
	}
	return out, wrapErr(rows, childTable)
}

// ReadSegment は期間と種別を持つ子行を親との結合で読む。どちらかのテーブルが
// 無ければ空を返す。
func ReadSegment(ctx context.Context, db *sql.DB, parentTable, childTable, startCol, endCol, typeCol string) ([]Segment, error) {
	if err := validateTable(parentTable); err != nil {
		return nil, err
	}
	if err := validateTable(childTable); err != nil {
		return nil, err
	}
	for _, c := range []string{startCol, endCol, typeCol} {
		if !identifierRe.MatchString(c) {
			return nil, fmt.Errorf("hcsql: invalid column %q", c)
		}
	}
	okParent, err := TableExists(ctx, db, parentTable)
	if err != nil {
		return nil, err
	}
	okChild, err := TableExists(ctx, db, childTable)
	if err != nil || !okParent || !okChild {
		return nil, err
	}

	query := fmt.Sprintf(
		"SELECT lower(hex(p.uuid)), p.end_time, s.%s, s.%s, s.%s, p.start_zone_offset, COALESCE(a.package_name, '')"+
			" FROM %s s JOIN %s p ON p.row_id = s.parent_key"+
			" LEFT JOIN application_info_table a ON a.row_id = p.app_info_id",
		startCol, endCol, typeCol, childTable, parentTable,
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("hcsql: query %s: %w", childTable, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Segment{}
	for rows.Next() {
		var r Segment
		if err := rows.Scan(&r.ParentUUID, &r.ParentEnd, &r.StartTime, &r.EndTime, &r.Type, &r.ZoneOffset, &r.AppID); err != nil {
			return nil, fmt.Errorf("hcsql: scan %s: %w", childTable, err)
		}
		out = append(out, r)
	}
	return out, wrapErr(rows, childTable)
}

// TableExists はテーブルの有無を返す。エクスポートDBは端末やアプリの構成で
// テーブルを持たないことがあるため、呼び出し側は無い場合を正常として扱う。
func TableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	if err := validateTable(table); err != nil {
		return false, err
	}
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hcsql: look up %s: %w", table, err)
	}
	return n > 0, nil
}

func validateTable(table string) error {
	if !tableNameRe.MatchString(table) {
		return fmt.Errorf("hcsql: invalid table name %q", table)
	}
	return nil
}

// selectList は値列を ", <別名>.<列>" の並びにする。列名は必ず検証する。
func selectList(table, alias string, valueCols []string) (string, error) {
	if err := validateTable(table); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range valueCols {
		if !identifierRe.MatchString(c) {
			return "", fmt.Errorf("hcsql: invalid column %q", c)
		}
		fmt.Fprintf(&sb, ", %s.%s", alias, c)
	}
	return sb.String(), nil
}

func ptrs(vals []sql.NullFloat64) []any {
	out := make([]any, len(vals))
	for i := range vals {
		out[i] = &vals[i]
	}
	return out
}

func wrapErr(rows *sql.Rows, table string) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("hcsql: rows %s: %w", table, err)
	}
	return nil
}
