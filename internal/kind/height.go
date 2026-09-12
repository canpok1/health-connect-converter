package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// heightRecord は身長の保存用モデル。エクスポートDBはメートルで持つので cm へ直して保存する。
type heightRecord struct {
	UUID       string
	Time       int64 // 測定時刻（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒）
	AppID      string
	CM         sql.NullFloat64
}

type heightKind struct{}

func (heightKind) Key() string { return "height" }

func (heightKind) Policy() Policy {
	return Policy{Window: WindowAll, Daily: []string{FuncMean, FuncCount}}
}

func (heightKind) Table() cumdb.Table {
	return envelopeTable("height", cumdb.Column{Name: "cm", Type: "REAL"})
}

func (k heightKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "height_record_table", []string{"height"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := heightRecord{UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID}
		if r.Values[0].Valid {
			rec.CM = sql.NullFloat64{Float64: r.Values[0].Float64 * 100, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.CM})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k heightKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]heightRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (heightRecord, error) {
		var rec heightRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.CM)
		return rec, err
	})
}

func (k heightKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.CM.Valid {
			values["cm"] = rec.CM.Float64
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
func (heightKind) ValueNames() []string { return []string{"cm"} }

func (k heightKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k heightKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.CM),
		})
	}
	return out, nil
}
