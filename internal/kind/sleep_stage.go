package kind

import (
	"context"
	"database/sql"
	"strconv"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/hcsql"
	"health-connect-converter/internal/model"
)

// 睡眠ステージの種別。値は Android の SleepSessionRecord.StageType の定数
// （AOSP の framework/java/android/health/connect/datatypes/SleepSessionRecord.java で確認）。
// 出力するのはこの4つだけで、他（0=不明・2=睡眠・3=ベッド外の覚醒・7=ベッド内の覚醒）は捨てる。
const (
	stageAwake = 1
	stageLight = 4
	stageDeep  = 5
	stageREM   = 6
)

// stageValueNames はステージ種別から値名への対応。ここに無い種別は取り込まない。
var stageValueNames = map[int64]string{
	stageAwake: "awake_min",
	stageLight: "light_min",
	stageDeep:  "deep_min",
	stageREM:   "rem_min",
}

// stageNames は生データタブに出すステージの名前。
var stageNames = map[int64]string{
	stageAwake: "awake",
	stageLight: "light",
	stageDeep:  "deep",
	stageREM:   "rem",
}

// sleepStageRecord は睡眠ステージ1区間の保存用モデル。
//
// エクスポートDBでは「ひと晩のセッション（親）」と「区間（子）」の親子2表に
// 分かれている。保存したい形は区間1つずつなので、読むときに結合して畳む。
// 子に uuid は無いため、親の uuid と区間の開始時刻を "#" で繋いで一意キーにする。
//
// **親セッションの終了時刻（起床時刻）も持つ。** 日次集計をどの日に数えるかは
// これで決める。区間自身の時刻で数えると、日付をまたぐひと晩が2日に割れ、
// 起床日で数えている睡眠時間と食い違う（ADR 0011）。
type sleepStageRecord struct {
	UUID string // <親のuuid>#<区間の開始時刻>
	// StartTime / EndTime は区間自身の時刻（UTC epoch ms）。
	StartTime int64
	EndTime   int64
	// ParentEndTime は親セッションの終了時刻（起床時刻。UTC epoch ms）。
	ParentEndTime int64
	ZoneOffset    int32 // 記録時のタイムゾーンオフセット（秒。親の開始側を使う）
	AppID         string
	StageType     int64
}

// durationMin は区間の長さ（分）。
func (rec sleepStageRecord) durationMin() float64 {
	return float64(rec.EndTime-rec.StartTime) / 60000.0
}

type sleepStageKind struct{}

func (sleepStageKind) Key() string { return "sleep_stage" }

// Policy は重複排除を使わない。ステージを書いているのは現状 Fitbit だけで、かつ
// ひと晩ぶんの区間はすべて同じ親に属するため、時間帯の覆いで判定する既存の
// 重複排除と噛み合わない（ADR 0011）。
//
// 窓は30日ぶん。1泊あたり約86区間で、30日で約2,600行になる。
func (sleepStageKind) Policy() Policy {
	return Policy{Window: "30d", Daily: []string{FuncSum, FuncCount}}
}

// Table は日ごと置き換えの基準を**親セッションの終了日**にする。ひと晩を
// 1単位として置き換えるため（区間の開始日で区切ると同じ夜が2日に割れる）。
func (sleepStageKind) Table() cumdb.Table {
	return cumdb.Table{
		Name: "record_sleep_stage",
		Columns: []cumdb.Column{
			{Name: "uuid", Type: "TEXT"},
			{Name: "start_time", Type: "INTEGER"},
			{Name: "end_time", Type: "INTEGER"},
			{Name: "zone_offset", Type: "INTEGER"},
			{Name: "app_id", Type: "TEXT"},
			{Name: "parent_end_time", Type: "INTEGER"},
			{Name: "stage_type", Type: "INTEGER"},
		},
		TimeColumn: "start_time",
		DateExpr:   cumdb.LocalDateExpr("parent_end_time", "zone_offset"),
	}
}

func (k sleepStageKind) ExportRows(ctx context.Context, exportDB *sql.DB) ([][]any, []string, error) {
	read, err := hcsql.ReadSegment(ctx, exportDB,
		"sleep_session_record_table", "sleep_stages_table",
		"stage_start_time", "stage_end_time", "stage_type")
	if err != nil {
		return nil, nil, err
	}

	rows := make([][]any, 0, len(read))
	dates := make([]string, 0, len(read))
	for _, r := range read {
		if _, ok := stageValueNames[r.Type]; !ok {
			// 対応表に無いステージ種別は取り込まない。意味の違う覚醒や不明を
			// 1つの列へ混ぜないため。落ちたことは件数と睡眠時間との差で気付ける。
			continue
		}
		rec := sleepStageRecord{
			UUID:          r.ParentUUID + "#" + strconv.FormatInt(r.StartTime, 10),
			StartTime:     r.StartTime,
			EndTime:       r.EndTime,
			ParentEndTime: r.ParentEnd,
			ZoneOffset:    r.ZoneOffset,
			AppID:         r.AppID,
			StageType:     r.Type,
		}
		rows = append(rows, []any{
			rec.UUID, rec.StartTime, rec.EndTime, rec.ZoneOffset, rec.AppID,
			rec.ParentEndTime, rec.StageType,
		})
		dates = append(dates, localDate(rec.ParentEndTime, rec.ZoneOffset))
	}
	return rows, uniqueDates(dates), nil
}

func (k sleepStageKind) load(ctx context.Context, cum *sql.DB, sinceMs int64) ([]sleepStageRecord, error) {
	return loadAll(ctx, cum, k.Table(), sinceMs, func(rows *sql.Rows) (sleepStageRecord, error) {
		var rec sleepStageRecord
		err := rows.Scan(&rec.UUID, &rec.StartTime, &rec.EndTime, &rec.ZoneOffset, &rec.AppID,
			&rec.ParentEndTime, &rec.StageType)
		return rec, err
	})
}

// Aggregate は**起床日**に数え、区間の長さをステージ種別ごとの値へ振り分ける。
func (k sleepStageKind) Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error) {
	recs, err := k.load(ctx, cum, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.AggRecord, 0, len(recs))
	for _, rec := range recs {
		name, ok := stageValueNames[rec.StageType]
		if !ok {
			continue
		}
		out = append(out, model.AggRecord{
			LocalDate:  localDate(rec.ParentEndTime, rec.ZoneOffset),
			StartTime:  rec.StartTime,
			EndTime:    rec.EndTime,
			ZoneOffset: rec.ZoneOffset,
			AppID:      rec.AppID,
			Values:     map[string]float64{name: rec.durationMin()},
		})
	}
	return out, nil
}

// ValueNames は daily_summary に出す値名（辞書順）。
func (sleepStageKind) ValueNames() []string {
	return []string{"awake_min", "deep_min", "light_min", "rem_min"}
}

// RawHeader は生データタブのヘッダ。ステージは名前で出し、長さは1列にまとめる
// （種別ごとに4列へ散らすより、ひと晩の推移が読みやすい）。
func (sleepStageKind) RawHeader() []any {
	return rawHeader("stage", "minutes")
}

func (k sleepStageKind) RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error) {
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
			rec.AppID, stageNames[rec.StageType], rec.durationMin(),
		})
	}
	return out, nil
}
