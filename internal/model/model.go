// Package model は種別によらない正規化済みのデータ表現を持つ。
package model

import "time"

// Record は種別によらない正規化済みの1レコード。
type Record struct {
	UUID       string
	StartTime  int64 // UTC epoch ms
	EndTime    int64 // UTC epoch ms。瞬時値の種別は StartTime と同値
	ZoneOffset int32 // 記録時のタイムゾーンオフセット（秒）
	AppID      string
	Values     map[string]float64
}

// ZipFile は Drive から取得したエクスポートZIP。
type ZipFile struct {
	FileID       string
	Name         string
	ModifiedTime time.Time
	Data         []byte
}

// AppPriorities はデータカテゴリごとのアプリ優先順。スライスの先頭が最優先。
// キーは Health Connect のカテゴリ整数（config.Categories の値）。
type AppPriorities map[int][]string

// ExportData はエクスポートDBから読み出した内容一式。
type ExportData struct {
	Records    map[string][]Record
	Priorities AppPriorities
	// TableRows はエクスポートDB内の全テーブルの行数。テーブル名がキー。
	// config に登録していないテーブルへ書き込みが始まったことに気づけるよう、
	// 種別定義とは無関係にDB内のテーブルをすべて数える。
	TableRows map[string]int64
}

// AggRecord は集計用のデータモデル。保存用モデルから種別ごとの変換で作り、
// 保存しない（ADR 0012）。重複排除と日次集計はこの形だけを見て動く。
type AggRecord struct {
	// LocalDate はこのレコードを数える現地日（"2006-01-02"）。どの日に数えるかは
	// 種別ごとの変換が決める（睡眠なら起床日、歩数なら歩き始めた日）。
	LocalDate string
	// StartTime / EndTime は重複排除でアプリ間の重なりを測るための期間（UTC epoch ms）。
	// 瞬間の記録は同値。
	StartTime int64
	EndTime   int64
	// ZoneOffset は記録時のタイムゾーンオフセット（秒）。重複排除が「アプリ×現地日」
	// で範囲をまとめるときに使う。
	ZoneOffset int32
	// AppID は記録元アプリのパッケージ名。重複排除で優先度を引くのに使う。
	AppID string
	// Values は集計対象の数値（値名 → 値）。
	Values map[string]float64
}

// DailyRow は1日ぶんの集約結果。
type DailyRow struct {
	Date   string // 現地日 "2006-01-02"
	Values map[string]float64
}

// TypeStats は _meta タブ用の種別ごとの統計。
type TypeStats struct {
	Count           int64
	LatestStartTime int64 // UTC epoch ms。0件なら 0
}
