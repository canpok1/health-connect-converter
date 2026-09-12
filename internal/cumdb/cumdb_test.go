package cumdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// testTable は種別のコードが宣言するのと同じ形のテーブル宣言。
func testTable(values ...Column) Table {
	cols := append([]Column{
		{Name: "uuid", Type: "TEXT"},
		{Name: "start_time", Type: "INTEGER"},
		{Name: "end_time", Type: "INTEGER"},
		{Name: "zone_offset", Type: "INTEGER"},
		{Name: "app_id", Type: "TEXT"},
	}, values...)
	return Table{
		Name:       "record_test",
		Columns:    cols,
		TimeColumn: "start_time",
		DateExpr:   LocalDateExpr("start_time", "zone_offset"),
	}
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cum.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

const zoneJST = 32400

// jstDay は 2026-09-11 00:00 JST（UTC epoch ms）。
const jstDay = int64(1789052400000)

func TestMigrate_Idempotent(t *testing.T) {
	db := newDB(t)
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := tbl.Migrate(ctx, db); err != nil {
			t.Fatalf("Migrate %d回目: %v", i+1, err)
		}
	}
}

// TestMigrate_AddsColumnKeepsRows は、宣言に列が増えたとき既存行を保ったまま
// 列を追加することを確認する。
func TestMigrate_AddsColumnKeepsRows(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	before := testTable(Column{Name: "v", Type: "REAL"})
	if err := before.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := before.Replace(ctx, db, []string{"2026-09-11"},
		[][]any{{"u1", jstDay, jstDay, zoneJST, "app", 1.5}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	after := testTable(Column{Name: "v", Type: "REAL"}, Column{Name: "w", Type: "REAL"})
	if err := after.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate（列追加）: %v", err)
	}

	rows, err := after.Query(ctx, db, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var n int
	for rows.Next() {
		var uuid, appID string
		var start, end int64
		var zone int32
		var v, w sql.NullFloat64
		if err := rows.Scan(&uuid, &start, &end, &zone, &appID, &v, &w); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		n++
		if uuid != "u1" || v.Float64 != 1.5 || w.Valid {
			t.Errorf("既存行が壊れている: uuid=%s v=%v w=%v", uuid, v, w)
		}
	}
	if n != 1 {
		t.Errorf("件数 = %d, want 1", n)
	}
}

func TestReplace_UpsertsByKey(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"},
		[][]any{{"u1", jstDay, jstDay, zoneJST, "app", 1.0}}); err != nil {
		t.Fatalf("Replace 1回目: %v", err)
	}
	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"},
		[][]any{{"u1", jstDay, jstDay, zoneJST, "app", 2.0}}); err != nil {
		t.Fatalf("Replace 2回目: %v", err)
	}

	var count int
	var v float64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*), MAX(v) FROM record_test").Scan(&count, &v); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 || v != 2.0 {
		t.Errorf("count=%d v=%v, want 1, 2.0（同じキーは上書き）", count, v)
	}
}

// TestReplace_DropsStaleRowsOfSameDate は、指定した現地日の既存行を消してから
// 入れることを確認する。端末側で作り直されたレコードの古い版が残ると合計が
// 膨らむため（ADR 0008）。
func TestReplace_DropsStaleRowsOfSameDate(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// 9/11 の古い版（2件）と 9/10 のレコード（残るはず）。
	if _, err := tbl.Replace(ctx, db, []string{"2026-09-10", "2026-09-11"}, [][]any{
		{"old1", jstDay, jstDay, zoneJST, "app", 1.0},
		{"old2", jstDay + 1000, jstDay + 1000, zoneJST, "app", 2.0},
		{"keep", jstDay - 86400000, jstDay - 86400000, zoneJST, "app", 3.0},
	}); err != nil {
		t.Fatalf("Replace 1回目: %v", err)
	}

	// 9/11 を1件で置き換える。
	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"},
		[][]any{{"new1", jstDay + 2000, jstDay + 2000, zoneJST, "app", 9.0}}); err != nil {
		t.Fatalf("Replace 2回目: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT uuid FROM record_test ORDER BY uuid")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, uuid)
	}
	want := []string{"keep", "new1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("残った行 = %v, want %v", got, want)
	}
}

func TestQuery_OrdersByTimeAndRespectsSince(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"}, [][]any{
		{"c", jstDay + 3000, jstDay + 3000, zoneJST, "app", 3.0},
		{"a", jstDay + 1000, jstDay + 1000, zoneJST, "app", 1.0},
		{"b", jstDay + 2000, jstDay + 2000, zoneJST, "app", 2.0},
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// 境界は「以降」を含む。
	rows, err := tbl.Query(ctx, db, jstDay+2000)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var uuid, appID string
		var start, end int64
		var zone int32
		var v sql.NullFloat64
		if err := rows.Scan(&uuid, &start, &end, &zone, &appID, &v); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, uuid)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("取得 = %v, want [b c]（時刻の昇順）", got)
	}
}

