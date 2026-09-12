package report

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// fakeKind は種別のうち report が見る部分（キー・方針・値名・ヘッダ）だけを持つ
// テスト用実装。累積DBを触るメソッドは fakeQuerier 側で差し替えるため呼ばれない。
type fakeKind struct {
	key    string
	policy kind.Policy
	values []string
}

func (k fakeKind) Key() string         { return k.key }
func (k fakeKind) Policy() kind.Policy { return k.policy }
func (k fakeKind) ValueNames() []string {
	return k.values
}

func (k fakeKind) RawHeader() []any {
	out := []any{"local_date", "local_start", "local_end", "app_id"}
	for _, v := range k.values {
		out = append(out, v)
	}
	return out
}

func (k fakeKind) Table() cumdb.Table { return cumdb.Table{} }

func (k fakeKind) ExportRows(context.Context, *sql.DB) ([][]any, []string, error) {
	return nil, nil, nil
}

func (k fakeKind) Aggregate(context.Context, *sql.DB) ([]model.AggRecord, error) { return nil, nil }

func (k fakeKind) RawRows(context.Context, *sql.DB, int64) ([][]any, error) { return nil, nil }

// fakeQuerier は Querier の固定返り値を持つテスト用実装。
type fakeQuerier struct {
	daily map[string][]model.DailyRow
	raw   map[string][][]any
	stats map[string]model.TypeStats

	// RawRows に渡された sinceMs を種別ごとに記録する。
	sinceMsByType map[string]int64
}

func (f *fakeQuerier) DailyAggregates(_ context.Context, k kind.Kind) ([]model.DailyRow, error) {
	return f.daily[k.Key()], nil
}

func (f *fakeQuerier) RawRows(_ context.Context, k kind.Kind, sinceMs int64) ([][]any, error) {
	if f.sinceMsByType == nil {
		f.sinceMsByType = make(map[string]int64)
	}
	f.sinceMsByType[k.Key()] = sinceMs
	return f.raw[k.Key()], nil
}

func (f *fakeQuerier) Stats(_ context.Context, k kind.Kind) (model.TypeStats, error) {
	return f.stats[k.Key()], nil
}

var (
	mmmc = []string{kind.FuncMean, kind.FuncMin, kind.FuncMax, kind.FuncCount}

	// fourKinds は仕様書の例（血圧・心拍・睡眠・歩数）を再現する。辞書順。
	fourKinds = []kind.Kind{
		fakeKind{key: "blood_pressure", values: []string{"diastolic", "systolic"},
			policy: kind.Policy{Window: kind.WindowAll, Daily: mmmc}},
		fakeKind{key: "heart_rate", values: []string{"bpm"},
			policy: kind.Policy{Window: kind.WindowAll, Daily: mmmc}},
		fakeKind{key: "sleep", values: []string{"duration_min"},
			policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum, kind.FuncCount}}},
		fakeKind{key: "steps", values: []string{"count"},
			policy: kind.Policy{Window: "30d", Daily: []string{kind.FuncSum}}},
	}
)

func TestBuildDailySummary_Header(t *testing.T) {
	got, err := BuildDailySummary(context.Background(), &fakeQuerier{}, fourKinds)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("行数 = %d, want 1（ヘッダのみ）", len(got))
	}

	want := []any{
		"date",
		// 値名は辞書順、関数は mean→min→max→sum の順、count は種別の末尾。
		"blood_pressure_diastolic_mean", "blood_pressure_diastolic_min", "blood_pressure_diastolic_max",
		"blood_pressure_systolic_mean", "blood_pressure_systolic_min", "blood_pressure_systolic_max",
		"blood_pressure_count",
		"heart_rate_bpm_mean", "heart_rate_bpm_min", "heart_rate_bpm_max", "heart_rate_count",
		"sleep_duration_min_sum", "sleep_count",
		"steps_count_sum",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("ヘッダ mismatch\n got: %v\nwant: %v", got[0], want)
	}
}

func TestBuildDailySummary_OnlyRequestedFunctions(t *testing.T) {
	kinds := []kind.Kind{fakeKind{key: "weight", values: []string{"kg"},
		policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncMean}}}}

	got, err := BuildDailySummary(context.Background(), &fakeQuerier{}, kinds)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}
	want := []any{"date", "weight_kg_mean"}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("ヘッダ = %v, want %v", got[0], want)
	}
}

