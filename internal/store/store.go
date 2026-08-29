// Package store は Health Connect のエクスポートデータを累積保存する SQLite ストアを提供する。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

const durationValueName = "duration_min"

var dailySQLFuncs = map[string]string{
	"mean": "AVG",
	"min":  "MIN",
	"max":  "MAX",
	"sum":  "SUM",
}

// Store は累積 SQLite への読み書きを担う。
type Store struct {
	db *sql.DB
}

// Open は path の SQLite ファイルを開く。親ディレクトリが無ければ作る。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// sqlite はマルチライタに弱いため、コネクションを1本に固定して直列化する。
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close はストアを閉じる。
func (s *Store) Close() error {
	return s.db.Close()
}

func tableName(typeKey string) string {
	return "record_" + typeKey
}

func columnNames(tc config.TypeConfig) []string {
	names := make([]string, 0, len(tc.Columns))
	for name := range tc.Columns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Migrate は cfg に定義された全種別ぶんのテーブルを作成・追加更新する。
func (s *Store) Migrate(ctx context.Context, cfg *config.Config) error {
	const stateDDL = `CREATE TABLE IF NOT EXISTS state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
)`
	if _, err := s.db.ExecContext(ctx, stateDDL); err != nil {
		return fmt.Errorf("store: migrate state table: %w", err)
	}

	for _, key := range cfg.TypeKeys() {
		if err := s.migrateType(ctx, key, cfg.Types[key]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateType(ctx context.Context, typeKey string, tc config.TypeConfig) error {
	table := tableName(typeKey)
	names := columnNames(tc)

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE IF NOT EXISTS %s (\n", table)
	sb.WriteString("\tuuid        TEXT    PRIMARY KEY,\n")
	sb.WriteString("\tstart_time  INTEGER NOT NULL,\n")
	sb.WriteString("\tend_time    INTEGER NOT NULL,\n")
	sb.WriteString("\tzone_offset INTEGER NOT NULL,\n")
	sb.WriteString("\tapp_id      TEXT    NOT NULL DEFAULT ''")
	for _, name := range names {
		fmt.Fprintf(&sb, ",\n\t%s REAL", name)
	}
	sb.WriteString("\n)")

	if _, err := s.db.ExecContext(ctx, sb.String()); err != nil {
		return fmt.Errorf("store: create table %s: %w", table, err)
	}

	idxDDL := fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_start ON %s(start_time)", table, table)
	if _, err := s.db.ExecContext(ctx, idxDDL); err != nil {
		return fmt.Errorf("store: create index on %s: %w", table, err)
	}

	existing, err := s.existingColumns(ctx, table)
	if err != nil {
		return err
	}
	for _, name := range names {
		if existing[name] {
			continue
		}
		alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s REAL", table, name)
		if _, err := s.db.ExecContext(ctx, alter); err != nil {
			return fmt.Errorf("store: add column %s to %s: %w", name, table, err)
		}
	}
	return nil
}

func (s *Store) existingColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("store: table_info %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("store: scan table_info %s: %w", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: table_info %s: %w", table, err)
	}
	return cols, nil
}

// UpsertRecords は recs を種別 typeKey のテーブルへ UUID をキーに反映する。
func (s *Store) UpsertRecords(ctx context.Context, typeKey string, tc config.TypeConfig, recs []model.Record) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}

	table := tableName(typeKey)
	names := columnNames(tc)

	cols := make([]string, 0, len(names)+5)
	cols = append(cols, "uuid", "start_time", "end_time", "zone_offset", "app_id")
	cols = append(cols, names...)

	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
	}

	setParts := make([]string, 0, len(cols)-1)
	for _, c := range cols[1:] {
		setParts = append(setParts, fmt.Sprintf("%s = excluded.%s", c, c))
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(uuid) DO UPDATE SET %s",
		table, strings.Join(cols, ", "), strings.Join(placeholders, ", "), strings.Join(setParts, ", "),
	)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx for %s: %w", table, err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("store: prepare upsert %s: %w", table, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, rec := range recs {
		args := make([]any, 0, len(cols))
		args = append(args, rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID)
		for _, name := range names {
			if v, ok := rec.Values[name]; ok {
				args = append(args, v)
			} else {
				args = append(args, nil)
			}
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return 0, fmt.Errorf("store: upsert %s uuid=%s: %w", table, rec.UUID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit upsert %s: %w", table, err)
	}
	return len(recs), nil
}

// RecordsSince は start_time >= sinceMs のレコードを昇順で返す。sinceMs=0 なら全件。
func (s *Store) RecordsSince(ctx context.Context, typeKey string, tc config.TypeConfig, sinceMs int64) ([]model.Record, error) {
	table := tableName(typeKey)
	names := columnNames(tc)

	cols := make([]string, 0, len(names)+5)
	cols = append(cols, "uuid", "start_time", "end_time", "zone_offset", "app_id")
	cols = append(cols, names...)

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE start_time >= ? ORDER BY start_time ASC",
		strings.Join(cols, ", "), table,
	)

	rows, err := s.db.QueryContext(ctx, query, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("store: query %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var recs []model.Record
	for rows.Next() {
		var uuid, appID string
		var startTime, endTime int64
		var zoneOffset int32
		values := make([]sql.NullFloat64, len(names))

		dest := make([]any, 0, len(names)+5)
		dest = append(dest, &uuid, &startTime, &endTime, &zoneOffset, &appID)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", table, err)
		}

		rec := model.Record{
			UUID:       uuid,
			StartTime:  startTime,
			EndTime:    endTime,
			ZoneOffset: zoneOffset,
			AppID:      appID,
			Values:     make(map[string]float64, len(names)),
		}
		for i, name := range names {
			if values[i].Valid {
				rec.Values[name] = values[i].Float64
			}
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query %s: %w", table, err)
	}
	return recs, nil
}

func valueExpr(name string) string {
	if name == durationValueName {
		return "(end_time - start_time) / 60000.0"
	}
	return name
}

type dailySelectCol struct {
	alias string
	expr  string
}

// DailyAggregates は tc.Daily に従って種別 typeKey のレコードを現地日ごとに集約する。
func (s *Store) DailyAggregates(ctx context.Context, typeKey string, tc config.TypeConfig) ([]model.DailyRow, error) {
	table := tableName(typeKey)
	valueNames := tc.ValueNames()

	var selects []dailySelectCol
	hasCount := false
	for _, fn := range tc.Daily {
		if fn == "count" {
			hasCount = true
			continue
		}
		sqlFunc := dailySQLFuncs[fn]
		for _, name := range valueNames {
			selects = append(selects, dailySelectCol{
				alias: name + "_" + fn,
				expr:  fmt.Sprintf("%s(%s)", sqlFunc, valueExpr(name)),
			})
		}
	}
	if hasCount {
		selects = append(selects, dailySelectCol{alias: "count", expr: "COUNT(*)"})
	}

	cols := make([]string, 0, len(selects)+1)
	cols = append(cols, "strftime('%Y-%m-%d', (start_time / 1000 + zone_offset), 'unixepoch') AS local_date")
	for _, sc := range selects {
		cols = append(cols, fmt.Sprintf("%s AS %s", sc.expr, sc.alias))
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s GROUP BY local_date ORDER BY local_date",
		strings.Join(cols, ", "), table,
	)

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: daily aggregates %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var result []model.DailyRow
	for rows.Next() {
		var date string
		vals := make([]sql.NullFloat64, len(selects))

		dest := make([]any, 0, len(selects)+1)
		dest = append(dest, &date)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan daily aggregates %s: %w", table, err)
		}

		row := model.DailyRow{Date: date, Values: make(map[string]float64, len(selects))}
		for i, sc := range selects {
			if vals[i].Valid {
				row.Values[sc.alias] = vals[i].Float64
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: daily aggregates %s: %w", table, err)
	}
	return result, nil
}

// TypeStats は種別 typeKey の件数と最新 start_time を返す。
func (s *Store) TypeStats(ctx context.Context, typeKey string) (model.TypeStats, error) {
	table := tableName(typeKey)
	query := fmt.Sprintf("SELECT COUNT(*), COALESCE(MAX(start_time), 0) FROM %s", table)

	var stats model.TypeStats
	if err := s.db.QueryRowContext(ctx, query).Scan(&stats.Count, &stats.LatestStartTime); err != nil {
		return model.TypeStats{}, fmt.Errorf("store: type stats %s: %w", table, err)
	}
	return stats, nil
}

// GetState は state テーブルから key の値を返す。未設定なら ("", nil)。
func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get state %s: %w", key, err)
	}
	return value, nil
}

// SetState は state テーブルへ key/value を保存する（既存なら上書き）。
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("store: set state %s: %w", key, err)
	}
	return nil
}
