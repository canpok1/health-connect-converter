package hcsql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

const (
	parentUUIDHex = "8eeed20eaffa302ba456713382e81048"
	otherUUIDHex  = "00112233445566778899aabbccddeeff"
)

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// newFixture はエクスポートDBを模した合成DBを作る。
func newFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}

	exec(`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`)
	exec(`INSERT INTO application_info_table (row_id, package_name) VALUES (1, 'com.example.app')`)

	exec(`CREATE TABLE hc_instant (
		uuid BLOB, time INTEGER, zone_offset INTEGER, app_info_id INTEGER, value_a REAL, value_b REAL)`)
	exec(`INSERT INTO hc_instant (uuid, time, zone_offset, app_info_id, value_a, value_b) VALUES (?, 1000, 32400, 1, 42, 64000)`,
		mustDecodeHex(t, parentUUIDHex))
	// app_info_id が NULL（アプリが消えている）・値が NULL の行。
	exec(`INSERT INTO hc_instant (uuid, time, zone_offset, app_info_id, value_a, value_b) VALUES (?, 2000, 32400, NULL, 7, NULL)`,
		mustDecodeHex(t, otherUUIDHex))

	exec(`CREATE TABLE hc_interval (
		uuid BLOB, start_time INTEGER, start_zone_offset INTEGER,
		end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER, count INTEGER)`)
	// end_zone_offset は使わないことを start と違う値にして確かめる。
	exec(`INSERT INTO hc_interval (uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id, count)
		VALUES (?, 1000, 32400, 2000, 99999, 1, 5)`, mustDecodeHex(t, parentUUIDHex))

	exec(`CREATE TABLE HcSeriesParent (
		row_id INTEGER PRIMARY KEY, uuid BLOB, start_time INTEGER, start_zone_offset INTEGER,
		end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER)`)
	exec(`CREATE TABLE hc_series_child (parent_key INTEGER, epoch_millis INTEGER, beats_per_minute INTEGER)`)
	exec(`INSERT INTO HcSeriesParent (row_id, uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id)
		VALUES (1, ?, 1000, 32400, 4000, 32400, 1)`, mustDecodeHex(t, parentUUIDHex))
	exec(`INSERT INTO hc_series_child (parent_key, epoch_millis, beats_per_minute) VALUES (1, 1000, 60), (1, 2000, 65)`)

	exec(`CREATE TABLE hc_segment_child (
		parent_key INTEGER, stage_start_time INTEGER, stage_end_time INTEGER, stage_type INTEGER)`)
	exec(`INSERT INTO hc_segment_child (parent_key, stage_start_time, stage_end_time, stage_type)
		VALUES (1, 1000, 2000, 4), (1, 2000, 3000, 5)`)

	return db
}

