package report

import (
	"context"
	"reflect"
	"testing"
	"time"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

// fakeQuerier は Querier の固定返り値を持つテスト用実装。
type fakeQuerier struct {
	daily   map[string][]model.DailyRow
	records map[string][]model.Record
	stats   map[string]model.TypeStats

	// RecordsSince に渡された sinceMs を種別ごとに記録する。
	sinceMsByType map[string]int64
}

func (f *fakeQuerier) DailyAggregates(_ context.Context, typeKey string, _ config.TypeConfig) ([]model.DailyRow, error) {
	return f.daily[typeKey], nil
}

func (f *fakeQuerier) RecordsSince(_ context.Context, typeKey string, _ config.TypeConfig, sinceMs int64) ([]model.Record, error) {
	if f.sinceMsByType == nil {
		f.sinceMsByType = make(map[string]int64)
	}
	f.sinceMsByType[typeKey] = sinceMs
	return f.records[typeKey], nil
}

func (f *fakeQuerier) TypeStats(_ context.Context, typeKey string) (model.TypeStats, error) {
	return f.stats[typeKey], nil
}

// fourTypeConfig は仕様書の例（blood_pressure/heart_rate/sleep/steps）を再現する。
func fourTypeConfig() *config.Config {
	return &config.Config{
		Types: map[string]config.TypeConfig{
			"blood_pressure": {
				SourceTable: "blood_pressure_record_table",
				TimeLayout:  config.LayoutInstant,
				Columns: map[string]config.ColumnConfig{
					"systolic":  {Column: "systolic", Scale: 1},
					"diastolic": {Column: "diastolic", Scale: 1},
				},
				Window: "all",
				Daily:  []string{"mean", "min", "max", "count"},
			},
			"heart_rate": {
				SourceTable: "heart_rate_record_table",
				TimeLayout:  config.LayoutSeries,
				SeriesTable: "heart_rate_record_series_table",
				Columns: map[string]config.ColumnConfig{
					"bpm": {Column: "beats_per_minute", Scale: 1},
				},
				Window: "all",
				Daily:  []string{"mean", "min", "max", "count"},
			},
			"sleep": {
				SourceTable:     "sleep_session_record_table",
				TimeLayout:      config.LayoutInterval,
				Columns:         map[string]config.ColumnConfig{},
				IncludeDuration: true,
				Window:          "all",
				Daily:           []string{"sum", "count"},
			},
			"steps": {
				SourceTable: "steps_record_table",
				TimeLayout:  config.LayoutInterval,
				Columns: map[string]config.ColumnConfig{
					"count": {Column: "count", Scale: 1},
				},
				Window: "30d",
				Daily:  []string{"sum"},
			},
		},
	}
}

func TestBuildDailySummary_Header(t *testing.T) {
	cfg := fourTypeConfig()
	q := &fakeQuerier{}

	got, err := BuildDailySummary(context.Background(), q, cfg)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}

	want := []any{
		"date",
		"blood_pressure_diastolic_mean", "blood_pressure_diastolic_min", "blood_pressure_diastolic_max",
		"blood_pressure_systolic_mean", "blood_pressure_systolic_min", "blood_pressure_systolic_max",
		"blood_pressure_count",
		"heart_rate_bpm_mean", "heart_rate_bpm_min", "heart_rate_bpm_max",
		"heart_rate_count",
		"sleep_duration_min_sum",
		"sleep_count",
		"steps_count_sum",
	}
	if len(got) == 0 {
		t.Fatalf("expected at least a header row")
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("header mismatch\n got: %#v\nwant: %#v", got[0], want)
	}
}

func TestBuildDailySummary_OnlyMeanFunctionEmitsOnlyMeanColumn(t *testing.T) {
	cfg := &config.Config{
		Types: map[string]config.TypeConfig{
			"weight": {
				SourceTable: "weight_record_table",
				TimeLayout:  config.LayoutInstant,
				Columns: map[string]config.ColumnConfig{
					"weight_kg": {Column: "weight", Scale: 1},
				},
				Window: "all",
				Daily:  []string{"mean"},
			},
		},
	}
	q := &fakeQuerier{}

	got, err := BuildDailySummary(context.Background(), q, cfg)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}

	want := []any{"date", "weight_weight_kg_mean"}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("header mismatch\n got: %#v\nwant: %#v", got[0], want)
	}
}

