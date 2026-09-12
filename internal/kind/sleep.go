package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// sleepRecord はひと晩の睡眠（セッション）の保存用モデル。数値の列は持たず、
// 長さは期間から求める。
//
// エクスポートDBはメモ（notes）とタイトル（title）も持つが取り込まない。実データは
// メモが全件空、タイトルはアプリが付けた「睡眠分析」のみで情報量が無い
// （docs/domain-model.md）。
type sleepRecord struct {
	UUID       string
	StartTime  int64 // 就寝（UTC epoch ms）
	EndTime    int64 // 起床（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒。開始側を使う）
	AppID      string
}

type sleepKind struct{}

func (sleepKind) Key() string { return "sleep" }

// Policy は重複排除を有効にする。同じひと晩を複数の睡眠アプリが書くため。
func (sleepKind) Policy() Policy {
	return Policy{
		Window: WindowAll, Daily: []string{FuncSum, FuncCount},
		Dedupe: true, Category: CategorySleep,
	}
}

func (sleepKind) Table() cumdb.Table { return envelopeTable("sleep") }

func (k sleepKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInterval(ctx, exportDB, "sleep_session_record_table", nil)
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := sleepRecord{
			UUID: r.UUID, StartTime: r.StartTime, EndTime: r.EndTime,
			ZoneOffset: r.ZoneOffset, AppID: r.AppID,
		}
		rows = append(rows, []any{rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID})
		// 日ごと置き換えの基準は記録が始まった日（累積DBの既存の扱いに合わせる）。
		dates = append(dates, localDate(rec.StartTime, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k sleepKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]sleepRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (sleepRecord, error) {
		var rec sleepRecord
		err := rows.Scan(&rec.UUID, &rec.StartTime, &rec.EndTime, &rec.ZoneOffset, &rec.AppID)
		return rec, err
	})
}

// durationMin は睡眠時間（分）。
func (rec sleepRecord) durationMin() float64 {
	return float64(rec.EndTime-rec.StartTime) / 60000.0
}

// Aggregate は**起床日**に数える。日付が変わってから寝た日に前夜ぶんと当夜ぶんが
// 合算されるのを防ぐため（ADR 0009）。
func (k sleepKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec.EndTime, rec.ZoneOffset),
			StartTime:  rec.StartTime,
			EndTime:    rec.EndTime,
			ZoneOffset: rec.ZoneOffset,
			AppID:      rec.AppID,
			Values:     map[string]float64{"duration_min": rec.durationMin()},
		})
	}
	return out, nil
}

// ValueNames は daily_summary に出す値名（辞書順）。
func (sleepKind) ValueNames() []string { return []string{"duration_min"} }

func (k sleepKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k sleepKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		start := localTime(rec.StartTime, rec.ZoneOffset)
		end := localTime(rec.EndTime, rec.ZoneOffset)
		out = append(out, []any{
			start.Format("2006-01-02"), start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05"),
			rec.AppID, rec.durationMin(),
		})
	}
	return out, nil
}
