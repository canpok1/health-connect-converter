// Package report は累積DBの問い合わせ結果から、スプレッドシートへ書き込む
// タブ（daily_summary / <種別>_raw / _meta）の行列を組み立てる。
package report

import (
	"context"
	"fmt"
	"sort"
	"time"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
)

// Querier は累積DBへの問い合わせ。internal/store が満たす。
type Querier interface {
	DailyAggregates(ctx context.Context, typeKey string, tc config.TypeConfig) ([]model.DailyRow, error)
	RecordsSince(ctx context.Context, typeKey string, tc config.TypeConfig, sinceMs int64) ([]model.Record, error)
	TypeStats(ctx context.Context, typeKey string) (model.TypeStats, error)
}

// タブ名。
const (
	DailySummaryTitle = "daily_summary"
	MetaTitle         = "_meta"
)

// durationValueName は config.TypeConfig.ValueNames() が IncludeDuration
// のときに末尾へ加える予約名と一致させる必要がある。
const durationValueName = "duration_min"

// dailyFuncOrder は daily_summary の列順（値名内での関数の並び）。
// config.TypeConfig.Daily に書かれた順序には従わず、常にこの順で出す。
var dailyFuncOrder = []string{"mean", "min", "max", "sum"}

// RawTabTitle は種別キーから生データタブ名を作る。
func RawTabTitle(typeKey string) string {
	return typeKey + "_raw"
}

// BuildDailySummary は全種別を横持ちにした daily_summary タブの行列を作る。
func BuildDailySummary(ctx context.Context, q Querier, cfg *config.Config) ([][]any, error) {
	type column struct {
		typeKey string
		key     string // DailyRow.Values のキー（"<値名>_<関数>" または "count"）
	}

	header := []any{"date"}
	var columns []column
	dateSet := make(map[string]bool)
	// typeKey -> date -> DailyRow.Values
	valuesByType := make(map[string]map[string]map[string]float64, len(cfg.Types))

	for _, tk := range cfg.TypeKeys() {
		tc := cfg.Types[tk]

		rows, err := q.DailyAggregates(ctx, tk, tc)
		if err != nil {
			return nil, fmt.Errorf("report: daily aggregates for %q: %w", tk, err)
		}
		dateMap := make(map[string]map[string]float64, len(rows))
		for _, r := range rows {
			dateMap[r.Date] = r.Values
			dateSet[r.Date] = true
		}
		valuesByType[tk] = dateMap

		daily := make(map[string]bool, len(tc.Daily))
		for _, fn := range tc.Daily {
			daily[fn] = true
		}

		for _, vn := range tc.ValueNames() {
			for _, fn := range dailyFuncOrder {
				if !daily[fn] {
					continue
				}
				header = append(header, fmt.Sprintf("%s_%s_%s", tk, vn, fn))
				columns = append(columns, column{typeKey: tk, key: vn + "_" + fn})
			}
		}
		if daily["count"] {
			header = append(header, tk+"_count")
			columns = append(columns, column{typeKey: tk, key: "count"})
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
func BuildRawTab(ctx context.Context, q Querier, cfg *config.Config, typeKey string, now time.Time) ([][]any, error) {
	tc, ok := cfg.Types[typeKey]
	if !ok {
		return nil, fmt.Errorf("report: unknown type %q", typeKey)
	}

	d, unlimited, err := tc.WindowDuration()
	if err != nil {
		return nil, fmt.Errorf("report: window for %q: %w", typeKey, err)
	}
	var sinceMs int64
	if !unlimited {
		sinceMs = now.Add(-d).UnixMilli()
	}

	records, err := q.RecordsSince(ctx, typeKey, tc, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("report: records since for %q: %w", typeKey, err)
	}

	valueNames := tc.ValueNames()
	header := make([]any, 0, 4+len(valueNames))
	header = append(header, "local_date", "local_start", "local_end", "app_id")
	for _, vn := range valueNames {
		header = append(header, vn)
	}

	out := make([][]any, 0, len(records)+1)
	out = append(out, header)
	for _, r := range records {
		localStart := localTime(r.StartTime, r.ZoneOffset)
		localEnd := localTime(r.EndTime, r.ZoneOffset)

		row := make([]any, 0, len(header))
		row = append(row,
			localStart.Format("2006-01-02"),
			localStart.Format("2006-01-02 15:04:05"),
			localEnd.Format("2006-01-02 15:04:05"),
			r.AppID,
		)
		for _, vn := range valueNames {
			if vn == durationValueName {
				row = append(row, float64(r.EndTime-r.StartTime)/60000.0)
				continue
			}
			if v, ok := r.Values[vn]; ok {
				row = append(row, v)
			} else {
				row = append(row, "")
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// _meta のキー名。
const (
	keyLastSuccessAt   = "last_success_at"
	keyZipModifiedTime = "last_processed_zip_modified_time"
	keyGeneratedAt     = "generated_at"
)

// BuildMeta は _meta タブの行列を作る。
func BuildMeta(ctx context.Context, q Querier, cfg *config.Config, lastSuccess, zipModified time.Time) ([][]any, error) {
	lastSuccessStr := formatRFC3339OrEmpty(lastSuccess)

	out := [][]any{
		{"key", "value"},
		{keyLastSuccessAt, lastSuccessStr},
		{keyZipModifiedTime, formatRFC3339OrEmpty(zipModified)},
		{keyGeneratedAt, lastSuccessStr},
	}

	for _, tk := range cfg.TypeKeys() {
		stats, err := q.TypeStats(ctx, tk)
		if err != nil {
			return nil, fmt.Errorf("report: type stats for %q: %w", tk, err)
		}

		latest := ""
		if stats.LatestStartTime != 0 {
			latest = time.Unix(0, stats.LatestStartTime*1e6).UTC().Format("2006-01-02 15:04:05Z")
		}

		out = append(out,
			[]any{tk + "_record_count", stats.Count},
			[]any{tk + "_latest_record_at", latest},
		)
	}

	return out, nil
}

func formatRFC3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// localTime はUTC epoch msとゾーンオフセット（秒）から現地時刻を作る。
// time.LoadLocation/time.Local はコンテナのtzdataに依存するため使わない。
func localTime(ms int64, zoneOffset int32) time.Time {
	return time.Unix(0, ms*1e6).UTC().Add(time.Duration(zoneOffset) * time.Second)
}
