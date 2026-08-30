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
