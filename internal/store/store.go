// Package store は Health Connect のエクスポートデータを累積保存する SQLite ストアを提供する。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/agg"
	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

const durationValueName = "duration_min"

// stateKeyAppPriorities は Health Connect の「アプリの優先度」を保存する state のキー。
const stateKeyAppPriorities = "app_priorities"

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

// localDateExpr はレコードの現地日を返す SQL 式。日次集約の Go 側の計算
// （localDate）と同じ規則にすること。
const localDateExpr = "strftime('%Y-%m-%d', (start_time / 1000 + zone_offset), 'unixepoch')"

// deleteDatesChunk は SQLite のプレースホルダ上限に収めるための1回ぶんの件数。
const deleteDatesChunk = 500

func deleteDates(ctx context.Context, tx *sql.Tx, table string, dates []string) error {
	for start := 0; start < len(dates); start += deleteDatesChunk {
		end := min(start+deleteDatesChunk, len(dates))
		chunk := dates[start:end]

		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		query := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", table, localDateExpr, placeholders)

		args := make([]any, len(chunk))
		for i, date := range chunk {
			args[i] = date
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: delete %s for %d dates: %w", table, len(chunk), err)
		}
	}
	return nil
}

// ReplaceRecords は recs に含まれる現地日ぶんの既存レコードを削除してから recs を
// 入れる。UUID をキーに追加するだけだと、端末側で作り直されたレコード（Health
// Connect は細かいレコードを後から日次合計へ書き換える）の古い版が累積DBに残り続け、
// 合計が膨らむ。エクスポートはその日の最新状態なので、日ごと置き換える。
// エクスポートに現れない日は端末から消えていても残す（累積DBの目的）。
func (s *Store) ReplaceRecords(ctx context.Context, typeKey string, tc config.TypeConfig, recs []model.Record) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}

	dateSet := make(map[string]bool)
	for _, rec := range recs {
		dateSet[localDate(rec, config.DateBasisStart)] = true
	}
	dates := make([]string, 0, len(dateSet))
	for date := range dateSet {
		dates = append(dates, date)
	}
	sort.Strings(dates)

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

	if err := deleteDates(ctx, tx, table, dates); err != nil {
		return 0, err
	}

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

// DailyAggregates は種別 typeKey のレコードを集計用モデルへ直し、agg に日次集計を
// させる。tc.Dedupe が真なら、集約の前にアプリ優先度による重複排除を行う。
func (s *Store) DailyAggregates(ctx context.Context, typeKey string, tc config.TypeConfig) ([]model.DailyRow, error) {
	recs, err := s.RecordsSince(ctx, typeKey, tc, 0)
	if err != nil {
		return nil, err
	}

	var order []string
	if tc.Dedupe {
		categoryID, ok := tc.CategoryID()
		if !ok {
			return nil, fmt.Errorf("store: daily aggregates %s: dedupe requires a known category", typeKey)
		}
		prios, err := s.AppPriorities(ctx)
		if err != nil {
			return nil, err
		}
		order = prios[categoryID]
	}

	return agg.Daily(toAggRecords(recs, tc), agg.Options{Daily: tc.Daily, Dedupe: tc.Dedupe}, order), nil
}

// toAggRecords は保存用のレコードを集計用モデルへ直す。どの日に数えるかの判断
// （date_basis）と、期間から求める値（duration_min）はここで済ませる。
func toAggRecords(recs []model.Record, tc config.TypeConfig) []model.AggRecord {
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := make(map[string]float64, len(rec.Values)+1)
		for name, v := range rec.Values {
			values[name] = v
		}
		if tc.IncludeDuration {
			values[durationValueName] = float64(rec.EndTime-rec.StartTime) / 60000.0
		}
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec, tc.DateBasis),
			StartTime:  rec.StartTime,
			EndTime:    rec.EndTime,
			ZoneOffset: rec.ZoneOffset,
			AppID:      rec.AppID,
			Values:     values,
		})
	}
	return out
}

// localDate はレコードを数える現地日を返す。
func localDate(rec model.Record, basis string) string {
	t := rec.StartTime
	if basis == config.DateBasisEnd {
		t = rec.EndTime
	}
	return time.Unix(t/1000+int64(rec.ZoneOffset), 0).UTC().Format("2006-01-02")
}

// SetAppPriorities は Health Connect のアプリ優先度を state へ保存する。
// 空のときは既存を消さない（優先度を持たないエクスポートを読んだだけで
// 重複排除が効かなくなるのを防ぐ）。
func (s *Store) SetAppPriorities(ctx context.Context, prios model.AppPriorities) error {
	if len(prios) == 0 {
		return nil
	}
	m := make(map[string][]string, len(prios))
	for category, apps := range prios {
		m[strconv.Itoa(category)] = apps
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode app priorities: %w", err)
	}
	return s.SetState(ctx, stateKeyAppPriorities, string(encoded))
}

// AppPriorities は保存済みのアプリ優先度を返す。未保存なら空。
func (s *Store) AppPriorities(ctx context.Context) (model.AppPriorities, error) {
	raw, err := s.GetState(ctx, stateKeyAppPriorities)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return model.AppPriorities{}, nil
	}
	var m map[string][]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("store: decode app priorities: %w", err)
	}
	prios := make(model.AppPriorities, len(m))
	for key, apps := range m {
		category, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("store: decode app priorities: invalid category %q: %w", key, err)
		}
		prios[category] = apps
	}
	return prios, nil
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
