package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// respiratoryRateRecord は呼吸数の保存用モデル。単位変換は要らない。
type respiratoryRateRecord struct {
	UUID          string
	Time          int64 // 測定時刻（UTC epoch ms）
	ZoneOffset    int32 // 記録時のタイムゾーンオフセット（秒）
	AppID         string
	BreathsPerMin sql.NullFloat64
}

type respiratoryRateKind struct{}

func (respiratoryRateKind) Key() string { return "respiratory_rate" }

func (respiratoryRateKind) Policy() Policy {
	return Policy{Window: WindowAll, Daily: []string{FuncMean, FuncCount}}
}

func (respiratoryRateKind) Table() cumdb.Table {
	return envelopeTable("respiratory_rate", cumdb.Column{Name: "breaths_per_min", Type: "REAL"})
}

func (k respiratoryRateKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "respiratory_rate_record_table", []string{"rate"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := respiratoryRateRecord{UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID}
		if r.Values[0].Valid {
			rec.BreathsPerMin = sql.NullFloat64{Float64: r.Values[0].Float64, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.BreathsPerMin})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k respiratoryRateKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]respiratoryRateRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (respiratoryRateRecord, error) {
		var rec respiratoryRateRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.BreathsPerMin)
		return rec, err
	})
}

func (k respiratoryRateKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.BreathsPerMin.Valid {
			values["breaths_per_min"] = rec.BreathsPerMin.Float64
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
func (respiratoryRateKind) ValueNames() []string { return []string{"breaths_per_min"} }

func (k respiratoryRateKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k respiratoryRateKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.BreathsPerMin),
		})
	}
	return out, nil
}
