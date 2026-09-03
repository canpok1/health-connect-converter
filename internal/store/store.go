// Package store は Health Connect のエクスポートデータを累積保存する SQLite ストアを提供する。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

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

// DailyAggregates は tc.Daily に従って種別 typeKey のレコードを現地日ごとに集約する。
// tc.Dedupe が真なら、集約の前にアプリ優先度による重複排除を行う。
func (s *Store) DailyAggregates(ctx context.Context, typeKey string, tc config.TypeConfig) ([]model.DailyRow, error) {
	recs, err := s.RecordsSince(ctx, typeKey, tc, 0)
	if err != nil {
		return nil, err
	}

	if tc.Dedupe {
		categoryID, ok := tc.CategoryID()
		if !ok {
			return nil, fmt.Errorf("store: daily aggregates %s: dedupe requires a known category", typeKey)
		}
		prios, err := s.AppPriorities(ctx)
		if err != nil {
			return nil, err
		}
		return aggregateDaily(dedupeByPriority(recs, prios[categoryID]), tc), nil
	}

	return aggregateDaily(unweighted(recs), tc), nil
}

// localDate はレコードを数える現地日を返す。
func localDate(rec model.Record, basis string) string {
	t := rec.StartTime
	if basis == config.DateBasisEnd {
		t = rec.EndTime
	}
	return time.Unix(t/1000+int64(rec.ZoneOffset), 0).UTC().Format("2006-01-02")
}

// weighted は重複排除の結果、レコードのうち採用する割合を持つ。時間帯の一部だけが
// 優先度の高いアプリと重なるとき、重なっていない割合ぶんだけを数える。
type weighted struct {
	rec   model.Record
	ratio float64
}

func unweighted(recs []model.Record) []weighted {
	out := make([]weighted, len(recs))
	for i, rec := range recs {
		out[i] = weighted{rec: rec, ratio: 1}
	}
	return out
}

func recordValue(rec model.Record, name string) (float64, bool) {
	if name == durationValueName {
		return float64(rec.EndTime-rec.StartTime) / 60000.0, true
	}
	v, ok := rec.Values[name]
	return v, ok
}

type dailyAccum struct {
	// sum は重複を除いた割合ぶんの合計、rawSum は割合を掛けない合計（平均に使う）。
	sum    float64
	rawSum float64
	min    float64
	max    float64
	count  int
}

func aggregateDaily(recs []weighted, tc config.TypeConfig) []model.DailyRow {
	valueNames := tc.ValueNames()
	funcs := make(map[string]bool, len(tc.Daily))
	for _, fn := range tc.Daily {
		funcs[fn] = true
	}

	// 日 -> 値名 -> 集計中の値
	byDate := make(map[string]map[string]*dailyAccum)
	recordCount := make(map[string]int)

	for _, w := range recs {
		date := localDate(w.rec, tc.DateBasis)
		recordCount[date]++

		accs, ok := byDate[date]
		if !ok {
			accs = make(map[string]*dailyAccum, len(valueNames))
			byDate[date] = accs
		}
		for _, name := range valueNames {
			v, ok := recordValue(w.rec, name)
			if !ok {
				continue
			}
			acc, ok := accs[name]
			if !ok {
				acc = &dailyAccum{min: math.Inf(1), max: math.Inf(-1)}
				accs[name] = acc
			}
			// 合計だけは重複を除いた割合ぶんにする。平均・最小・最大は
			// 1レコードの測定値そのものを見るものなので割合を掛けない。
			acc.sum += v * w.ratio
			acc.rawSum += v
			acc.count++
			acc.min = math.Min(acc.min, v)
			acc.max = math.Max(acc.max, v)
		}
	}

	dates := make([]string, 0, len(byDate))
	for date := range recordCount {
		dates = append(dates, date)
	}
	sort.Strings(dates)

	rows := make([]model.DailyRow, 0, len(dates))
	for _, date := range dates {
		row := model.DailyRow{Date: date, Values: make(map[string]float64)}
		for name, acc := range byDate[date] {
			if acc.count == 0 {
				continue
			}
			if funcs["sum"] {
				row.Values[name+"_sum"] = acc.sum
			}
			if funcs["mean"] {
				row.Values[name+"_mean"] = acc.rawSum / float64(acc.count)
			}
			if funcs["min"] {
				row.Values[name+"_min"] = acc.min
			}
			if funcs["max"] {
				row.Values[name+"_max"] = acc.max
			}
		}
		if funcs["count"] {
			row.Values["count"] = float64(recordCount[date])
		}
		rows = append(rows, row)
	}
	return rows
}

