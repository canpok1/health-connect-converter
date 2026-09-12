// Package agg は集計用モデル（model.AggRecord）だけを見て、アプリ優先度による
// 重複排除と日次集計を行う。
//
// 値の意味（血圧か歩数か）には一切触れない。種別ごとの事情は保存用モデルから
// 集計用モデルへの変換で済ませる（ADR 0012）。
package agg

import (
	"math"
	"sort"
	"time"

	"health-connect-converter/internal/model"
)

// Options は日次集計のやり方。種別が持つ方針から渡す。
type Options struct {
	// Daily は出す集計関数（mean / min / max / sum / count）。
	Daily []string
	// Dedupe が真なら、集計の前にアプリ優先度で重複排除する。
	Dedupe bool
}

// Daily は現地日ごとに集計する。order はアプリ優先度（先頭が最優先）で、
// Options.Dedupe が真のときだけ使う。空なら重複排除しない。
func Daily(recs []model.AggRecord, opt Options, order []string) []model.DailyRow {
	if opt.Dedupe {
		return aggregate(dedupeByPriority(recs, order), opt)
	}
	return aggregate(unweighted(recs), opt)
}

// weighted は重複排除の結果、レコードのうち採用する割合を持つ。時間帯の一部だけが
// 優先度の高いアプリと重なるとき、重なっていない割合ぶんだけを数える。
type weighted struct {
	rec   model.AggRecord
	ratio float64
}

func unweighted(recs []model.AggRecord) []weighted {
	out := make([]weighted, len(recs))
	for i, rec := range recs {
		out[i] = weighted{rec: rec, ratio: 1}
	}
	return out
}

type dailyAccum struct {
	// sum は重複を除いた割合ぶんの合計、rawSum は割合を掛けない合計（平均に使う）。
	sum    float64
	rawSum float64
	min    float64
	max    float64
	count  int
}

func aggregate(recs []weighted, opt Options) []model.DailyRow {
	funcs := make(map[string]bool, len(opt.Daily))
	for _, fn := range opt.Daily {
		funcs[fn] = true
	}

	// 日 -> 値名 -> 集計中の値
	byDate := make(map[string]map[string]*dailyAccum)
	recordCount := make(map[string]int)

	for _, w := range recs {
		date := w.rec.LocalDate
		recordCount[date]++

		accs, ok := byDate[date]
		if !ok {
			accs = make(map[string]*dailyAccum, len(w.rec.Values))
			byDate[date] = accs
		}
		for name, v := range w.rec.Values {
			acc, ok := accs[name]
			if !ok {
				acc = &dailyAccum{min: math.Inf(1), max: math.Inf(-1)}
				accs[name] = acc
			}
			// 合計だけは重複を除いた割合ぶんにする。平均・最小・最大は
			// 1レコードの測定値そのものを見るものなので割合を掛けない。
			acc.sum += v * w.ratio
			acc.rawSum += v
			acc.count++
			acc.min = math.Min(acc.min, v)
			acc.max = math.Max(acc.max, v)
		}
	}

	dates := make([]string, 0, len(recordCount))
	for date := range recordCount {
		dates = append(dates, date)
	}
	sort.Strings(dates)

	rows := make([]model.DailyRow, 0, len(dates))
	for _, date := range dates {
		row := model.DailyRow{Date: date, Values: make(map[string]float64)}
		for name, acc := range byDate[date] {
			if acc.count == 0 {
				continue
			}
			if funcs["sum"] {
				row.Values[name+"_sum"] = acc.sum
			}
			if funcs["mean"] {
				row.Values[name+"_mean"] = acc.rawSum / float64(acc.count)
			}
			if funcs["min"] {
				row.Values[name+"_min"] = acc.min
			}
			if funcs["max"] {
				row.Values[name+"_max"] = acc.max
			}
		}
		if funcs["count"] {
			row.Values["count"] = float64(recordCount[date])
		}
		rows = append(rows, row)
	}
	return rows
}

type interval struct {
	start int64
	end   int64
}

