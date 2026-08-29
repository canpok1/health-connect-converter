package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
	"health-connect-converter/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func weightConfig(extraColumns ...string) config.TypeConfig {
	cols := map[string]config.ColumnConfig{
		"weight_kg": {Column: "weight", Scale: 1},
	}
	for _, name := range extraColumns {
		cols[name] = config.ColumnConfig{Column: name, Scale: 1}
	}
	return config.TypeConfig{
		SourceTable: "weight_record",
		TimeLayout:  config.LayoutInstant,
		Columns:     cols,
		Window:      "all",
		Daily:       []string{"mean", "min", "max", "sum", "count"},
	}
}

func sleepConfig() config.TypeConfig {
	return config.TypeConfig{
		SourceTable:     "sleep_session_record",
		TimeLayout:      config.LayoutInterval,
		Window:          "all",
		Daily:           []string{"sum", "count"},
		IncludeDuration: true,
	}
}

func utcMs(t *testing.T, layout string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, layout)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", layout, err)
	}
	return parsed.UnixMilli()
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": weightConfig()}}

	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate #1: %v", err)
	}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate #2: %v", err)
	}
}

func TestMigrateAddsColumnAndKeepsExistingRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	tc1 := weightConfig()
	cfg1 := &config.Config{Types: map[string]config.TypeConfig{"weight": tc1}}
	if err := s.Migrate(ctx, cfg1); err != nil {
		t.Fatalf("Migrate #1: %v", err)
	}

	rec := model.Record{
		UUID:       "u1",
		StartTime:  utcMs(t, "2024-01-01T00:00:00Z"),
		EndTime:    utcMs(t, "2024-01-01T00:00:00Z"),
		ZoneOffset: 0,
		AppID:      "app1",
		Values:     map[string]float64{"weight_kg": 50},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc1, []model.Record{rec}); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	tc2 := weightConfig("height_cm")
	cfg2 := &config.Config{Types: map[string]config.TypeConfig{"weight": tc2}}
	if err := s.Migrate(ctx, cfg2); err != nil {
		t.Fatalf("Migrate #2 (add column): %v", err)
	}

	recs, err := s.RecordsSince(ctx, "weight", tc2, 0)
	if err != nil {
		t.Fatalf("RecordsSince: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("len(recs) = %d, want 1", len(recs))
	}
	got := recs[0]
	if got.UUID != "u1" {
		t.Errorf("UUID = %q, want u1", got.UUID)
	}
	if v, ok := got.Values["weight_kg"]; !ok || v != 50 {
		t.Errorf("weight_kg = %v, %v; want 50, true", v, ok)
	}
	if _, ok := got.Values["height_cm"]; ok {
		t.Errorf("height_cm should be NULL (absent), got present")
	}

	rec2 := model.Record{
		UUID:       "u2",
		StartTime:  utcMs(t, "2024-01-02T00:00:00Z"),
		EndTime:    utcMs(t, "2024-01-02T00:00:00Z"),
		ZoneOffset: 0,
		AppID:      "app1",
		Values:     map[string]float64{"weight_kg": 60, "height_cm": 170},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc2, []model.Record{rec2}); err != nil {
		t.Fatalf("UpsertRecords into new column: %v", err)
	}
	recs2, err := s.RecordsSince(ctx, "weight", tc2, 0)
	if err != nil {
		t.Fatalf("RecordsSince #2: %v", err)
	}
	if len(recs2) != 2 {
		t.Fatalf("len(recs2) = %d, want 2", len(recs2))
	}
}

func TestUpsertRecordsUpdatesExistingUUID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	rec := model.Record{
		UUID:       "u1",
		StartTime:  utcMs(t, "2024-01-01T00:00:00Z"),
		EndTime:    utcMs(t, "2024-01-01T00:00:00Z"),
		ZoneOffset: 0,
		AppID:      "app1",
		Values:     map[string]float64{"weight_kg": 50},
	}
	if n, err := s.UpsertRecords(ctx, "weight", tc, []model.Record{rec}); err != nil || n != 1 {
		t.Fatalf("UpsertRecords #1: n=%d, err=%v", n, err)
	}

	rec.Values["weight_kg"] = 70
	rec.AppID = "app2"
	if n, err := s.UpsertRecords(ctx, "weight", tc, []model.Record{rec}); err != nil || n != 1 {
		t.Fatalf("UpsertRecords #2: n=%d, err=%v", n, err)
	}

	stats, err := s.TypeStats(ctx, "weight")
	if err != nil {
		t.Fatalf("TypeStats: %v", err)
	}
	if stats.Count != 1 {
		t.Fatalf("Count = %d, want 1 (row must be updated, not duplicated)", stats.Count)
	}

	recs, err := s.RecordsSince(ctx, "weight", tc, 0)
	if err != nil {
		t.Fatalf("RecordsSince: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("len(recs) = %d, want 1", len(recs))
	}
	if recs[0].Values["weight_kg"] != 70 {
		t.Errorf("weight_kg = %v, want 70", recs[0].Values["weight_kg"])
	}
	if recs[0].AppID != "app2" {
		t.Errorf("AppID = %q, want app2", recs[0].AppID)
	}
}

func TestUpsertRecordsEmptyIsNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	n, err := s.UpsertRecords(ctx, "weight", tc, nil)
	if err != nil || n != 0 {
		t.Fatalf("UpsertRecords(nil) = %d, %v; want 0, nil", n, err)
	}
}

func TestRecordsSinceOrderingAndBoundary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t1 := utcMs(t, "2024-01-01T00:00:00Z")
	t2 := utcMs(t, "2024-01-02T00:00:00Z")
	t3 := utcMs(t, "2024-01-03T00:00:00Z")
	recs := []model.Record{
		{UUID: "u3", StartTime: t3, EndTime: t3, Values: map[string]float64{"weight_kg": 3}},
		{UUID: "u1", StartTime: t1, EndTime: t1, Values: map[string]float64{"weight_kg": 1}},
		{UUID: "u2", StartTime: t2, EndTime: t2, Values: map[string]float64{"weight_kg": 2}},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc, recs); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	got, err := s.RecordsSince(ctx, "weight", tc, t2)
	if err != nil {
		t.Fatalf("RecordsSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (boundary t2 must be included)", len(got))
	}
	if got[0].UUID != "u2" || got[1].UUID != "u3" {
		t.Fatalf("got UUIDs = [%s, %s], want [u2, u3]", got[0].UUID, got[1].UUID)
	}

	all, err := s.RecordsSince(ctx, "weight", tc, 0)
	if err != nil {
		t.Fatalf("RecordsSince(0): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(all))
	}
	if all[0].UUID != "u1" || all[1].UUID != "u2" || all[2].UUID != "u3" {
		t.Fatalf("all not in ascending start_time order: %v", all)
	}
}

func TestDailyAggregatesZoneOffsetAndFunctions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const zoneOffset = 32400 // JST, +9h

	// UTC 2024-01-01T20:00:00 は zone_offset=32400 で現地 2024-01-02T05:00:00。
	tA := utcMs(t, "2024-01-01T20:00:00Z")
	// UTC 2024-01-02T00:00:00 は zone_offset=32400 で現地 2024-01-02T09:00:00。
	// UTC 日付は tA と別日だが、現地日は tA と同じ 2024-01-02 にまとまるはず。
	tB := utcMs(t, "2024-01-02T00:00:00Z")
	// weight_kg が欠測の行。count には数えるが mean/min/max/sum の分母には入らない。
	tC := utcMs(t, "2024-01-02T01:00:00Z")

	recs := []model.Record{
		{UUID: "a", StartTime: tA, EndTime: tA, ZoneOffset: zoneOffset, Values: map[string]float64{"weight_kg": 50}},
		{UUID: "b", StartTime: tB, EndTime: tB, ZoneOffset: zoneOffset, Values: map[string]float64{"weight_kg": 60}},
		{UUID: "c", StartTime: tC, EndTime: tC, ZoneOffset: zoneOffset, Values: map[string]float64{}},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc, recs); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	rows, err := s.DailyAggregates(ctx, "weight", tc)
	if err != nil {
		t.Fatalf("DailyAggregates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (all three records fall in the same local day)", len(rows))
	}
	row := rows[0]
	if row.Date != "2024-01-02" {
		t.Fatalf("Date = %q, want 2024-01-02", row.Date)
	}
	if row.Values["count"] != 3 {
		t.Errorf("count = %v, want 3", row.Values["count"])
	}
	if row.Values["weight_kg_mean"] != 55 {
		t.Errorf("weight_kg_mean = %v, want 55 (NULL row must not affect the denominator)", row.Values["weight_kg_mean"])
	}
	if row.Values["weight_kg_min"] != 50 {
		t.Errorf("weight_kg_min = %v, want 50", row.Values["weight_kg_min"])
	}
	if row.Values["weight_kg_max"] != 60 {
		t.Errorf("weight_kg_max = %v, want 60", row.Values["weight_kg_max"])
	}
	if row.Values["weight_kg_sum"] != 110 {
		t.Errorf("weight_kg_sum = %v, want 110", row.Values["weight_kg_sum"])
	}
}

