package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// bloodPressureRecord は血圧の保存用モデル。単位はどちらも mmHg で変換は要らない。
//
// エクスポートDBは測定部位（measurement_location）と体位（body_position）も持つが
// 取り込まない。実データは全件が未設定（"0"）で情報量がゼロのため（docs/domain-model.md）。
type bloodPressureRecord struct {
	UUID       string
	Time       int64 // 測定時刻（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒）
	AppID      string
	Diastolic  sql.NullFloat64 // 拡張期
	Systolic   sql.NullFloat64 // 収縮期
}

type bloodPressureKind struct{}

func (bloodPressureKind) Key() string { return "blood_pressure" }

func (bloodPressureKind) Policy() Policy {
	return Policy{Window: WindowAll, Daily: []string{FuncMean, FuncMin, FuncMax, FuncCount}}
}

func (bloodPressureKind) Table() cumdb.Table {
	return envelopeTable("blood_pressure",
		cumdb.Column{Name: "diastolic", Type: "REAL"},
		cumdb.Column{Name: "systolic", Type: "REAL"},
	)
}

func (k bloodPressureKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInstant(ctx, exportDB, "blood_pressure_record_table", []string{"diastolic", "systolic"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := bloodPressureRecord{
			UUID: r.UUID, Time: r.Time, ZoneOffset: r.ZoneOffset, AppID: r.AppID,
			Diastolic: r.Values[0], Systolic: r.Values[1],
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.Diastolic, rec.Systolic})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k bloodPressureKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]bloodPressureRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (bloodPressureRecord, error) {
		var rec bloodPressureRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.Diastolic, &rec.Systolic)
		return rec, err
	})
}

func (k bloodPressureKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.Diastolic.Valid {
			values["diastolic"] = rec.Diastolic.Float64
		}
		if rec.Systolic.Valid {
			values["systolic"] = rec.Systolic.Float64
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
func (bloodPressureKind) ValueNames() []string { return []string{"diastolic", "systolic"} }

func (k bloodPressureKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k bloodPressureKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
	recs, err := k.load(ctx, cum, sinceMs)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(recs))
	for _, rec := range recs {
		t := localTime(rec.Time, rec.ZoneOffset)
		out = append(out, []any{
			t.Format("2006-01-02"), t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02 15:04:05"),
			rec.AppID, nullable(rec.Diastolic), nullable(rec.Systolic),
		})
	}
	return out, nil
}