func TestBuildDailySummary_DateUnionAndMissingCells(t *testing.T) {
	cfg := &config.Config{
		Types: map[string]config.TypeConfig{
			"a": {
				SourceTable: "a_table", TimeLayout: config.LayoutInstant,
				Columns: map[string]config.ColumnConfig{"v": {Column: "v", Scale: 1}},
				Window:  "all", Daily: []string{"mean"},
			},
			"b": {
				SourceTable: "b_table", TimeLayout: config.LayoutInstant,
				Columns: map[string]config.ColumnConfig{"v": {Column: "v", Scale: 1}},
				Window:  "all", Daily: []string{"mean"},
			},
		},
	}
	q := &fakeQuerier{
		daily: map[string][]model.DailyRow{
			"a": {
				{Date: "2024-01-01", Values: map[string]float64{"v_mean": 1.5}},
				{Date: "2024-01-03", Values: map[string]float64{"v_mean": 3.5}},
			},
			"b": {
				{Date: "2024-01-02", Values: map[string]float64{"v_mean": 2.5}},
			},
		},
	}

	got, err := BuildDailySummary(context.Background(), q, cfg)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}

	// ヘッダ + 3日ぶんの行。
	if len(got) != 4 {
		t.Fatalf("expected 4 rows (header + 3 dates), got %d: %#v", len(got), got)
	}

	want := [][]any{
		{"date", "a_v_mean", "b_v_mean"},
		{"2024-01-01", 1.5, ""},
		{"2024-01-02", "", 2.5},
		{"2024-01-03", 3.5, ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildDailySummary_ZeroValueIsNotEmptyString(t *testing.T) {
	cfg := &config.Config{
		Types: map[string]config.TypeConfig{
			"a": {
				SourceTable: "a_table", TimeLayout: config.LayoutInstant,
				Columns: map[string]config.ColumnConfig{"v": {Column: "v", Scale: 1}},
				Window:  "all", Daily: []string{"mean"},
			},
		},
	}
	q := &fakeQuerier{
		daily: map[string][]model.DailyRow{
			"a": {
				{Date: "2024-01-01", Values: map[string]float64{"v_mean": 0}},
			},
		},
	}

	got, err := BuildDailySummary(context.Background(), q, cfg)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}

	row := got[1]
	cell := row[1]
	if v, ok := cell.(float64); !ok || v != 0 {
		t.Fatalf("expected float64(0), got %#v (%T)", cell, cell)
	}
}

func typeConfigForRaw(window string, includeDuration bool, columnNames ...string) config.TypeConfig {
	cols := make(map[string]config.ColumnConfig, len(columnNames))
	for _, n := range columnNames {
		cols[n] = config.ColumnConfig{Column: n, Scale: 1}
	}
	return config.TypeConfig{
		SourceTable:     "t",
		TimeLayout:      config.LayoutInstant,
		Columns:         cols,
		IncludeDuration: includeDuration,
		Window:          window,
		Daily:           []string{"mean"},
	}
}

func TestBuildRawTab_Header(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"steps": typeConfigForRaw("all", false, "count"),
	}}
	q := &fakeQuerier{}

	got, err := BuildRawTab(context.Background(), q, cfg, "steps", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	want := []any{"local_date", "local_start", "local_end", "app_id", "count"}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("header mismatch\n got: %#v\nwant: %#v", got[0], want)
	}
}

func TestBuildRawTab_ZoneOffsetShiftsLocalTime(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"x": typeConfigForRaw("all", false, "v"),
	}}
	// 2024-01-01T00:00:00Z
	startMs := int64(1704067200000)
	q := &fakeQuerier{
		records: map[string][]model.Record{
			"x": {
				{
					UUID: "u1", StartTime: startMs, EndTime: startMs, ZoneOffset: 32400,
					AppID: "app", Values: map[string]float64{"v": 1},
				},
			},
		},
	}

	got, err := BuildRawTab(context.Background(), q, cfg, "x", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	row := got[1]
	// UTC 00:00:00 + 9h = 09:00:00
	if row[1] != "2024-01-01 09:00:00" {
		t.Fatalf("expected local_start shifted by 9h, got %v", row[1])
	}
}

func TestBuildRawTab_ZoneOffsetShiftsLocalDate(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"x": typeConfigForRaw("all", false, "v"),
	}}
	// 2024-01-01T20:00:00Z -> +9h = 2024-01-02 05:00:00 現地
	startMs := int64(1704139200000)
	q := &fakeQuerier{
		records: map[string][]model.Record{
			"x": {
				{
					UUID: "u1", StartTime: startMs, EndTime: startMs, ZoneOffset: 32400,
					AppID: "app", Values: map[string]float64{"v": 1},
				},
			},
		},
	}

	got, err := BuildRawTab(context.Background(), q, cfg, "x", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	row := got[1]
	if row[0] != "2024-01-02" {
		t.Fatalf("expected local_date to roll over to next day, got %v", row[0])
	}
}