func TestReadInstant(t *testing.T) {
	db := newFixture(t)
	got, err := ReadInstant(context.Background(), db, "hc_instant", []string{"value_a", "value_b"})
	if err != nil {
		t.Fatalf("ReadInstant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got))
	}

	first := got[0]
	if first.UUID != parentUUIDHex {
		t.Errorf("UUID = %q, want %q（小文字16進32桁）", first.UUID, parentUUIDHex)
	}
	if first.Time != 1000 || first.ZoneOffset != 32400 || first.AppID != "com.example.app" {
		t.Errorf("1件目 = %+v", first)
	}
	if !first.Values[0].Valid || first.Values[0].Float64 != 42 || first.Values[1].Float64 != 64000 {
		t.Errorf("値 = %+v, want 42 と 64000（倍率は種別側で掛ける）", first.Values)
	}

	second := got[1]
	if second.AppID != "" {
		t.Errorf("アプリが引けない行の AppID = %q, want 空", second.AppID)
	}
	if second.Values[1].Valid {
		t.Errorf("NULL の値が Valid になっている: %+v", second.Values[1])
	}
}

func TestReadInterval(t *testing.T) {
	db := newFixture(t)
	got, err := ReadInterval(context.Background(), db, "hc_interval", []string{"count"})
	if err != nil {
		t.Fatalf("ReadInterval: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("件数 = %d, want 1", len(got))
	}
	r := got[0]
	if r.StartTime != 1000 || r.EndTime != 2000 {
		t.Errorf("期間 = %d〜%d, want 1000〜2000", r.StartTime, r.EndTime)
	}
	if r.ZoneOffset != 32400 {
		t.Errorf("ZoneOffset = %d, want 32400（開始側を使う）", r.ZoneOffset)
	}
	if r.Values[0].Float64 != 5 {
		t.Errorf("値 = %+v, want 5", r.Values[0])
	}
}

// TestReadSeries_FoldsParentAndChild は親子を結合して子1行ずつ返すことを確認する。
// 親テーブル名が CamelCase でも読めること（エクスポートDBは SpeedRecordTable の
// ように大文字混じりの親を持つ）も同時に見る。
func TestReadSeries_FoldsParentAndChild(t *testing.T) {
	db := newFixture(t)
	got, err := ReadSeries(context.Background(), db, "HcSeriesParent", "hc_series_child", []string{"beats_per_minute"})
	if err != nil {
		t.Fatalf("ReadSeries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("件数 = %d, want 2（子の件数）", len(got))
	}
	for i, want := range []struct {
		epoch int64
		bpm   float64
	}{{1000, 60}, {2000, 65}} {
		if got[i].ParentUUID != parentUUIDHex {
			t.Errorf("%d件目の親UUID = %q", i, got[i].ParentUUID)
		}
		if got[i].EpochMillis != want.epoch || got[i].Values[0].Float64 != want.bpm {
			t.Errorf("%d件目 = %+v, want epoch=%d bpm=%v", i, got[i], want.epoch, want.bpm)
		}
		if got[i].ZoneOffset != 32400 || got[i].AppID != "com.example.app" {
			t.Errorf("%d件目のオフセット/アプリ = %d, %q（親から引く）", i, got[i].ZoneOffset, got[i].AppID)
		}
	}
}

// TestReadSegment は期間と種別を持つ子行を、親の終了時刻つきで返すことを確認する。
func TestReadSegment(t *testing.T) {
	db := newFixture(t)
	got, err := ReadSegment(context.Background(), db, "HcSeriesParent", "hc_segment_child",
		"stage_start_time", "stage_end_time", "stage_type")
	if err != nil {
		t.Fatalf("ReadSegment: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got))
	}
	first := got[0]
	if first.ParentUUID != parentUUIDHex || first.ParentEnd != 4000 {
		t.Errorf("親の情報 = %q, %d, want %q, 4000", first.ParentUUID, first.ParentEnd, parentUUIDHex)
	}
	if first.StartTime != 1000 || first.EndTime != 2000 || first.Type != 4 {
		t.Errorf("1件目 = %+v, want 1000〜2000 type=4", first)
	}
	if got[1].Type != 5 {
		t.Errorf("2件目の種別 = %d, want 5", got[1].Type)
	}
}

// TestRead_MissingTablesReturnEmpty は、テーブルが無いエクスポートを正常として
// 扱うことを確認する（端末やアプリの構成で存在しないことがある）。
func TestRead_MissingTablesReturnEmpty(t *testing.T) {
	db := newFixture(t)
	ctx := context.Background()

	if got, err := ReadInstant(ctx, db, "no_such_table", []string{"v"}); err != nil || len(got) != 0 {
		t.Errorf("ReadInstant = %v, %v, want 空", got, err)
	}
	if got, err := ReadInterval(ctx, db, "no_such_table", []string{"v"}); err != nil || len(got) != 0 {
		t.Errorf("ReadInterval = %v, %v, want 空", got, err)
	}
	if got, err := ReadSeries(ctx, db, "HcSeriesParent", "no_such_child", []string{"v"}); err != nil || len(got) != 0 {
		t.Errorf("ReadSeries（子なし） = %v, %v, want 空", got, err)
	}
	if got, err := ReadSeries(ctx, db, "NoSuchParent", "hc_series_child", []string{"beats_per_minute"}); err != nil || len(got) != 0 {
		t.Errorf("ReadSeries（親なし） = %v, %v, want 空", got, err)
	}
	if got, err := ReadSegment(ctx, db, "NoSuchParent", "hc_segment_child", "stage_start_time", "stage_end_time", "stage_type"); err != nil || len(got) != 0 {
		t.Errorf("ReadSegment（親なし） = %v, %v, want 空", got, err)
	}
}

// TestRead_RejectsInvalidIdentifiers は、識別子をSQLへ連結する前に検証している
// ことを確認する（未検証だとインジェクションの余地が生まれる）。
func TestRead_RejectsInvalidIdentifiers(t *testing.T) {
	db := newFixture(t)
	ctx := context.Background()

	if _, err := ReadInstant(ctx, db, "hc_instant; DROP TABLE hc_instant", []string{"value_a"}); err == nil {
		t.Error("不正なテーブル名を受け入れた")
	}
	if _, err := ReadInstant(ctx, db, "hc_instant", []string{"value_a, 1 AS x"}); err == nil {
		t.Error("不正な列名を受け入れた")
	}
	if _, err := ReadSegment(ctx, db, "HcSeriesParent", "hc_segment_child", "stage_start_time", "stage_end_time", "stage_type; DROP TABLE x"); err == nil {
		t.Error("不正な列名を受け入れた（ReadSegment）")
	}
}

func TestTableExists(t *testing.T) {
	db := newFixture(t)
	ctx := context.Background()

	if ok, err := TableExists(ctx, db, "hc_instant"); err != nil || !ok {
		t.Errorf("TableExists(hc_instant) = %v, %v, want true", ok, err)
	}
	if ok, err := TableExists(ctx, db, "no_such_table"); err != nil || ok {
		t.Errorf("TableExists(no_such_table) = %v, %v, want false", ok, err)
	}
}
