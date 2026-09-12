package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// basalMetabolicRateRecord は基礎代謝の保存用モデル。エクスポートDBはワットで持つので kcal/日 へ直して保存する（86400秒 ÷ 4184ジュール ≈ 20.65）。
type basalMetabolicRateRecord struct {
	UUID       string
	Time       int64 // 測定時刻（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒）
	AppID      string
	KcalPerDay sql.NullFloat64
}

type basalMetabolicRateKind struct{}

func (basalMetabolicRateKind) Key() string { return "basal_metabolic_rate" }

func (basalMetabolicRateKind) Policy() Policy {
	return Policy{Window: WindowAll, Daily: []string{FuncMean, FuncCount}}
}

func (basalMetabolicRateKind) Table() cumdb.Table {
	return envelopeTable("basal_metabolic_rate", cumdb.Column{Name: "kcal_per_day", Type: "REAL"})
}

func (k basalMetabolicRateKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "basal_metabolic_rate_record_table", []string{"basal_metabolic_rate"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := basalMetabolicRateRecord{UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID}
		if r.Values[0].Valid {
			rec.KcalPerDay = sql.NullFloat64{Float64: r.Values[0].Float64 * 20.65, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.KcalPerDay})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k basalMetabolicRateKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]basalMetabolicRateRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (basalMetabolicRateRecord, error) {
		var rec basalMetabolicRateRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.KcalPerDay)
		return rec, err
	})
}

func (k basalMetabolicRateKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.KcalPerDay.Valid {
			values["kcal_per_day"] = rec.KcalPerDay.Float64
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
func (basalMetabolicRateKind) ValueNames() []string { return []string{"kcal_per_day"} }

func (k basalMetabolicRateKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k basalMetabolicRateKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.KcalPerDay),
		})
	}
	return out, nil
}