// TestBuildDailySummary_DateUnionAndMissingCells は、種別ごとに日付が違っても
// 全部の日を行として出し、値が無いセルは空にすることを確認する。
func TestBuildDailySummary_DateUnionAndMissingCells(t *testing.T) {
	kinds := []kind.Kind{
		fakeKind{key: "a", values: []string{"v"}, policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}},
		fakeKind{key: "b", values: []string{"v"}, policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}},
	}
	q := &fakeQuerier{daily: map[string][]model.DailyRow{
		"a": {{Date: "2026-09-10", Values: map[string]float64{"v_sum": 1}}},
		"b": {{Date: "2026-09-11", Values: map[string]float64{"v_sum": 2}}},
	}}

	got, err := BuildDailySummary(context.Background(), q, kinds)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}
	want := [][]any{
		{"date", "a_v_sum", "b_v_sum"},
		{"2026-09-10", 1.0, ""},
		{"2026-09-11", "", 2.0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("行列 mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestBuildDailySummary_ZeroValueIsNotEmpty は 0 を空セルと混同しないことを確認する。
func TestBuildDailySummary_ZeroValueIsNotEmpty(t *testing.T) {
	kinds := []kind.Kind{fakeKind{key: "steps", values: []string{"count"},
		policy: kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}}}
	q := &fakeQuerier{daily: map[string][]model.DailyRow{
		"steps": {{Date: "2026-09-11", Values: map[string]float64{"count_sum": 0}}},
	}}

	got, err := BuildDailySummary(context.Background(), q, kinds)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}
	if got[1][1] != 0.0 {
		t.Errorf("セル = %#v, want 0（空文字ではない）", got[1][1])
	}
}

func TestBuildRawTab_HeaderComesFromKindAndRowsFollow(t *testing.T) {
	k := fakeKind{key: "weight", values: []string{"kg"}, policy: kind.Policy{Window: kind.WindowAll}}
	q := &fakeQuerier{raw: map[string][][]any{
		"weight": {{"2026-09-11", "2026-09-11 07:00:00", "2026-09-11 07:00:00", "app", 64.0}},
	}}

	got, err := BuildRawTab(context.Background(), q, k, time.Now())
	if err != nil {
		t.Fatalf("BuildRawTab: %v", err)
	}
	want := [][]any{
		{"local_date", "local_start", "local_end", "app_id", "kg"},
		{"2026-09-11", "2026-09-11 07:00:00", "2026-09-11 07:00:00", "app", 64.0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("行列 mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestBuildRawTab_WindowDecidesSince は、窓の指定が問い合わせの起点になることを
// 確認する。"all" は 0（全期間）。
func TestBuildRawTab_WindowDecidesSince(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		window string
		want   int64
	}{
		{kind.WindowAll, 0},
		{"1d", now.AddDate(0, 0, -1).UnixMilli()},
		{"30d", now.AddDate(0, 0, -30).UnixMilli()},
	}
	for _, c := range cases {
		q := &fakeQuerier{}
		k := fakeKind{key: "k", values: []string{"v"}, policy: kind.Policy{Window: c.window}}
		if _, err := BuildRawTab(context.Background(), q, k, now); err != nil {
			t.Fatalf("BuildRawTab(%s): %v", c.window, err)
		}
		if got := q.sinceMsByType["k"]; got != c.want {
			t.Errorf("window %q の sinceMs = %d, want %d", c.window, got, c.want)
		}
	}
}

func TestBuildRawTab_InvalidWindowErrors(t *testing.T) {
	k := fakeKind{key: "k", values: []string{"v"}, policy: kind.Policy{Window: "毎日"}}
	if _, err := BuildRawTab(context.Background(), &fakeQuerier{}, k, time.Now()); err == nil {
		t.Error("不正な窓でエラーにならなかった")
	}
}

func TestBuildMeta_RowOrderAndEmptyLatest(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	zipModified := time.Date(2026, 9, 12, 1, 30, 0, 0, time.UTC)
	kinds := []kind.Kind{
		fakeKind{key: "a", values: []string{"v"}, policy: kind.Policy{Window: kind.WindowAll}},
		fakeKind{key: "b", values: []string{"v"}, policy: kind.Policy{Window: kind.WindowAll}},
	}
	q := &fakeQuerier{stats: map[string]model.TypeStats{
		"a": {Count: 3, LatestStartTime: time.Date(2026, 9, 11, 22, 0, 0, 0, time.UTC).UnixMilli()},
		// b は0件。最新レコードの時刻は空になる。
	}}

	got, err := BuildMeta(context.Background(), q, kinds, now, zipModified, map[string]int64{"z_table": 1, "a_table": 0})
	if err != nil {
		t.Fatalf("BuildMeta: %v", err)
	}
	want := [][]any{
		{"key", "value"},
		{"last_success_at", "2026-09-12T12:00:00Z"},
		{"last_processed_zip_modified_time", "2026-09-12T01:30:00Z"},
		{"generated_at", "2026-09-12T12:00:00Z"},
		{"a_record_count", int64(3)},
		{"a_latest_record_at", "2026-09-11 22:00:00Z"},
		{"b_record_count", int64(0)},
		{"b_latest_record_at", ""},
		// エクスポートDBの行数はテーブル名の昇順で末尾に出る（0件も出す）。
		{"export_a_table_rows", int64(0)},
		{"export_z_table_rows", int64(1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("行列 mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildMeta_ZeroTimesAreEmpty(t *testing.T) {
	got, err := BuildMeta(context.Background(), &fakeQuerier{}, nil, time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatalf("BuildMeta: %v", err)
	}
	for _, row := range got[1:] {
		if row[1] != "" {
			t.Errorf("%v = %#v, want 空文字", row[0], row[1])
		}
	}
}

func TestRawTabTitle(t *testing.T) {
	if got := RawTabTitle("blood_pressure"); got != "blood_pressure_raw" {
		t.Errorf("RawTabTitle = %q, want blood_pressure_raw", got)
	}
}
