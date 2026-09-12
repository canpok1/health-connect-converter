package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// totalCaloriesBurnedRecord は消費エネルギーの保存用モデル。エクスポートDBはカロリーで持つので kcal へ直して保存する。
type totalCaloriesBurnedRecord struct {
	UUID       string
	StartTime  int64 // 開始（UTC epoch ms）
	EndTime    int64 // 終了（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒。開始側を使う）
	AppID      string
	Kcal       sql.NullFloat64
}

type totalCaloriesBurnedKind struct{}

func (totalCaloriesBurnedKind) Key() string { return "total_calories_burned" }

// Policy は重複排除を有効にする。同じ実測を複数のアプリが書くため、単純な合計は
// 多重計上になる（ADR 0009）。
func (totalCaloriesBurnedKind) Policy() Policy {
	return Policy{
		Window: WindowAll, Daily: []string{FuncSum},
		Dedupe: true, Category: CategoryActivity,
	}
}

func (totalCaloriesBurnedKind) Table() cumdb.Table {
	return envelopeTable("total_calories_burned", cumdb.Column{Name: "kcal", Type: "REAL"})
}

func (k totalCaloriesBurnedKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInterval(ctx, exportDB, "total_calories_burned_record_table", []string{"energy"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := totalCaloriesBurnedRecord{
			UUID: r.UUID, StartTime: r.StartTime, EndTime: r.EndTime,
			ZoneOffset: r.ZoneOffset, AppID: r.AppID,
		}
		if r.Values[0].Valid {
			rec.Kcal = sql.NullFloat64{Float64: r.Values[0].Float64 * 0.001, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID, rec.Kcal})
		dates = append(dates, localDate(rec.StartTime, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k totalCaloriesBurnedKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]totalCaloriesBurnedRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (totalCaloriesBurnedRecord, error) {
		var rec totalCaloriesBurnedRecord
		err := rows.Scan(&rec.UUID, &rec.StartTime, &rec.EndTime, &rec.ZoneOffset, &rec.AppID, &rec.Kcal)
		return rec, err
	})
}

func (k totalCaloriesBurnedKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.Kcal.Valid {
			values["kcal"] = rec.Kcal.Float64
		}
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec.StartTime, rec.ZoneOffset),
			StartTime:  rec.StartTime,
			EndTime:    rec.EndTime,
			ZoneOffset: rec.ZoneOffset,
			AppID:      rec.AppID,
			Values:     values,
		})
	}
	return out, nil
}

// ValueNames は daily_summary に出す値名（辞書順）。
func (totalCaloriesBurnedKind) ValueNames() []string { return []string{"kcal"} }

func (k totalCaloriesBurnedKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k totalCaloriesBurnedKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
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
			rec.AppID, nullable(rec.Kcal),
		})
	}
	return out, nil
}
