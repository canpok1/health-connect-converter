package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// --- テスト用の種別 ---
//
// 本物の種別（internal/kind）は非公開なので、ここでは同じインターフェースを
// 満たす最小の種別を用意する。値列は v ひとつ。
//
// レコードは「開始から1分間」の期間として保存する。瞬間の記録（開始＝終了）だと
// 時間帯の重なりが生じず重複排除を試せないため。

type testKind struct {
	key    string
	policy kind.Policy
}

func (k testKind) Key() string         { return k.key }
func (k testKind) Policy() kind.Policy { return k.policy }

func (k testKind) Table() cumdb.Table {
	return cumdb.Table{
		Name: "record_" + k.key,
		Columns: []cumdb.Column{
			{Name: "uuid", Type: "TEXT"},
			{Name: "start_time", Type: "INTEGER"},
			{Name: "end_time", Type: "INTEGER"},
			{Name: "zone_offset", Type: "INTEGER"},
			{Name: "app_id", Type: "TEXT"},
			{Name: "v", Type: "REAL"},
		},
		TimeColumn: "start_time",
		DateExpr:   cumdb.LocalDateExpr("start_time", "zone_offset"),
	}
}

func (k testKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	rows, err := exportDB.QueryContext(ctx, "SELECT uuid, time, zone_offset, app_id, v FROM src ORDER BY time")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	var out [][]any
	var dates []string
	for rows.Next() {
		var uuid, appID string
		var tm int64
		var zone int32
		var v float64
		if err := rows.Scan(&uuid, &tm, &zone, &appID, &v); err != nil {
			return nil, nil, err
		}
		out = append(out, []any{uuid, tm, tm + testDurationMs, zone, appID, v})
		dates = append(dates, localDateForTest(tm, zone))
	}
	return out, dates, rows.Err()
}

func (k testKind) records(ctx context.Context, cum *sql.DB, sinceMs int64) ([]model.AggRecord, error) {
	rows, err := k.Table().Query(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.AggRecord
	for rows.Next() {
		var uuid, appID string
		var start, end int64
		var zone int32
		var v sql.NullFloat64
		if err := rows.Scan(&uuid, &start, &end, &zone, &appID, &v); err != nil {
			return nil, err
		}
		values := map[string]float64{}
		if v.Valid {
			values["v"] = v.Float64
		}
		out = append(out, model.AggRecord{
			LocalDate: localDateForTest(start, zone), StartTime: start, EndTime: end,
			ZoneOffset: zone, AppID: appID, Values: values,
		})
	}
	return out, rows.Err()
}

func (k testKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	return k.records(ctx, cum, 0)
}

func (k testKind) ValueNames() []string { return []string{"v"} }
func (k testKind) RawHeader() []any     { return []any{"local_date", "v"} }

func (k testKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.records(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, []any{rec.LocalDate, rec.Values["v"]})
	}
	return out, nil
}

func localDateForTest(ms int64, zoneOffset int32) string {
	return time.Unix(ms/1000+int64(zoneOffset), 0).UTC().Format("2006-01-02")
}

// --- ヘルパー ---

const zoneJST = 32400

// testDurationMs はテスト用の種別が1レコードに与える長さ（1分）。
const testDurationMs = int64(60 * 1000)

// jstNoon は 2026-09-11 12:00 JST（UTC epoch ms）。
const jstNoon = int64(1789095600000)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sub", "cum.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newExportDB はテスト用の種別が読むエクスポートDBを作る。
func newExportDB(t *testing.T, rows [][]any) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open export db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE src (uuid TEXT, time INTEGER, zone_offset INTEGER, app_id TEXT, v REAL)`); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO src (uuid, time, zone_offset, app_id, v) VALUES (?, ?, ?, ?, ?)`, r...); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return db
}

// --- テスト ---

func TestIngestAndDailyAggregates(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	k := testKind{key: "t1", policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum, kind.FuncCount}}}

	if err := st.Migrate(ctx, []kind.Kind{k}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	exportDB := newExportDB(t, [][]any{
		{"u1", jstNoon, zoneJST, "app", 10.0},
		{"u2", jstNoon + 1000, zoneJST, "app", 20.0},
	})
	n, err := st.Ingest(ctx, k, exportDB)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if n != 2 {
		t.Errorf("取り込み件数 = %d, want 2", n)
	}

	rows, err := st.DailyAggregates(ctx, k)
	if err != nil {
		t.Fatalf("DailyAggregates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("日数 = %d, want 1（%+v）", len(rows), rows)
	}
	if rows[0].Values["v_sum"] != 30 || rows[0].Values["count"] != 2 {
		t.Errorf("集計 = %+v, want v_sum=30 count=2", rows[0].Values)
	}
}

// TestIngestTwiceIsIdempotent は同じエクスポートを2回取り込んでも件数が増えない
// ことを確認する（取り込みは毎回同じZIPを読み直す可能性がある）。
func TestIngestTwiceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	k := testKind{key: "t1", policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}}
	if err := st.Migrate(ctx, []kind.Kind{k}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	exportDB := newExportDB(t, [][]any{{"u1", jstNoon, zoneJST, "app", 10.0}})

	for i := 0; i < 2; i++ {
		if _, err := st.Ingest(ctx, k, exportDB); err != nil {
			t.Fatalf("Ingest %d回目: %v", i+1, err)
		}
	}

	stats, err := st.Stats(ctx, k)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 1 {
		t.Errorf("件数 = %d, want 1", stats.Count)
	}
}