// dedupeByPriority は、優先度の高いアプリが既に覆っている時間帯のレコードを落とす。
// 複数のアプリが同じ実測（歩数・消費カロリーなど）を書いていると単純な合計が
// 多重計上になるため、Health Connect 自身の集計と同じく優先度で1つに絞る
// （ADR 0009）。order が空（優先度が読めていない）なら何もしない。
func dedupeByPriority(recs []model.AggRecord, order []string) []weighted {
	if len(order) == 0 || len(recs) == 0 {
		return unweighted(recs)
	}

	rank := make(map[string]int, len(order))
	for i, app := range order {
		rank[app] = i
	}
	rankOf := func(appID string) int {
		if r, ok := rank[appID]; ok {
			return r
		}
		// 優先度に載っていないアプリは最下位。順序を決めきれないと結果が
		// 実行ごとに変わるため、アプリIDで安定させる。
		return len(order)
	}

	byApp := make(map[string][]model.AggRecord)
	for _, rec := range recs {
		byApp[rec.AppID] = append(byApp[rec.AppID], rec)
	}
	apps := make([]string, 0, len(byApp))
	for app := range byApp {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool {
		if ri, rj := rankOf(apps[i]), rankOf(apps[j]); ri != rj {
			return ri < rj
		}
		return apps[i] < apps[j]
	})

	var covered []interval
	var kept []weighted
	for _, app := range apps {
		for _, rec := range byApp[app] {
			ratio := uncoveredRatio(covered, rec.StartTime, rec.EndTime)
			if ratio == 0 {
				continue
			}
			kept = append(kept, weighted{rec: rec, ratio: ratio})
		}
		// このアプリが「その日に記録していた範囲」を覆ったものとして扱う。
		// 歩数のように歩いた区間しかレコードが無い種別では、レコード単位で
		// 覆うと隙間が空き、そこへ下位アプリの同じ実測が入り込んで多重計上が残る。
		covered = mergeIntervals(append(covered, dailySpans(byApp[app])...))
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].rec.StartTime < kept[j].rec.StartTime })
	return kept
}

// dailySpans はアプリのレコードを「記録が始まった現地日」ごとにまとめ、その日の
// 最初から最後までを1つの区間として返す。
//
// 集計の日（LocalDate）ではなく開始時刻の日で区切る。睡眠のように集計を起床日へ
// 寄せる種別でも、覆う範囲は実際に記録された時間帯で考えるべきだから。
func dailySpans(recs []model.AggRecord) []interval {
	spans := make(map[string]interval, len(recs))
	for _, rec := range recs {
		date := startLocalDate(rec)
		span, ok := spans[date]
		if !ok {
			spans[date] = interval{start: rec.StartTime, end: rec.EndTime}
			continue
		}
		span.start = min(span.start, rec.StartTime)
		span.end = max(span.end, rec.EndTime)
		spans[date] = span
	}

	out := make([]interval, 0, len(spans))
	for _, span := range spans {
		out = append(out, span)
	}
	return out
}

func startLocalDate(rec model.AggRecord) string {
	return time.Unix(rec.StartTime/1000+int64(rec.ZoneOffset), 0).UTC().Format("2006-01-02")
}

func mergeIntervals(ivs []interval) []interval {
	if len(ivs) <= 1 {
		return ivs
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start < ivs[j].start })
	merged := ivs[:1]
	for _, iv := range ivs[1:] {
		last := &merged[len(merged)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
			continue
		}
		merged = append(merged, iv)
	}
	return merged
}

// uncoveredRatio は [start, end) のうち covered に含まれない割合を返す。
// covered はマージ済みで start 昇順であること。瞬間の記録（start == end）は
// その時刻を含む区間があれば 0、無ければ 1 を返す。
func uncoveredRatio(covered []interval, start, end int64) float64 {
	if start > end {
		start, end = end, start
	}
	if start == end {
		if pointCovered(covered, start) {
			return 0
		}
		return 1
	}

	var overlap int64
	i := sort.Search(len(covered), func(i int) bool { return covered[i].end > start })
	for ; i < len(covered) && covered[i].start < end; i++ {
		overlap += min(covered[i].end, end) - max(covered[i].start, start)
	}

	total := end - start
	if overlap >= total {
		return 0
	}
	return float64(total-overlap) / float64(total)
}

func pointCovered(covered []interval, t int64) bool {
	i := sort.Search(len(covered), func(i int) bool { return covered[i].end > t })
	return i < len(covered) && covered[i].start <= t
}
