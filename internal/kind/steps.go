package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// stepsRecord は歩数の保存用モデル。単位変換は要らない。細切れの区間で記録されるため件数が多く、生データは直近30日に絞る。
type stepsRecord struct {
	UUID       string
	StartTime  int64 // 開始（UTC epoch ms）
	EndTime    int64 // 終了（UTC epoch ms）
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒。開始側を使う）
	AppID      string
	Count      sql.NullFloat64
}

type stepsKind struct{}

func (stepsKind) Key() string { return "steps" }

// Policy は重複排除を有効にする。同じ実測を複数のアプリが書くため、単純な合計は
// 多重計上になる（ADR 0009）。
func (stepsKind) Policy() Policy {
	return Policy{
		Window: "30d", Daily: []string{FuncSum},
		Dedupe: true, Category: CategoryActivity,
	}
}

func (stepsKind) Table() cumdb.Table {
	return envelopeTable("steps", cumdb.Column{Name: "count", Type: "REAL"})
}

func (k stepsKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInterval(ctx, exportDB, "steps_record_table", []string{"count"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := stepsRecord{
			UUID: r.UUID, StartTime: r.StartTime, EndTime: r.EndTime,
			ZoneOffset: r.ZoneOffset, AppID: r.AppID,
		}
		if r.Values[0].Valid {
			rec.Count = sql.NullFloat64{Float64: r.Values[0].Float64, Valid: true}
		}
		rows = append(rows, []any{rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID, rec.Count})
		dates = append(dates, localDate(rec.StartTime, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k stepsKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]stepsRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (stepsRecord, error) {
		var rec stepsRecord
		err := rows.Scan(&rec.UUID, &rec.StartTime, &rec.EndTime, &rec.ZoneOffset, &rec.AppID, &rec.Count)
		return rec, err
	})
}

func (k stepsKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		values := map[string]float64{}
		if rec.Count.Valid {
			values["count"] = rec.Count.Float64
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
func (stepsKind) ValueNames() []string { return []string{"count"} }

func (k stepsKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k stepsKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
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
			rec.AppID, nullable(rec.Count),
		})
	}
	return out, nil
}
