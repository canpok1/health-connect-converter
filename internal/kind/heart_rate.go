package kind

import (
	"context"
	"database/sql"
	"strconv"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// heartRateRecord は心拍の保存用モデル。**1行＝1測定値**にする。
//
// エクスポートDBでは「期間の記録（値を持たない親）」と「測定値の並び（子）」の
// 親子2表に分かれているが、保存したい形は測定値1つずつなので、読むときに結合して
// 畳む（ADR 0012）。子に uuid は無いため、親の uuid と測定時刻を "#" で繋いで
// 一意キーにする。
type heartRateRecord struct {
	UUID       string // <親のuuid>#<測定時刻>
	Time       int64  // 測定時刻（UTC epoch ms）
	ZoneOffset int32  // 記録時のタイムゾーンオフセット（秒。親の開始側を使う）
	AppID      string
	BPM        sql.NullFloat64
}

type heartRateKind struct{}

func (heartRateKind) Key() string { return "heart_rate" }

// Policy の窓は1日ぶんだけ。常時計測のウェアラブルが入ると1日約36,000件になり、
// 生データタブが数十日でシートのセル上限に達する。日次集計は窓に関わらず全期間残る。
func (heartRateKind) Policy() Policy {
	return Policy{Window: "1d", Daily: []string{FuncMean, FuncMin, FuncMax, FuncCount}}
}

func (heartRateKind) Table() cumdb.Table {
	return envelopeTable("heart_rate", cumdb.Column{Name: "bpm", Type: "REAL"})
}

func (k heartRateKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadSeries(ctx, exportDB, "heart_rate_record_table", "heart_rate_record_series_table", []string{"beats_per_minute"})
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		rec := heartRateRecord{
			UUID:       r.ParentUUID + "#" + strconv.FormatInt(r.EpochMillis, 10),
			Time:       r.EpochMillis,
			ZoneOffset: r.ZoneOffset,
			AppID:      r.AppID,
			BPM:        r.Values[0],
		}
		rows = append(rows, []any{rec.UUID, rec.Time, rec.Time, rec.ZoneOffset, rec.AppID, rec.BPM})
		dates = append(dates, localDate(rec.Time, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k heartRateKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]heartRateRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (heartRateRecord, error) {
		var rec heartRateRecord
		var endTime int64
		err := rows.Scan(&rec.UUID, &rec.Time, &endTime, &rec.ZoneOffset, &rec.AppID, &rec.BPM)
		return rec, err
	})
}

func (k heartRateKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
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
func (heartRateKind) ValueNames() []string { return []string{"bpm"} }

func (k heartRateKind) RawHeader() []any { return rawHeader(k.ValueNames()...) }

func (k heartRateKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
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
