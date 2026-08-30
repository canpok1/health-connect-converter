package hcreader

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

// --- フィクスチャ ---

const (
	parentUUIDHex = "0123456789abcdef0123456789abcdef"
	otherUUIDHex  = "fedcba9876543210fedcba9876543210"
)

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// newFixtureDB は instant/interval/series の各1テーブルと application_info_table を
// 持つ、エクスポートDB相当のSQLiteファイルを作る。
func newFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()

	ddl := []string{
		`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`,
		`CREATE TABLE hc_instant (
			uuid BLOB, time INTEGER, zone_offset INTEGER, app_info_id INTEGER,
			value_a REAL, value_b REAL
		)`,
		`CREATE TABLE hc_interval (
			uuid BLOB, start_time INTEGER, start_zone_offset INTEGER,
			end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER, count INTEGER
		)`,
		`CREATE TABLE hc_series_parent (
			uuid BLOB, start_time INTEGER, start_zone_offset INTEGER,
			end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER,
			row_id INTEGER PRIMARY KEY
		)`,
		`CREATE TABLE hc_series_child (
			parent_key INTEGER, epoch_millis INTEGER, beats_per_minute INTEGER
		)`,
		// エクスポートDBが series の親テーブルの一部を CamelCase で命名すること
		// （例: SpeedRecordTable）を再現するフィクスチャ。
		`CREATE TABLE CamelSeriesParent (
			uuid BLOB, start_time INTEGER, start_zone_offset INTEGER,
			end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER,
			row_id INTEGER PRIMARY KEY
		)`,
		`CREATE TABLE camel_series_child (
			parent_key INTEGER, epoch_millis INTEGER, speed REAL
		)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec ddl %q: %v", stmt, err)
		}
	}

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}

	exec(`INSERT INTO application_info_table (row_id, package_name) VALUES (1, 'com.example.app')`)

	// instant: app_info_id あり・値NULL無しの行と、app_info_id NULL・value_b NULLの行。
	exec(`INSERT INTO hc_instant (uuid, time, zone_offset, app_info_id, value_a, value_b)
		VALUES (?, 1000, 32400, 1, 42, 64000)`, mustDecodeHex(t, parentUUIDHex))
	exec(`INSERT INTO hc_instant (uuid, time, zone_offset, app_info_id, value_a, value_b)
		VALUES (?, 2000, 32400, NULL, 7, NULL)`, mustDecodeHex(t, otherUUIDHex))

	// interval: end_zone_offset はレコードに使われないことを end_zone_offset != start_zone_offset で確認する。
	exec(`INSERT INTO hc_interval (uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id, count)
		VALUES (?, 1000, 32400, 2000, 99999, 1, 5)`, mustDecodeHex(t, parentUUIDHex))

	// series: 親1件、子3件。
	exec(`INSERT INTO hc_series_parent (uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id, row_id)
		VALUES (?, 1000, 32400, 4000, 32400, 1, 1)`, mustDecodeHex(t, parentUUIDHex))
	exec(`INSERT INTO hc_series_child (parent_key, epoch_millis, beats_per_minute) VALUES (1, 1000, 60)`)
	exec(`INSERT INTO hc_series_child (parent_key, epoch_millis, beats_per_minute) VALUES (1, 2000, 65)`)
	exec(`INSERT INTO hc_series_child (parent_key, epoch_millis, beats_per_minute) VALUES (1, 3000, 70)`)

	exec(`INSERT INTO CamelSeriesParent (uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id, row_id)
		VALUES (?, 1000, 32400, 4000, 32400, 1, 1)`, mustDecodeHex(t, parentUUIDHex))
	exec(`INSERT INTO camel_series_child (parent_key, epoch_millis, speed) VALUES (1, 1000, 1.5)`)

	return path
}

func instantTypeConfig() config.TypeConfig {
	return config.TypeConfig{
		SourceTable: "hc_instant",
		TimeLayout:  config.LayoutInstant,
		Columns: map[string]config.ColumnConfig{
			"val_a": {Column: "value_a", Scale: 1},
			"val_b": {Column: "value_b", Scale: 0.001},
		},
	}
}

func intervalTypeConfig() config.TypeConfig {
	return config.TypeConfig{
		SourceTable: "hc_interval",
		TimeLayout:  config.LayoutInterval,
		Columns: map[string]config.ColumnConfig{
			"count": {Column: "count", Scale: 1},
		},
	}
}

func seriesTypeConfig() config.TypeConfig {
	return config.TypeConfig{
		SourceTable: "hc_series_parent",
		TimeLayout:  config.LayoutSeries,
		SeriesTable: "hc_series_child",
		Columns: map[string]config.ColumnConfig{
			"bpm": {Column: "beats_per_minute", Scale: 1},
		},
	}
}

func camelSeriesTypeConfig() config.TypeConfig {
	return config.TypeConfig{
		SourceTable: "CamelSeriesParent",
		TimeLayout:  config.LayoutSeries,
		SeriesTable: "camel_series_child",
		Columns: map[string]config.ColumnConfig{
			"speed": {Column: "speed", Scale: 1},
		},
	}
}

// --- ReadDB: instant ---

func TestReadDB_Instant(t *testing.T) {
	dbPath := newFixtureDB(t)
	cfg := &config.Config{Types: map[string]config.TypeConfig{"instant_type": instantTypeConfig()}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}

	recs := got["instant_type"]
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}

	byUUID := make(map[string]int)
	for i, r := range recs {
		byUUID[r.UUID] = i
	}

	r0 := recs[byUUID[parentUUIDHex]]
	if r0.UUID != parentUUIDHex {
		t.Errorf("uuid = %q, want %q", r0.UUID, parentUUIDHex)
	}
	if r0.StartTime != 1000 || r0.EndTime != 1000 {
		t.Errorf("StartTime/EndTime = %d/%d, want 1000/1000", r0.StartTime, r0.EndTime)
	}
	if r0.ZoneOffset != 32400 {
		t.Errorf("ZoneOffset = %d, want 32400", r0.ZoneOffset)
	}
	if r0.AppID != "com.example.app" {
		t.Errorf("AppID = %q, want com.example.app", r0.AppID)
	}
	if got, want := r0.Values["val_a"], 42.0; got != want {
		t.Errorf("val_a = %v, want %v", got, want)
	}
	if got, want := r0.Values["val_b"], 64.0; got != want {
		t.Errorf("val_b (scaled) = %v, want %v", got, want)
	}

	r1 := recs[byUUID[otherUUIDHex]]
	if r1.AppID != "" {
		t.Errorf("AppID for NULL app_info_id = %q, want empty", r1.AppID)
	}
	if _, ok := r1.Values["val_b"]; ok {
		t.Errorf("val_b should be absent for NULL column, got %v", r1.Values["val_b"])
	}
}

func TestReadDB_Instant_UUIDIsLowerHex32(t *testing.T) {
	dbPath := newFixtureDB(t)
	cfg := &config.Config{Types: map[string]config.TypeConfig{"instant_type": instantTypeConfig()}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	for _, r := range got["instant_type"] {
		if len(r.UUID) != 32 {
			t.Errorf("uuid length = %d, want 32 (uuid=%q)", len(r.UUID), r.UUID)
		}
		for _, c := range r.UUID {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Errorf("uuid %q contains non lower-hex char %q", r.UUID, c)
			}
		}
	}
}

// --- ReadDB: interval ---

func TestReadDB_Interval(t *testing.T) {
	dbPath := newFixtureDB(t)
	cfg := &config.Config{Types: map[string]config.TypeConfig{"interval_type": intervalTypeConfig()}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}

	recs := got["interval_type"]
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.StartTime != 1000 {
		t.Errorf("StartTime = %d, want 1000", r.StartTime)
	}
	if r.EndTime != 2000 {
		t.Errorf("EndTime = %d, want 2000", r.EndTime)
	}
	if r.ZoneOffset != 32400 {
		t.Errorf("ZoneOffset = %d, want 32400 (start_zone_offset, not end_zone_offset)", r.ZoneOffset)
	}
	if got, want := r.Values["count"], 5.0; got != want {
		t.Errorf("count = %v, want %v", got, want)
	}
}

// --- ReadDB: series ---

func TestReadDB_Series(t *testing.T) {
	dbPath := newFixtureDB(t)
	cfg := &config.Config{Types: map[string]config.TypeConfig{"series_type": seriesTypeConfig()}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}

	recs := got["series_type"]
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}

	wantUUIDs := map[string]struct {
		epochMillis int64
		bpm         float64
	}{
		parentUUIDHex + "#1000": {1000, 60},
		parentUUIDHex + "#2000": {2000, 65},
		parentUUIDHex + "#3000": {3000, 70},
	}

	seen := make(map[string]bool)
	for _, r := range recs {
		if seen[r.UUID] {
			t.Errorf("duplicate uuid %q", r.UUID)
		}
		seen[r.UUID] = true

		want, ok := wantUUIDs[r.UUID]
		if !ok {
			t.Fatalf("unexpected uuid %q", r.UUID)
		}
		if r.StartTime != want.epochMillis || r.EndTime != want.epochMillis {
			t.Errorf("uuid %q: StartTime/EndTime = %d/%d, want %d/%d", r.UUID, r.StartTime, r.EndTime, want.epochMillis, want.epochMillis)
		}
		if r.ZoneOffset != 32400 {
			t.Errorf("uuid %q: ZoneOffset = %d, want 32400", r.UUID, r.ZoneOffset)
		}
		if r.AppID != "com.example.app" {
			t.Errorf("uuid %q: AppID = %q, want com.example.app", r.UUID, r.AppID)
		}
		if got := r.Values["bpm"]; got != want.bpm {
			t.Errorf("uuid %q: bpm = %v, want %v", r.UUID, got, want.bpm)
		}
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct uuids, got %d", len(seen))
	}
}

// --- 存在しないテーブル ---

func TestReadDB_MissingSourceTable_ReturnsEmptySliceNoError(t *testing.T) {
	dbPath := newFixtureDB(t)
	tc := instantTypeConfig()
	tc.SourceTable = "table_does_not_exist"
	cfg := &config.Config{Types: map[string]config.TypeConfig{"missing_type": tc}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	recs, ok := got["missing_type"]
	if !ok {
		t.Fatalf("expected key %q to be present", "missing_type")
	}
	if len(recs) != 0 {
		t.Errorf("expected empty slice, got %d records", len(recs))
	}
}

func TestReadDB_MissingSeriesTable_ReturnsEmptySliceNoError(t *testing.T) {
	dbPath := newFixtureDB(t)
	tc := seriesTypeConfig()
	tc.SeriesTable = "series_table_does_not_exist"
	cfg := &config.Config{Types: map[string]config.TypeConfig{"missing_series": tc}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	recs, ok := got["missing_series"]
	if !ok {
		t.Fatalf("expected key %q to be present", "missing_series")
	}
	if len(recs) != 0 {
		t.Errorf("expected empty slice, got %d records", len(recs))
	}
}

// --- 識別子検証（SQLインジェクション対策） ---

func TestReadDB_Series_CamelCaseSourceTable(t *testing.T) {
	dbPath := newFixtureDB(t)
	cfg := &config.Config{Types: map[string]config.TypeConfig{"camel_series_type": camelSeriesTypeConfig()}}

	got, err := ReadDB(dbPath, cfg)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	recs, ok := got["camel_series_type"]
	if !ok || len(recs) != 1 {
		t.Fatalf("expected 1 record for camel_series_type, got %+v", got["camel_series_type"])
	}
	if recs[0].Values["speed"] != 1.5 {
		t.Errorf("speed = %v, want 1.5", recs[0].Values["speed"])
	}
}

func TestReadDB_InvalidSourceTable_Errors(t *testing.T) {
	dbPath := newFixtureDB(t)
	tc := instantTypeConfig()
	tc.SourceTable = "hc_instant; DROP TABLE hc_instant;--"
	cfg := &config.Config{Types: map[string]config.TypeConfig{"evil": tc}}

	if _, err := ReadDB(dbPath, cfg); err == nil {
		t.Fatalf("expected error for invalid source_table, got nil")
	}

	// テーブルが実際に残っていることも確認する。
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM hc_instant").Scan(&count); err != nil {
		t.Fatalf("hc_instant should still exist: %v", err)
	}
}

// --- ExtractDB ---

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

func TestReader_Read_CleansUpTempFile(t *testing.T) {
	dbPath := newFixtureDB(t)
	dbData, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read fixture db: %v", err)
	}
	zipData := buildZip(t, map[string][]byte{"export.db": dbData})

	tempDir := t.TempDir()
	r := &Reader{TempDir: tempDir}
	cfg := &config.Config{Types: map[string]config.TypeConfig{"instant_type": instantTypeConfig()}}

	got, err := r.Read(&model.ZipFile{Data: zipData}, cfg)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got["instant_type"]) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got["instant_type"]))
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected TempDir to be empty after Read, found %v", entries)
	}
}

func TestReader_Read_CleansUpTempFileOnError(t *testing.T) {
	zipData := buildZip(t, map[string][]byte{"readme.txt": []byte("no db here")})

	tempDir := t.TempDir()
	r := &Reader{TempDir: tempDir}
	cfg := &config.Config{Types: map[string]config.TypeConfig{"instant_type": instantTypeConfig()}}

	if _, err := r.Read(&model.ZipFile{Data: zipData}, cfg); err == nil {
		t.Fatalf("expected error for zip with no .db entry")
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected TempDir to be empty after failed Read, found %v", entries)
	}
}
