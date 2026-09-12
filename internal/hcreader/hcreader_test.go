package hcreader

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// --- フィクスチャ ---

// mustOpen はエクスポートDBを開く（テスト終了時に閉じる）。
func mustOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDB_ExtractsDBEntry(t *testing.T) {
	want := []byte("sqlite-file-contents")
	zipData := buildZip(t, map[string][]byte{
		"export/health_connect_export.db": want,
		"export/readme.txt":               []byte("not a db"),
	})

	destPath := filepath.Join(t.TempDir(), "out.db")
	if err := ExtractDB(zipData, destPath); err != nil {
		t.Fatalf("ExtractDB: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted content = %q, want %q", got, want)
	}
}

func TestExtractDB_PicksLargestDBEntry(t *testing.T) {
	small := []byte("small")
	large := []byte("this-is-the-larger-db-file-contents")
	zipData := buildZip(t, map[string][]byte{
		"a.db": small,
		"b.db": large,
	})

	destPath := filepath.Join(t.TempDir(), "out.db")
	if err := ExtractDB(zipData, destPath); err != nil {
		t.Fatalf("ExtractDB: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, large) {
		t.Errorf("expected largest entry to be extracted, got %q", got)
	}
}

func TestExtractDB_NoDBEntry_Errors(t *testing.T) {
	zipData := buildZip(t, map[string][]byte{
		"readme.txt": []byte("no db here"),
	})

	destPath := filepath.Join(t.TempDir(), "out.db")
	if err := ExtractDB(zipData, destPath); err == nil {
		t.Fatalf("expected error when zip has no .db entry")
	}
}

// --- Reader.Read: 一時ファイルの後始末 ---

func newPriorityDB(t *testing.T, hasPriorityTable bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "priority.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}

	exec(`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`)
	exec(`INSERT INTO application_info_table (row_id, package_name) VALUES (1, 'com.example.fit'), (9, 'com.example.band')`)
	exec(`CREATE TABLE hc_instant (
		uuid BLOB, time INTEGER, zone_offset INTEGER, app_info_id INTEGER, value_a REAL
	)`)

	if hasPriorityTable {
		exec(`CREATE TABLE health_data_category_priority_table (
			row_id INTEGER PRIMARY KEY, health_data_category INTEGER, app_id_priority_order TEXT
		)`)
		// 4 は application_info_table に無いアプリ。順序だけ残っていても使えない。
		exec(`INSERT INTO health_data_category_priority_table (health_data_category, app_id_priority_order)
			VALUES (1, '4,9,1'), (5, '1')`)
	}

	return path
}

func TestReadAppPriorities(t *testing.T) {
	db := mustOpen(t, newPriorityDB(t, true))
	got, err := ReadAppPriorities(context.Background(), db)
	if err != nil {
		t.Fatalf("ReadAppPriorities: %v", err)
	}

	activity := got[1]
	want := []string{"com.example.band", "com.example.fit"}
	if len(activity) != len(want) {
		t.Fatalf("priorities[1] = %v, want %v", activity, want)
	}
	for i := range want {
		if activity[i] != want[i] {
			t.Fatalf("priorities[1] = %v, want %v", activity, want)
		}
	}
	if len(got[5]) != 1 || got[5][0] != "com.example.fit" {
		t.Errorf("priorities[5] = %v, want [com.example.fit]", got[5])
	}
}

func TestReadAppPriorities_WithoutPriorityTable(t *testing.T) {
	db := mustOpen(t, newPriorityDB(t, false))
	got, err := ReadAppPriorities(context.Background(), db)
	if err != nil {
		t.Fatalf("ReadAppPriorities: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("priorities = %v, want empty", got)
	}
}

// --- ReadTableRows ---

// TestReadTableRows_CountsAllTablesRegardlessOfKinds は、取り込み対象の種別に
// 関わらずDB内の全テーブルを数えることを確認する。未登録のテーブルへ書き込みが
// 始まったことに気づくのが目的なので、名前や登録の有無で絞らない。
func TestReadTableRows_CountsAllTablesRegardlessOfKinds(t *testing.T) {
	db := mustOpen(t, newPriorityDB(t, true))
	got, err := ReadTableRows(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("ReadTableRows: %v", err)
	}

	want := map[string]int64{
		"application_info_table":              2,
		"hc_instant":                          0,
		"health_data_category_priority_table": 2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TableRows mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestReadTableRows_IncludesEmptyTablesAndExcludesNonTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tables.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ddl := []string{
		// AUTOINCREMENT により sqlite_sequence が作られる。除外されるはず。
		`CREATE TABLE filled (row_id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER)`,
		`CREATE TABLE empty_table (v INTEGER)`,
		// 二重引用符を含むテーブル名でもクォートが壊れないことを確認する。
		`CREATE TABLE "we""ird" (v INTEGER)`,
		// LIKE の `_` をワイルドカードのまま使うと、これが sqlite_% に当たって
		// 落ちる。SQLite の内部テーブルではないので出るはず。
		`CREATE TABLE sqliteX_not_internal (v INTEGER)`,
		`CREATE INDEX idx_filled_v ON filled (v)`,
		`CREATE VIEW filled_view AS SELECT v FROM filled`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec ddl %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO filled (v) VALUES (1), (2)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO "we""ird" (v) VALUES (9)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	got, err := ReadTableRows(context.Background(), mustOpen(t, path), nil)
	if err != nil {
		t.Fatalf("ReadTableRows: %v", err)
	}

	want := map[string]int64{
		"filled":               2,
		"empty_table":          0,
		`we"ird`:               1,
		"sqliteX_not_internal": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TableRows mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestQuoteIdentifier(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", `"plain"`},
		{`we"ird`, `"we""ird"`},
		{`a"b"c`, `"a""b""c"`},
	}
	for _, c := range cases {
		if got := quoteIdentifier(c.in); got != c.want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadTableRows_SkipsFailingTableAndLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// sqlite_master へ直接書いて、COUNT が失敗するテーブル（存在しないモジュールの
	// 仮想テーブル）を作る。破損したテーブルが1つあっても取り込みを止めないこと。
	stmts := []string{
		`CREATE TABLE ok_table (v INTEGER)`,
		`INSERT INTO ok_table VALUES (1)`,
		`PRAGMA writable_schema=ON`,
		`INSERT INTO sqlite_master (type, name, tbl_name, rootpage, sql)
			VALUES ('table', 'ghost', 'ghost', 0, 'CREATE VIRTUAL TABLE ghost USING nosuchmodule(x)')`,
		`PRAGMA writable_schema=OFF`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got, err := ReadTableRows(context.Background(), mustOpen(t, path), logger)
	if err != nil {
		t.Fatalf("ReadTableRows: %v", err)
	}

	want := map[string]int64{"ok_table": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TableRows mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if !strings.Contains(logs.String(), "ghost") {
		t.Errorf("log does not mention the failing table: %q", logs.String())
	}
}
