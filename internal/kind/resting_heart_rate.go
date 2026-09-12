package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// restingHeartRateRecord は安静時心拍の保存用モデル。単位変換は要らない。
type restingHeartRateRecord struct {
	UUID       string
	Time       int64 // 測定時刻（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒）
	AppID      string
	BPM        sql.NullFloat64
}

type restingHeartRateKind struct{}

func (restingHeartRateKind) Key() string { return "resting_heart_rate" }

func (restingHeartRateKind) Policy() Policy {
	return Policy{Window: WindowAll, Daily: []string{FuncMean, FuncMin, FuncMax, FuncCount}}
}

func (restingHeartRateKind) Table() cumdb.Table {
	return envelopeTable("resting_heart_rate", cumdb.Column{Name: "bpm", Type: "REAL"})
}

func (k restingHeartRateKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "resting_heart_rate_record_table", []string{"beats_per_minute"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := restingHeartRateRecord{UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID}
		if r.Values[0].Valid {
			rec.BPM = sql.NullFloat64{Float64: r.Values[0].Float64, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.BPM})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k restingHeartRateKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]restingHeartRateRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (restingHeartRateRecord, error) {
		var rec restingHeartRateRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.BPM)
		return rec, err
	})
}

func (k restingHeartRateKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.BPM.Valid {
			values["bpm"] = rec.BPM.Float64
		}
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec.Time, rec.ZoneOffset),
			StartTime:  rec.Time,
			EndTime:    rec.Time,
			ZoneOffset: rec.ZoneOffset,
			AppID:      rec.AppID,
			Values:     values,
		})
	}
	return out, nil
}

// ValueNames は daily_summary に出す値名（辞書順）。
func (restingHeartRateKind) ValueNames() []string { return []string{"bpm"} }

func (k restingHeartRateKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k restingHeartRateKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.BPM),
		})
	}
	return out, nil
}