type interval struct {
	start int64
	end   int64
}

// dedupeByPriority は、優先度の高いアプリが既に覆っている時間帯のレコードを落とす。
// 複数のアプリが同じ実測（歩数・消費カロリーなど）を書いていると単純な合計が
// 多重計上になるため、Health Connect 自身の集計と同じく優先度で1つに絞る。
// order が空（優先度が読めていない）なら何もしない。
func dedupeByPriority(recs []model.Record, order []string) []weighted {
	if len(order) == 0 || len(recs) == 0 {
		return unweighted(recs)
	}

	rank := make(map[string]int, len(order))
	for i, app := range order {
		rank[app] = i
	}
	rankOf := func(appID string) int {
		if r, ok := rank[appID]; ok {
			return r
		}
		// 優先度に載っていないアプリは最下位。順序を決めきれないと結果が
		// 実行ごとに変わるため、アプリIDで安定させる。
		return len(order)
	}

	byApp := make(map[string][]model.Record)
	for _, rec := range recs {
		byApp[rec.AppID] = append(byApp[rec.AppID], rec)
	}
	apps := make([]string, 0, len(byApp))
	for app := range byApp {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool {
		if ri, rj := rankOf(apps[i]), rankOf(apps[j]); ri != rj {
			return ri < rj
		}
		return apps[i] < apps[j]
	})

	var covered []interval
	var kept []weighted
	for _, app := range apps {
		for _, rec := range byApp[app] {
			ratio := uncoveredRatio(covered, rec.StartTime, rec.EndTime)
			if ratio == 0 {
				continue
			}
			kept = append(kept, weighted{rec: rec, ratio: ratio})
		}
		// このアプリが「その日に記録していた範囲」を覆ったものとして扱う。
		// 歩数のように歩いた区間しかレコードが無い種別では、レコード単位で
		// 覆うと隙間が空き、そこへ下位アプリの同じ実測が入り込んで多重計上が残る。
		covered = mergeIntervals(append(covered, dailySpans(byApp[app])...))
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].rec.StartTime < kept[j].rec.StartTime })
	return kept
}

// dailySpans はアプリのレコードを現地日ごとにまとめ、その日の最初から最後までを
// 1つの区間として返す。
func dailySpans(recs []model.Record) []interval {
	spans := make(map[string]interval, len(recs))
	for _, rec := range recs {
		date := localDate(rec, config.DateBasisStart)
		span, ok := spans[date]
		if !ok {
			spans[date] = interval{start: rec.StartTime, end: rec.EndTime}
			continue
		}
		span.start = min(span.start, rec.StartTime)
		span.end = max(span.end, rec.EndTime)
		spans[date] = span
	}

	out := make([]interval, 0, len(spans))
	for _, span := range spans {
		out = append(out, span)
	}
	return out
}

func mergeIntervals(ivs []interval) []interval {
	if len(ivs) <= 1 {
		return ivs
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start < ivs[j].start })
	merged := ivs[:1]
	for _, iv := range ivs[1:] {
		last := &merged[len(merged)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
			continue
		}
		merged = append(merged, iv)
	}
	return merged
}

// uncoveredRatio は [start, end) のうち covered に含まれない割合を返す。
// covered はマージ済みで start 昇順であること。瞬時値（start == end）は
// その時刻を含む区間があれば 0、無ければ 1 を返す。
func uncoveredRatio(covered []interval, start, end int64) float64 {
	if start > end {
		start, end = end, start
	}
	if start == end {
		if pointCovered(covered, start) {
			return 0
		}
		return 1
	}

	var overlap int64
	i := sort.Search(len(covered), func(i int) bool { return covered[i].end > start })
	for ; i < len(covered) && covered[i].start < end; i++ {
		overlap += min(covered[i].end, end) - max(covered[i].start, start)
	}

	total := end - start
	if overlap >= total {
		return 0
	}
	return float64(total-overlap) / float64(total)
}

func pointCovered(covered []interval, t int64) bool {
	i := sort.Search(len(covered), func(i int) bool { return covered[i].end > t })
	return i < len(covered) && covered[i].start <= t
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