func TestDailyAggregatesSeparatesDifferentLocalDays(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t1 := utcMs(t, "2024-01-01T00:00:00Z")
	t2 := utcMs(t, "2024-01-02T00:00:00Z")
	recs := []model.Record{
		{UUID: "a", StartTime: t1, EndTime: t1, Values: map[string]float64{"weight_kg": 10}},
		{UUID: "b", StartTime: t2, EndTime: t2, Values: map[string]float64{"weight_kg": 20}},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc, recs); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	rows, err := s.DailyAggregates(ctx, "weight", tc)
	if err != nil {
		t.Fatalf("DailyAggregates: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Date != "2024-01-01" || rows[1].Date != "2024-01-02" {
		t.Fatalf("dates = [%s, %s], want [2024-01-01, 2024-01-02]", rows[0].Date, rows[1].Date)
	}
}

func TestDailyAggregatesDurationMinSum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := sleepConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"sleep": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	start1 := utcMs(t, "2024-01-01T22:00:00Z")
	end1 := utcMs(t, "2024-01-01T23:30:00Z") // 90分
	start2 := utcMs(t, "2024-01-01T23:45:00Z")
	end2 := utcMs(t, "2024-01-02T00:15:00Z") // 30分

	recs := []model.Record{
		{UUID: "a", StartTime: start1, EndTime: end1, Values: map[string]float64{}},
		{UUID: "b", StartTime: start2, EndTime: end2, Values: map[string]float64{}},
	}
	if _, err := s.UpsertRecords(ctx, "sleep", tc, recs); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	rows, err := s.DailyAggregates(ctx, "sleep", tc)
	if err != nil {
		t.Fatalf("DailyAggregates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Values["duration_min_sum"] != 120 {
		t.Errorf("duration_min_sum = %v, want 120", rows[0].Values["duration_min_sum"])
	}
	if rows[0].Values["count"] != 2 {
		t.Errorf("count = %v, want 2", rows[0].Values["count"])
	}
}

func TestColumnsEmptyTypeWorks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := sleepConfig()
	if len(tc.Columns) != 0 {
		t.Fatalf("sleepConfig should have empty Columns, got %v", tc.Columns)
	}
	cfg := &config.Config{Types: map[string]config.TypeConfig{"sleep": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate #2: %v", err)
	}

	start := utcMs(t, "2024-01-01T22:00:00Z")
	end := utcMs(t, "2024-01-01T23:00:00Z")
	rec := model.Record{UUID: "u1", StartTime: start, EndTime: end, Values: map[string]float64{}}
	if n, err := s.UpsertRecords(ctx, "sleep", tc, []model.Record{rec}); err != nil || n != 1 {
		t.Fatalf("UpsertRecords: n=%d, err=%v", n, err)
	}

	recs, err := s.RecordsSince(ctx, "sleep", tc, 0)
	if err != nil {
		t.Fatalf("RecordsSince: %v", err)
	}
	if len(recs) != 1 || recs[0].UUID != "u1" {
		t.Fatalf("recs = %v, want [u1]", recs)
	}
}

func TestGetSetState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": weightConfig()}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := s.GetState(ctx, "missing_key")
	if err != nil || v != "" {
		t.Fatalf("GetState(missing) = %q, %v; want \"\", nil", v, err)
	}

	if err := s.SetState(ctx, "k1", "v1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	v, err = s.GetState(ctx, "k1")
	if err != nil || v != "v1" {
		t.Fatalf("GetState(k1) = %q, %v; want v1, nil", v, err)
	}

	if err := s.SetState(ctx, "k1", "v2"); err != nil {
		t.Fatalf("SetState overwrite: %v", err)
	}
	v, err = s.GetState(ctx, "k1")
	if err != nil || v != "v2" {
		t.Fatalf("GetState(k1) after overwrite = %q, %v; want v2, nil", v, err)
	}
}

func TestTypeStatsEmptyAndPopulated(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tc := weightConfig()
	cfg := &config.Config{Types: map[string]config.TypeConfig{"weight": tc}}
	if err := s.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	stats, err := s.TypeStats(ctx, "weight")
	if err != nil {
		t.Fatalf("TypeStats(empty): %v", err)
	}
	if stats != (model.TypeStats{Count: 0, LatestStartTime: 0}) {
		t.Fatalf("TypeStats(empty) = %+v, want {0 0}", stats)
	}

	t1 := utcMs(t, "2024-01-01T00:00:00Z")
	t2 := utcMs(t, "2024-01-02T00:00:00Z")
	recs := []model.Record{
		{UUID: "a", StartTime: t1, EndTime: t1, Values: map[string]float64{"weight_kg": 1}},
		{UUID: "b", StartTime: t2, EndTime: t2, Values: map[string]float64{"weight_kg": 2}},
	}
	if _, err := s.UpsertRecords(ctx, "weight", tc, recs); err != nil {
		t.Fatalf("UpsertRecords: %v", err)
	}

	stats, err = s.TypeStats(ctx, "weight")
	if err != nil {
		t.Fatalf("TypeStats(populated): %v", err)
	}
	if stats.Count != 2 || stats.LatestStartTime != t2 {
		t.Fatalf("TypeStats(populated) = %+v, want {2 %d}", stats, t2)
	}
}
