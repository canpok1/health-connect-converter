package e2e

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// jst は現地時刻（JST）から UTC epoch ms を作る。エクスポートDBの時刻はこの形。
func jst(y int, m time.Month, d, hh, mm int) int64 {
	return time.Date(y, m, d, hh, mm, 0, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
}

const zoneOffsetJST = 32400 // 秒

// uuidSeq は決定的な16バイトUUIDを順番に作る。期待値ファイルを安定させるため
// ランダムにしない。
type uuidSeq struct{ n byte }

func (u *uuidSeq) next() []byte {
	u.n++
	b := make([]byte, 16)
	for i := range b {
		b[i] = u.n
	}
	return b
}

// newExportFixture は実物のエクスポートDBを模した合成DBを作る。
//
// 目的は出力（daily_summary / 生データ / _meta）の期待値を固定すること。
// 実データの性質のうち出力に効くものを意図的に含める。
//   - 単位変換が要る値（体重のグラム、消費エネルギーのカロリー、基礎代謝のワット、身長のメートル）
//   - 日付をまたぐ睡眠（起床日に数える）
//   - 同じ時間帯を複数アプリが書いた歩数（アプリ優先度で重複排除する）
//   - 親子2表で持つ種別（心拍・速度）と、親テーブル名が CamelCase の種別（速度）
//   - config に登録が無いテーブル（_meta の行数一覧に出る）
func newExportFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}

	instant := func(table string, valueCols ...string) {
		cols := ""
		for _, c := range valueCols {
			cols += fmt.Sprintf(", %s REAL", c)
		}
		exec(fmt.Sprintf(`CREATE TABLE %s (
			row_id INTEGER PRIMARY KEY AUTOINCREMENT, uuid BLOB, time INTEGER,
			zone_offset INTEGER, app_info_id INTEGER%s)`, table, cols))
	}
	interval := func(table string, valueCols ...string) {
		cols := ""
		for _, c := range valueCols {
			cols += fmt.Sprintf(", %s REAL", c)
		}
		exec(fmt.Sprintf(`CREATE TABLE %s (
			row_id INTEGER PRIMARY KEY AUTOINCREMENT, uuid BLOB,
			start_time INTEGER, start_zone_offset INTEGER,
			end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER%s)`, table, cols))
	}

	exec(`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`)
	exec(`CREATE TABLE health_data_category_priority_table (
		row_id INTEGER PRIMARY KEY, health_data_category INTEGER UNIQUE, app_id_priority_order TEXT NOT NULL)`)
	// config に登録が無いテーブル。_meta の行数一覧に出ることを確かめる。
	exec(`CREATE TABLE mindfulness_session_record_table (row_id INTEGER PRIMARY KEY, v INTEGER)`)

	instant("blood_pressure_record_table", "systolic", "diastolic")
	instant("weight_record_table", "weight")
	instant("body_fat_record_table", "percentage")
	instant("basal_metabolic_rate_record_table", "basal_metabolic_rate")
	instant("height_record_table", "height")
	instant("resting_heart_rate_record_table", "beats_per_minute")
	instant("heart_rate_variability_rmssd_record_table", "heart_rate_variability_millis")
	instant("respiratory_rate_record_table", "rate")
	instant("oxygen_saturation_record_table", "percentage")

	interval("steps_record_table", "count")
	interval("distance_record_table", "distance")
	interval("total_calories_burned_record_table", "energy")
	interval("sleep_session_record_table")
	// 運動は種目（exercise_type）も持つ。保存はするが出力しない。
	interval("exercise_session_record_table", "exercise_type")

	// 睡眠ステージ（親は睡眠セッション）。
	exec(`CREATE TABLE sleep_stages_table (
		parent_key INTEGER NOT NULL, stage_start_time INTEGER NOT NULL,
		stage_end_time INTEGER NOT NULL, stage_type INTEGER NOT NULL)`)

	// 親子2表の種別。親は値列を持たない。
	interval("heart_rate_record_table")
	exec(`CREATE TABLE heart_rate_record_series_table (
		parent_key INTEGER, epoch_millis INTEGER, beats_per_minute INTEGER)`)
	interval("SpeedRecordTable")
	exec(`CREATE TABLE speed_record_table (
		parent_key INTEGER, epoch_millis INTEGER, speed REAL)`)

	// アプリ。歩数は fitness（優先）と pepup（下位）の2つが同じ時間帯を書く。
	exec(`INSERT INTO application_info_table (row_id, package_name) VALUES
		(1, 'com.google.android.apps.fitness'),
		(2, 'jp.co.omron.healthcare.omron_connect'),
		(3, 'life.pepup.app'),
		(4, 'com.fitbit.FitbitMobile')`)
	// 1=activity, 5=sleep, 6=vitals（config.Categories の値）
	exec(`INSERT INTO health_data_category_priority_table (row_id, health_data_category, app_id_priority_order) VALUES
		(1, 1, '1,3'), (2, 5, '4,1'), (3, 6, '2,4')`)

	u := &uuidSeq{}

	instantRow := func(table, valueCols string, tm int64, appID int, values ...any) {
		t.Helper()
		cols := "uuid, time, zone_offset, app_info_id, " + valueCols
		ph := "?, ?, ?, ?"
		for range values {
			ph += ", ?"
		}
		args := append([]any{u.next(), tm, zoneOffsetJST, appID}, values...)
		exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, ph), args...)
	}
	intervalRow := func(table, valueCols string, start, end int64, appID int, values ...any) {
		t.Helper()
		cols := "uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id"
		ph := "?, ?, ?, ?, ?, ?"
		if valueCols != "" {
			cols += ", " + valueCols
		}
		for range values {
			ph += ", ?"
		}
		args := append([]any{u.next(), start, zoneOffsetJST, end, zoneOffsetJST, appID}, values...)
		exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, ph), args...)
	}

	// --- 瞬間の記録（オムロン・Fitbit） ---
	instantRow("blood_pressure_record_table", "systolic, diastolic", jst(2026, 9, 11, 8, 0), 2, 125, 68)
	instantRow("blood_pressure_record_table", "systolic, diastolic", jst(2026, 9, 11, 21, 30), 2, 131, 74)
	instantRow("weight_record_table", "weight", jst(2026, 9, 11, 7, 0), 2, 64000) // グラム → kg
	instantRow("weight_record_table", "weight", jst(2026, 9, 10, 7, 0), 2, 64600)
	instantRow("body_fat_record_table", "percentage", jst(2026, 9, 11, 7, 0), 2, 20.8)
	instantRow("basal_metabolic_rate_record_table", "basal_metabolic_rate", jst(2026, 9, 11, 7, 0), 1, 70.5) // ワット → kcal/日
	instantRow("height_record_table", "height", jst(2026, 9, 10, 7, 0), 1, 1.67)                             // メートル → cm
	instantRow("resting_heart_rate_record_table", "beats_per_minute", jst(2026, 9, 11, 6, 37), 4, 64)
	instantRow("heart_rate_variability_rmssd_record_table", "heart_rate_variability_millis", jst(2026, 9, 11, 2, 0), 4, 36.2)
	instantRow("heart_rate_variability_rmssd_record_table", "heart_rate_variability_millis", jst(2026, 9, 11, 3, 0), 4, 44.3)
	instantRow("respiratory_rate_record_table", "rate", jst(2026, 9, 11, 6, 37), 4, 14.6)
	instantRow("oxygen_saturation_record_table", "percentage", jst(2026, 9, 11, 6, 37), 4, 97)

	// --- 期間の記録 ---
	// 歩数: fitness（優先）と pepup（下位）が重なる時間帯を書く。重複排除が効く。
	intervalRow("steps_record_table", "count", jst(2026, 9, 11, 10, 0), jst(2026, 9, 11, 10, 30), 1, 1000)
	intervalRow("steps_record_table", "count", jst(2026, 9, 11, 10, 10), jst(2026, 9, 11, 10, 20), 3, 800)
	intervalRow("steps_record_table", "count", jst(2026, 9, 11, 18, 0), jst(2026, 9, 11, 18, 15), 1, 500)
	intervalRow("distance_record_table", "distance", jst(2026, 9, 11, 10, 0), jst(2026, 9, 11, 10, 30), 1, 1260)
	intervalRow("total_calories_burned_record_table", "energy", jst(2026, 9, 11, 0, 0), jst(2026, 9, 11, 23, 59), 1, 1749366.5) // カロリー → kcal
	// 睡眠: 日付をまたぐ。起床日（9/11）に数える。
	intervalRow("sleep_session_record_table", "", jst(2026, 9, 10, 23, 0), jst(2026, 9, 11, 6, 30), 4)
	intervalRow("exercise_session_record_table", "exercise_type", jst(2026, 9, 11, 13, 0), jst(2026, 9, 11, 13, 30), 1, 53)

	// --- 親子2表の種別 ---
	intervalRow("heart_rate_record_table", "", jst(2026, 9, 11, 22, 0), jst(2026, 9, 11, 23, 0), 2)
	exec(`INSERT INTO heart_rate_record_series_table (parent_key, epoch_millis, beats_per_minute) VALUES (1, ?, 76), (1, ?, 81)`,
		jst(2026, 9, 11, 22, 0), jst(2026, 9, 11, 23, 0))
	intervalRow("SpeedRecordTable", "", jst(2026, 9, 11, 10, 0), jst(2026, 9, 11, 10, 30), 1)
	exec(`INSERT INTO speed_record_table (parent_key, epoch_millis, speed) VALUES (1, ?, 0.3), (1, ?, 1.5)`,
		jst(2026, 9, 11, 10, 0), jst(2026, 9, 11, 10, 30))

	// 睡眠ステージ: 上の睡眠セッション（row_id=1、9/10 23:00〜9/11 06:30）の区間。
	// 日付をまたぐが、すべて起床日（9/11）に数えるはず。合計は 450 分で
	// セッションの長さと一致する。対応表に無い種別（2=睡眠）は落ちるはず。
	stage := func(startH, startM, endH, endM, stageType int, nextDay bool) {
		t.Helper()
		startDay, endDay := 10, 10
		if nextDay {
			startDay, endDay = 11, 11
		}
		if startH > endH {
			endDay = startDay + 1
		}
		exec(`INSERT INTO sleep_stages_table (parent_key, stage_start_time, stage_end_time, stage_type) VALUES (1, ?, ?, ?)`,
			jst(2026, 9, startDay, startH, startM), jst(2026, 9, endDay, endH, endM), stageType)
	}
	stage(23, 0, 23, 30, 1, false) // 覚醒 30分
	stage(23, 30, 2, 30, 4, false) // 浅い 180分（日付をまたぐ）
	stage(2, 30, 3, 30, 5, true)   // 深い 60分
	stage(3, 30, 5, 30, 4, true)   // 浅い 120分（合計 300分）
	stage(5, 30, 6, 30, 6, true)   // REM 60分
	stage(6, 30, 6, 30, 2, true)   // 対応表に無い種別（落ちるはず）

	exec(`INSERT INTO mindfulness_session_record_table (row_id, v) VALUES (1, 1)`)

	return path
}
