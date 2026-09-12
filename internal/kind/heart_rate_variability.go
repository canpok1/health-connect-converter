package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// heartRateVariabilityRecord は心拍変動（RMSSD）の保存用モデル。単位変換は要らない。Fitbit が睡眠中に5分間隔で書くため件数が多く、生データは直近30日に絞る。
type heartRateVariabilityRecord struct {
	UUID        string
	Time        int64 // 測定時刻（UTC epoch ms）
	ZoneOffset  int32 // 記録時のタイムゾーンオフセット（秒）
	AppID       string
	RMSSDMillis sql.NullFloat64
}

type heartRateVariabilityKind struct{}

func (heartRateVariabilityKind) Key() string { return "heart_rate_variability" }

func (heartRateVariabilityKind) Policy() Policy {
	return Policy{Window: "30d", Daily: []string{FuncMean, FuncMin, FuncMax, FuncCount}}
}

func (heartRateVariabilityKind) Table() cumdb.Table {
	return envelopeTable("heart_rate_variability", cumdb.Column{Name: "rmssd_ms", Type: "REAL"})
}

func (k heartRateVariabilityKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "heart_rate_variability_rmssd_record_table", []string{"heart_rate_variability_millis"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := heartRateVariabilityRecord{UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID}
		if r.Values[0].Valid {
			rec.RMSSDMillis = sql.NullFloat64{Float64: r.Values[0].Float64, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.RMSSDMillis})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k heartRateVariabilityKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]heartRateVariabilityRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (heartRateVariabilityRecord, error) {
		var rec heartRateVariabilityRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.RMSSDMillis)
		return rec, err
	})
}

func (k heartRateVariabilityKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.RMSSDMillis.Valid {
			values["rmssd_ms"] = rec.RMSSDMillis.Float64
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
func (heartRateVariabilityKind) ValueNames() []string { return []string{"rmssd_ms"} }

func (k heartRateVariabilityKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k heartRateVariabilityKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.RMSSDMillis),
		})
	}
	return out, nil
}
