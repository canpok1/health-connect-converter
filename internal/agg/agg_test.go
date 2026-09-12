package agg

import (
	"testing"

	"health-connect-converter/internal/model"
)

const (
	appPriority = "com.example.priority"
	appLower    = "com.example.lower"
)

// rec は集計用モデルを組み立てる。時刻はミリ秒。
func rec(date string, start, end int64, app string, values map[string]float64) model.AggRecord {
	return model.AggRecord{
		LocalDate: date, StartTime: start, EndTime: end,
		ZoneOffset: 32400, AppID: app, Values: values,
	}
}

func valuesOf(t *testing.T, rows []model.DailyRow, date string) map[string]float64 {
	t.Helper()
	for _, r := range rows {
		if r.Date == date {
			return r.Values
		}
	}
	t.Fatalf("%s の行が無い: %+v", date, rows)
	return nil
}

func TestDaily_AllFunctions(t *testing.T) {
	recs := []model.AggRecord{
		rec("2026-09-11", 1000, 1000, appPriority, map[string]float64{"v": 10}),
		rec("2026-09-11", 2000, 2000, appPriority, map[string]float64{"v": 30}),
	}

	rows := Daily(recs, Options{Daily: []string{"mean", "min", "max", "sum", "count"}}, nil)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d, want 1", len(rows))
	}
	got := valuesOf(t, rows, "2026-09-11")
	want := map[string]float64{"v_mean": 20, "v_min": 10, "v_max": 30, "v_sum": 40, "count": 2}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("列数 = %d, want %d（%+v）", len(got), len(want), got)
	}
}

func TestDaily_OnlyRequestedFunctions(t *testing.T) {
	recs := []model.AggRecord{rec("2026-09-11", 1000, 1000, appPriority, map[string]float64{"v": 10})}

	rows := Daily(recs, Options{Daily: []string{"mean"}}, nil)
	got := valuesOf(t, rows, "2026-09-11")
	if len(got) != 1 || got["v_mean"] != 10 {
		t.Errorf("values = %+v, want v_mean だけ", got)
	}
}

func TestDaily_SeparatesDates(t *testing.T) {
	recs := []model.AggRecord{
		rec("2026-09-10", 1000, 1000, appPriority, map[string]float64{"v": 1}),
		rec("2026-09-11", 2000, 2000, appPriority, map[string]float64{"v": 2}),
	}

	rows := Daily(recs, Options{Daily: []string{"sum"}}, nil)
	if len(rows) != 2 {
		t.Fatalf("行数 = %d, want 2", len(rows))
	}
	// 日付の昇順で返す。
	if rows[0].Date != "2026-09-10" || rows[1].Date != "2026-09-11" {
		t.Errorf("日付の順序 = %s, %s", rows[0].Date, rows[1].Date)
	}
	if valuesOf(t, rows, "2026-09-10")["v_sum"] != 1 || valuesOf(t, rows, "2026-09-11")["v_sum"] != 2 {
		t.Errorf("日ごとの合計が混ざっている: %+v", rows)
	}
}

// TestDaily_DedupesByAppPriority は、優先度の高いアプリが記録していた時間帯の
// 下位アプリのレコードを合計から外すことを確認する（ADR 0009）。
func TestDaily_DedupesByAppPriority(t *testing.T) {
	const minute = 60 * 1000
	recs := []model.AggRecord{
		// 優先アプリ: 10:00-10:30
		rec("2026-09-11", 10*60*minute, 10*60*minute+30*minute, appPriority, map[string]float64{"v": 1000}),
		// 下位アプリ: 完全に覆われる 10:10-10:20
		rec("2026-09-11", 10*60*minute+10*minute, 10*60*minute+20*minute, appLower, map[string]float64{"v": 800}),
	}

	rows := Daily(recs, Options{Daily: []string{"sum", "count"}, Dedupe: true}, []string{appPriority, appLower})
	got := valuesOf(t, rows, "2026-09-11")
	if got["v_sum"] != 1000 {
		t.Errorf("v_sum = %v, want 1000（下位アプリのぶんを数えない）", got["v_sum"])
	}
	if got["count"] != 1 {
		t.Errorf("count = %v, want 1", got["count"])
	}
}

// TestDaily_DedupePartialOverlapCountsUncoveredRatio は、一部だけ重なる場合に
// 重なっていない割合ぶんを数えることを確認する。
func TestDaily_DedupePartialOverlapCountsUncoveredRatio(t *testing.T) {
	const minute = 60 * 1000
	recs := []model.AggRecord{
		rec("2026-09-11", 0, 10*minute, appPriority, map[string]float64{"v": 100}),
		// 半分だけ重なる: 5分〜15分
		rec("2026-09-11", 5*minute, 15*minute, appLower, map[string]float64{"v": 100}),
	}

	rows := Daily(recs, Options{Daily: []string{"sum", "mean"}, Dedupe: true}, []string{appPriority, appLower})
	got := valuesOf(t, rows, "2026-09-11")
	if got["v_sum"] != 150 {
		t.Errorf("v_sum = %v, want 150（下位は半分だけ数える）", got["v_sum"])
	}
	// 平均は割合を掛けない（測定値そのものを見るため）。
	if got["v_mean"] != 100 {
		t.Errorf("v_mean = %v, want 100", got["v_mean"])
	}
}

// TestDaily_NoPrioritiesKeepsAll は、優先度が読めていないときに重複排除を
// 行わないことを確認する（消してしまうより多いまま出す方が気付ける）。
func TestDaily_NoPrioritiesKeepsAll(t *testing.T) {
	const minute = 60 * 1000
	recs := []model.AggRecord{
		rec("2026-09-11", 0, 10*minute, appPriority, map[string]float64{"v": 100}),
		rec("2026-09-11", 0, 10*minute, appLower, map[string]float64{"v": 100}),
	}

	rows := Daily(recs, Options{Daily: []string{"sum"}, Dedupe: true}, nil)
	if got := valuesOf(t, rows, "2026-09-11")["v_sum"]; got != 200 {
		t.Errorf("v_sum = %v, want 200", got)
	}
}

// TestDaily_DedupeUsesStartDateForSpans は、覆う範囲を「記録が始まった日」で
// 区切ることを確認する。集計の日（LocalDate）が別の日に寄っていても、範囲は
// 実際に記録された時間帯で考える。
func TestDaily_DedupeUsesStartDateForSpans(t *testing.T) {
	const hour = 60 * 60 * 1000
	// 9/10 23:00〜9/11 06:30 の睡眠。集計は起床日（9/11）に寄せる。
	start := int64(1757509200000) // 2026-09-10 23:00 JST 相当の値（テスト内で相対比較のみ）
	recs := []model.AggRecord{
		rec("2026-09-11", start, start+7*hour, appPriority, map[string]float64{"duration_min": 420}),
		// 同じ時間帯を下位アプリも書いている。
		rec("2026-09-11", start+hour, start+3*hour, appLower, map[string]float64{"duration_min": 120}),
	}

	rows := Daily(recs, Options{Daily: []string{"sum", "count"}, Dedupe: true}, []string{appPriority, appLower})
	got := valuesOf(t, rows, "2026-09-11")
	if got["duration_min_sum"] != 420 {
		t.Errorf("duration_min_sum = %v, want 420（下位アプリぶんを数えない）", got["duration_min_sum"])
	}
}

func TestDaily_EmptyInput(t *testing.T) {
	if rows := Daily(nil, Options{Daily: []string{"sum"}}, nil); len(rows) != 0 {
		t.Errorf("行数 = %d, want 0", len(rows))
	}
}