func TestStats_EmptyAndPopulated(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	stats, err := tbl.Stats(ctx, db)
	if err != nil {
		t.Fatalf("Stats（空）: %v", err)
	}
	if stats.Count != 0 || stats.LatestStartTime != 0 {
		t.Errorf("空のとき = %+v, want ゼロ値", stats)
	}

	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"}, [][]any{
		{"a", jstDay + 1000, jstDay + 1000, zoneJST, "app", 1.0},
		{"b", jstDay + 5000, jstDay + 5000, zoneJST, "app", 2.0},
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	stats, err = tbl.Stats(ctx, db)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 2 || stats.LatestStartTime != jstDay+5000 {
		t.Errorf("stats = %+v, want count=2 latest=%d", stats, jstDay+5000)
	}
}

// TestTable_NoValueColumns は値列を持たない種別（睡眠・運動）でも動くことを確認する。
func TestTable_NoValueColumns(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable()
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := tbl.Replace(ctx, db, []string{"2026-09-11"},
		[][]any{{"u1", jstDay, jstDay + 1000, zoneJST, "app"}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	stats, err := tbl.Stats(ctx, db)
	if err != nil || stats.Count != 1 {
		t.Errorf("stats = %+v err = %v, want count=1", stats, err)
	}
}

func TestReplace_RejectsWrongValueCount(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})
	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	_, err := tbl.Replace(ctx, db, nil, [][]any{{"u1", jstDay}})
	if err == nil {
		t.Fatal("列数が合わない行を受け入れた")
	}
}

func TestValidate_RejectsBadDeclarations(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	cases := map[string]Table{
		"テーブル名が不正":   {Name: "record test", Columns: []Column{{Name: "uuid", Type: "TEXT"}}, TimeColumn: "uuid", DateExpr: "x"},
		"列名が不正":      {Name: "record_x", Columns: []Column{{Name: "UUID", Type: "TEXT"}}, TimeColumn: "uuid", DateExpr: "x"},
		"型が未対応":      {Name: "record_x", Columns: []Column{{Name: "uuid", Type: "BLOB"}}, TimeColumn: "uuid", DateExpr: "x"},
		"列が無い":       {Name: "record_x", TimeColumn: "uuid", DateExpr: "x"},
		"DateExprが空": {Name: "record_x", Columns: []Column{{Name: "uuid", Type: "TEXT"}}, TimeColumn: "uuid"},
	}
	for name, tbl := range cases {
		if err := tbl.Migrate(ctx, db); err == nil {
			t.Errorf("%s: エラーにならなかった", name)
		}
	}
}

func TestLocalDateExpr_PanicsOnBadColumn(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("不正な列名で panic しなかった")
		}
	}()
	_ = LocalDateExpr("start time", "zone_offset")
}

// TestMigrate_AddsTimeColumnBeforeIndex は、既存テーブルに無い列を TimeColumn と
// して宣言したときでも移行が通ることを確認する。索引を列追加より先に作ると
// 「まだ無い列」を参照して落ちる。
func TestMigrate_AddsTimeColumnBeforeIndex(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// 先に time_col を持たないテーブルを作る。
	before := Table{
		Name: "record_test",
		Columns: []Column{
			{Name: "uuid", Type: "TEXT"},
			{Name: "zone_offset", Type: "INTEGER"},
		},
		TimeColumn: "zone_offset",
		DateExpr:   LocalDateExpr("zone_offset", "zone_offset"),
	}
	if err := before.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate（前）: %v", err)
	}

	// 新しく time_col を足し、それを TimeColumn にする。
	after := Table{
		Name: "record_test",
		Columns: []Column{
			{Name: "uuid", Type: "TEXT"},
			{Name: "zone_offset", Type: "INTEGER"},
			{Name: "time_col", Type: "INTEGER"},
		},
		TimeColumn: "time_col",
		DateExpr:   LocalDateExpr("time_col", "zone_offset"),
	}
	if err := after.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate（後）: %v", err)
	}
}

// TestMigrate_DropsLegacyStartIndex は、設定ファイル駆動だった頃の索引
// （idx_<テーブル>_start）を落とすことを確認する。同じ列に2本あっても速く
// ならず、書き込みが遅くなるだけ。
func TestMigrate_DropsLegacyStartIndex(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tbl := testTable(Column{Name: "v", Type: "REAL"})

	// 旧実装が作っていた形を再現する。
	if _, err := db.ExecContext(ctx, `CREATE TABLE record_test (
		uuid TEXT PRIMARY KEY, start_time INTEGER, end_time INTEGER,
		zone_offset INTEGER, app_id TEXT, v REAL)`); err != nil {
		t.Fatalf("旧テーブル作成: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX idx_record_test_start ON record_test(start_time)`); err != nil {
		t.Fatalf("旧索引作成: %v", err)
	}

	if err := tbl.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	indexes := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'record_test'`)
	if err != nil {
		t.Fatalf("索引の一覧: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		indexes[name] = true
	}
	if indexes["idx_record_test_start"] {
		t.Error("旧索引 idx_record_test_start が残っている")
	}
	if !indexes["idx_record_test_start_time"] {
		t.Errorf("新しい索引が無い: %v", indexes)
	}
}