func TestBuildRawTab_DurationMinComputedInMinutes(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"sleep": typeConfigForRaw("all", true),
	}}
	start := int64(1704067200000)
	end := start + 3600*1000 // 1時間後
	q := &fakeQuerier{
		records: map[string][]model.Record{
			"sleep": {
				{UUID: "u1", StartTime: start, EndTime: end, ZoneOffset: 0, AppID: "app", Values: map[string]float64{}},
			},
		},
	}

	got, err := BuildRawTab(context.Background(), q, cfg, "sleep", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	// header: local_date, local_start, local_end, app_id, duration_min
	durationCell := got[1][4]
	if v, ok := durationCell.(float64); !ok || v != 60 {
		t.Fatalf("expected duration_min 60, got %#v", durationCell)
	}
}

func TestBuildRawTab_MissingValueIsEmptyString(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"x": typeConfigForRaw("all", false, "v"),
	}}
	q := &fakeQuerier{
		records: map[string][]model.Record{
			"x": {
				{UUID: "u1", StartTime: 0, EndTime: 0, ZoneOffset: 0, AppID: "app", Values: map[string]float64{}},
			},
		},
	}

	got, err := BuildRawTab(context.Background(), q, cfg, "x", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}
	if got[1][4] != "" {
		t.Fatalf("expected empty string for missing value, got %#v", got[1][4])
	}
}

func TestBuildRawTab_Window30dPassesSinceMs30DaysAgo(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"steps": typeConfigForRaw("30d", false, "count"),
	}}
	q := &fakeQuerier{}
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	_, err := BuildRawTab(context.Background(), q, cfg, "steps", now)
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	want := now.Add(-30 * 24 * time.Hour).UnixMilli()
	got := q.sinceMsByType["steps"]
	if got != want {
		t.Fatalf("sinceMs mismatch: got %d, want %d", got, want)
	}
}

func TestBuildRawTab_WindowAllPassesSinceMsZero(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{
		"heart_rate": typeConfigForRaw("all", false, "bpm"),
	}}
	q := &fakeQuerier{}

	_, err := BuildRawTab(context.Background(), q, cfg, "heart_rate", time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}

	if got := q.sinceMsByType["heart_rate"]; got != 0 {
		t.Fatalf("expected sinceMs 0 for window all, got %d", got)
	}
}

func TestBuildMeta_RowOrderAndZeroCountLatestIsEmpty(t *testing.T) {
	cfg := &config.Config{
		Types: map[string]config.TypeConfig{
			"a": typeConfigForRaw("all", false, "v"),
			"b": typeConfigForRaw("all", false, "v"),
		},
	}
	q := &fakeQuerier{
		stats: map[string]model.TypeStats{
			"a": {Count: 0, LatestStartTime: 0},
			"b": {Count: 5, LatestStartTime: 1704067200000}, // 2024-01-01T00:00:00Z
		},
	}
	lastSuccess := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	zipModified := time.Date(2024, 5, 31, 9, 0, 0, 0, time.UTC)

	got, err := BuildMeta(context.Background(), q, cfg, lastSuccess, zipModified)
	if err != nil {
		t.Fatalf("BuildMeta: %v", err)
	}

	want := [][]any{
		{"key", "value"},
		{"last_success_at", lastSuccess.Format(time.RFC3339)},
		{"last_processed_zip_modified_time", zipModified.Format(time.RFC3339)},
		{"generated_at", lastSuccess.Format(time.RFC3339)},
		{"a_record_count", int64(0)},
		{"a_latest_record_at", ""},
		{"b_record_count", int64(5)},
		{"b_latest_record_at", "2024-01-01 00:00:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildMeta_ZeroLastSuccessIsEmptyString(t *testing.T) {
	cfg := &config.Config{Types: map[string]config.TypeConfig{}}
	q := &fakeQuerier{}

	got, err := BuildMeta(context.Background(), q, cfg, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("BuildMeta: %v", err)
	}

	want := [][]any{
		{"key", "value"},
		{"last_success_at", ""},
		{"last_processed_zip_modified_time", ""},
		{"generated_at", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRawTabTitle(t *testing.T) {
	if got := RawTabTitle("steps"); got != "steps_raw" {
		t.Fatalf("expected steps_raw, got %q", got)
	}
}
