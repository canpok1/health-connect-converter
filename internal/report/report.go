// Package report は累積DBの問い合わせ結果から、スプレッドシートへ書き込む
// タブ（daily_summary / <種別>_raw / _meta）の行列を組み立てる。
package report

import (
	"context"
	"fmt"
	"sort"
	"time"

	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// Querier は累積DBへの問い合わせ。internal/store が満たす。
type Querier interface {
	DailyAggregates(ctx context.Context, k kind.Kind) ([]model.DailyRow, error)
	RawRows(ctx context.Context, k kind.Kind, sinceMs int64) ([][]any, error)
	Stats(ctx context.Context, k kind.Kind) (model.TypeStats, error)
}

// タブ名。
const (
	DailySummaryTitle = "daily_summary"
	MetaTitle         = "_meta"
)

// dailyFuncOrder は daily_summary の列順（値名内での関数の並び）。
// 種別が並べた順序には従わず、常にこの順で出す。
var dailyFuncOrder = []string{kind.FuncMean, kind.FuncMin, kind.FuncMax, kind.FuncSum}

// RawTabTitle は種別キーから生データタブ名を作る。
func RawTabTitle(typeKey string) string {
	return typeKey + "_raw"
}

// BuildDailySummary は全種別を横持ちにした daily_summary タブの行列を作る。
func BuildDailySummary(ctx context.Context, q Querier, kinds []kind.Kind) ([][]any, error) {
	type column struct {
		typeKey string
		key     string // DailyRow.Values のキー（"<値名>_<関数>" または "count"）
	}

	header := []any{"date"}
	var columns []column
	dateSet := make(map[string]bool)
	// 種別キー -> 日 -> DailyRow.Values
	valuesByType := make(map[string]map[string]map[string]float64, len(kinds))

	for _, k := range kinds {
		rows, err := q.DailyAggregates(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("report: daily aggregates for %q: %w", k.Key(), err)
		}
		dateMap := make(map[string]map[string]float64, len(rows))
		for _, r := range rows {
			dateMap[r.Date] = r.Values
			dateSet[r.Date] = true
		}
		valuesByType[k.Key()] = dateMap

		policy := k.Policy()
		daily := make(map[string]bool, len(policy.Daily))
		for _, fn := range policy.Daily {
			daily[fn] = true
		}

		for _, vn := range k.ValueNames() {
			for _, fn := range dailyFuncOrder {
				if !daily[fn] {
					continue
				}
				header = append(header, fmt.Sprintf("%s_%s_%s", k.Key(), vn, fn))
				columns = append(columns, column{typeKey: k.Key(), key: vn + "_" + fn})
			}
		}
		if daily[kind.FuncCount] {
			header = append(header, k.Key()+"_count")
			columns = append(columns, column{typeKey: k.Key(), key: kind.FuncCount})
		}
	}

	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	out := make([][]any, 0, len(dates)+1)
	out = append(out, header)
	for _, d := range dates {
		row := make([]any, 0, len(header))
		row = append(row, d)
		for _, c := range columns {
			var cell any = ""
			if values, ok := valuesByType[c.typeKey][d]; ok {
				if v, ok := values[c.key]; ok {
					cell = v
				}
			}
			row = append(row, cell)
		}
		out = append(out, row)
	}
	return out, nil
}

// BuildRawTab は種別1つぶんの生データタブの行列を作る。
func BuildRawTab(ctx context.Context, q Querier, k kind.Kind, now time.Time) ([][]any, error) {
	d, unlimited, err := k.Policy().WindowDuration()
	if err != nil {
		return nil, fmt.Errorf("report: window for %q: %w", k.Key(), err)
	}
	var sinceMs int64
	if !unlimited {
		sinceMs = now.Add(-d).UnixMilli()
	}

	rows, err := q.RawRows(ctx, k, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("report: raw rows for %q: %w", k.Key(), err)
	}

	out := make([][]any, 0, len(rows)+1)
	out = append(out, k.RawHeader())
	out = append(out, rows...)
	return out, nil
}

// _meta のキー名。
const (
	keyLastSuccessAt   = "last_success_at"
	keyZipModifiedTime = "last_processed_zip_modified_time"
	keyGeneratedAt     = "generated_at"
)

// BuildMeta は _meta タブの行列を作る。
//
// tableRows はエクスポートDB内の全テーブルの行数（テーブル名がキー）。累積DB由来の
// <種別>_record_count と混ざらないよう export_<テーブル名>_rows として末尾へ出す。
func BuildMeta(
	ctx context.Context,
	q Querier,
	kinds []kind.Kind,
	lastSuccess, zipModified time.Time,
	tableRows map[string]int64,
) ([][]any, error) {
	lastSuccessStr := formatRFC3339OrEmpty(lastSuccess)

	out := [][]any{
		{"key", "value"},
		{keyLastSuccessAt, lastSuccessStr},
		{keyZipModifiedTime, formatRFC3339OrEmpty(zipModified)},
		{keyGeneratedAt, lastSuccessStr},
	}

	for _, k := range kinds {
		stats, err := q.Stats(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("report: stats for %q: %w", k.Key(), err)
		}

		latest := ""
		if stats.LatestStartTime != 0 {
			latest = time.Unix(0, stats.LatestStartTime*1e6).UTC().Format("2006-01-02 15:04:05Z")
		}

		out = append(out,
			[]any{k.Key() + "_record_count", stats.Count},
			[]any{k.Key() + "_latest_record_at", latest},
		)
	}

	tableNames := make([]string, 0, len(tableRows))
	for name := range tableRows {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)
	for _, name := range tableNames {
		out = append(out, []any{"export_" + name + "_rows", tableRows[name]})
	}

	return out, nil
}

func formatRFC3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
