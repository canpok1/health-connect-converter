package kind

import (
	"context"
	"database/sql"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// exerciseSessionRecord は運動セッションの保存用モデル。数値の列は持たず、長さは
// 期間から求める。
//
// 種目（ExerciseType）は保存するが出力しない。実データは全件が同じ値で列にする
// 意味が無いが、壊れているわけではなく、散歩と筋トレを分けて記録するようになれば
// 意味を持つため残す（docs/domain-model.md）。
//
// メモ・タイトル・経路有無・主観的運動強度は取り込まない。前3つは空で、強度は
// 壊れ値（1.4e-45）しか入っていない。
type exerciseSessionRecord struct {
	UUID         string
	StartTime    int64 // 開始（UTC epoch ms）
	EndTime      int64 // 終了（UTC epoch ms）
	ZoneOffset   int32 // 記録時のタイムゾーンオフセット（秒。開始側を使う）
	AppID        string
	ExerciseType sql.NullFloat64 // Health Connect の運動種目（整数値）
}

type exerciseSessionKind struct{}

func (exerciseSessionKind) Key() string { return "exercise_session" }

// Policy は重複排除を有効にする。同じ運動を複数のアプリが書くため。
func (exerciseSessionKind) Policy() Policy {
	return Policy{
		Window: WindowAll, Daily: []string{FuncSum, FuncCount},
		Dedupe: true, Category: CategoryActivity,
	}
}

func (exerciseSessionKind) Table() cumdb.Table {
	return envelopeTable("exercise_session", cumdb.Column{Name: "exercise_type", Type: "REAL"})
}

func (k exerciseSessionKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadInterval(ctx, exportDB, "exercise_session_record_table", []string{"exercise_type"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := exerciseSessionRecord{
			UUID: r.UUID, StartTime: r.StartTime, EndTime: r.EndTime,
			ZoneOffset: r.ZoneOffset, AppID: r.AppID, ExerciseType: r.Values[0],
		}
		rows = append(rows, []any{rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID, rec.ExerciseType})
		dates = append(dates, localDate(rec.StartTime, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k exerciseSessionKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]exerciseSessionRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (exerciseSessionRecord, error) {
		var rec exerciseSessionRecord
		err := rows.Scan(&rec.UUID, &rec.StartTime, &rec.EndTime, &rec.ZoneOffset, &rec.AppID, &rec.ExerciseType)
		return rec, err
	})
}

func (rec exerciseSessionRecord) durationMin() float64 {
	return float64(rec.EndTime-rec.StartTime) / 60000.0
}

func (k exerciseSessionKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec.StartTime, rec.ZoneOffset),
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
func (exerciseSessionKind) ValueNames() []string { return []string{"duration_min"} }

func (k exerciseSessionKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k exerciseSessionKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
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