func TestRawRowsRespectsSince(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	k := testKind{key: "t1", policy: kind.Policy{Window: "1d", Daily: []string{kind.FuncSum}}}
	if err := st.Migrate(ctx, []kind.Kind{k}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	exportDB := newExportDB(t, [][]any{
		{"old", jstNoon - 5*86400000, zoneJST, "app", 1.0},
		{"new", jstNoon, zoneJST, "app", 2.0},
	})
	if _, err := st.Ingest(ctx, k, exportDB); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	rows, err := st.RawRows(ctx, k, jstNoon-86400000)
	if err != nil {
		t.Fatalf("RawRows: %v", err)
	}
	if len(rows) != 1 || rows[0][1] != 2.0 {
		t.Errorf("rows = %+v, want 直近の1件だけ", rows)
	}
}

// TestDailyAggregatesUsesSavedPriorities は、保存済みのアプリ優先度が重複排除に
// 使われることを確認する。
func TestDailyAggregatesUsesSavedPriorities(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	k := testKind{key: "t1", policy: kind.Policy{
		Window: kind.WindowAll, Daily: []string{kind.FuncSum},
		Dedupe: true, Category: kind.CategoryActivity,
	}}
	if err := st.Migrate(ctx, []kind.Kind{k}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := st.SetAppPriorities(ctx, model.AppPriorities{kind.CategoryActivity: {"top", "low"}}); err != nil {
		t.Fatalf("SetAppPriorities: %v", err)
	}

	// 同じ時刻を2アプリが記録している。優先度の高い方だけを数えるはず。
	exportDB := newExportDB(t, [][]any{
		{"u1", jstNoon, zoneJST, "top", 100.0},
		{"u2", jstNoon, zoneJST, "low", 100.0},
	})
	if _, err := st.Ingest(ctx, k, exportDB); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	rows, err := st.DailyAggregates(ctx, k)
	if err != nil {
		t.Fatalf("DailyAggregates: %v", err)
	}
	if len(rows) != 1 || rows[0].Values["v_sum"] != 100 {
		t.Errorf("集計 = %+v, want v_sum=100", rows)
	}
}

func TestGetSetState(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got, err := st.GetState(ctx, "missing")
	if err != nil || got != "" {
		t.Errorf("未設定のキー = %q, %v, want 空", got, err)
	}

	if err := st.SetState(ctx, "k", "v1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := st.SetState(ctx, "k", "v2"); err != nil {
		t.Fatalf("SetState（上書き）: %v", err)
	}
	if got, err := st.GetState(ctx, "k"); err != nil || got != "v2" {
		t.Errorf("GetState = %q, %v, want v2", got, err)
	}
}

func TestAppPrioritiesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if got, err := st.AppPriorities(ctx); err != nil || len(got) != 0 {
		t.Errorf("未保存のとき = %v, %v, want 空", got, err)
	}

	want := model.AppPriorities{
		kind.CategoryActivity: {"a", "b"},
		kind.CategorySleep:    {"c"},
	}
	if err := st.SetAppPriorities(ctx, want); err != nil {
		t.Fatalf("SetAppPriorities: %v", err)
	}
	got, err := st.AppPriorities(ctx)
	if err != nil {
		t.Fatalf("AppPriorities: %v", err)
	}
	if len(got) != 2 || got[kind.CategoryActivity][1] != "b" || got[kind.CategorySleep][0] != "c" {
		t.Errorf("priorities = %+v, want %+v", got, want)
	}

	// 空を渡しても既存を消さない（優先度を持たないエクスポートで重複排除が
	// 効かなくなるのを防ぐ）。
	if err := st.SetAppPriorities(ctx, model.AppPriorities{}); err != nil {
		t.Fatalf("SetAppPriorities（空）: %v", err)
	}
	if got, err := st.AppPriorities(ctx); err != nil || len(got) != 2 {
		t.Errorf("空を渡した後 = %+v, %v, want 2件のまま", got, err)
	}
}
