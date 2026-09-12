package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// fakeKind は取り込みの呼び出しだけを見るテスト用の種別。
type fakeKind struct{ key string }

func (k fakeKind) Key() string { return k.key }
func (k fakeKind) Policy() kind.Policy {
	return kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}
}
func (k fakeKind) Table() cumdb.Table   { return cumdb.Table{} }
func (k fakeKind) ValueNames() []string { return []string{"v"} }
func (k fakeKind) RawHeader() []any     { return []any{"v"} }
func (k fakeKind) ExportRows(context.Context, *sql.DB) ([][]any, []string, error) {
	return nil, nil, nil
}
func (k fakeKind) Aggregate(context.Context, *sql.DB) ([]model.AggRecord, error) { return nil, nil }
func (k fakeKind) RawRows(context.Context, *sql.DB, int64) ([][]any, error)      { return nil, nil }

type fakeStore struct {
	prios      model.AppPriorities
	prioErr    error
	ingestErr  error
	ingestKeys []string
}

func (f *fakeStore) SetAppPriorities(_ context.Context, prios model.AppPriorities) error {
	f.prios = prios
	return f.prioErr
}

func (f *fakeStore) Ingest(_ context.Context, k kind.Kind, _ *sql.DB) (int, error) {
	f.ingestKeys = append(f.ingestKeys, k.Key())
	return 0, f.ingestErr
}

// buildZip はエクスポートZIPを模したZIPを作る。
func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// exportDBBytes は最小のエクスポートDB（アプリ一覧と優先度）のバイト列を作る。
func exportDBBytes(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`,
		`INSERT INTO application_info_table (row_id, package_name) VALUES (1, 'com.example.app')`,
		`CREATE TABLE health_data_category_priority_table (
			row_id INTEGER PRIMARY KEY, health_data_category INTEGER UNIQUE, app_id_priority_order TEXT NOT NULL)`,
		`INSERT INTO health_data_category_priority_table (health_data_category, app_id_priority_order) VALUES (1, '1')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIngest_ReadsPrioritiesAndAllKinds(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "tmp")
	st := &fakeStore{}
	ing := &Ingester{
		Kinds:   []kind.Kind{fakeKind{key: "a"}, fakeKind{key: "b"}},
		Store:   st,
		TempDir: tempDir,
		Logger:  discardLogger(),
	}

	zipData := buildZip(t, map[string][]byte{"export.db": exportDBBytes(t)})
	info, err := ing.Ingest(context.Background(), &model.ZipFile{Data: zipData})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if len(st.ingestKeys) != 2 || st.ingestKeys[0] != "a" || st.ingestKeys[1] != "b" {
		t.Errorf("取り込んだ種別 = %v, want [a b]（一覧の順）", st.ingestKeys)
	}
	if len(st.prios[1]) != 1 || st.prios[1][0] != "com.example.app" {
		t.Errorf("優先度 = %+v, want カテゴリ1にアプリ1件", st.prios)
	}
	if info.TableRows["application_info_table"] != 1 {
		t.Errorf("TableRows = %+v, want application_info_table=1", info.TableRows)
	}
}

// TestIngest_CleansUpTempFile は展開した一時ファイルを残さないことを確認する。
// 20MB超のDBを残し続けると data ディレクトリを食い潰す。
func TestIngest_CleansUpTempFile(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "tmp")
	ing := &Ingester{Store: &fakeStore{}, TempDir: tempDir, Logger: discardLogger()}

	zipData := buildZip(t, map[string][]byte{"export.db": exportDBBytes(t)})
	if _, err := ing.Ingest(context.Background(), &model.ZipFile{Data: zipData}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("一時ファイルが残っている: %d件", len(entries))
	}
}

func TestIngest_CleansUpTempFileOnError(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "tmp")
	ing := &Ingester{
		Kinds:   []kind.Kind{fakeKind{key: "a"}},
		Store:   &fakeStore{ingestErr: errors.New("boom")},
		TempDir: tempDir,
		Logger:  discardLogger(),
	}

	zipData := buildZip(t, map[string][]byte{"export.db": exportDBBytes(t)})
	if _, err := ing.Ingest(context.Background(), &model.ZipFile{Data: zipData}); err == nil {
		t.Fatal("Ingest() error = nil, want error")
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("失敗時に一時ファイルが残っている: %d件", len(entries))
	}
}

func TestIngest_NoDBEntryInZip(t *testing.T) {
	ing := &Ingester{Store: &fakeStore{}, TempDir: filepath.Join(t.TempDir(), "tmp"), Logger: discardLogger()}
	zipData := buildZip(t, map[string][]byte{"readme.txt": []byte("no db here")})
	if _, err := ing.Ingest(context.Background(), &model.ZipFile{Data: zipData}); err == nil {
		t.Fatal("Ingest() error = nil, want error")
	}
}
