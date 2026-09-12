// Package kind は種別ごとの保存用モデルと、その読み書き・変換を持つ。
//
// 1種別 = 1ファイル。エクスポートDBからの読み出し、累積DBの列の宣言、集計用
// モデルへの変換、生データタブの行の作り方、集計の方針（窓・日次関数・重複排除）
// をその種別のファイルに閉じる（ADR 0012）。種別を跨いで共有するのは SQL の
// 組み立てのような定型処理だけで、モデルは共有しない。
package kind

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/model"
)

// Health Connect のデータカテゴリ。重複排除でアプリ優先度を引くのに使う
// （優先度はカテゴリ単位。ADR 0009）。値は Android の HealthDataCategory の定数。
const (
	CategoryActivity         = 1
	CategoryBodyMeasurements = 2
	CategoryCycleTracking    = 3
	CategoryNutrition        = 4
	CategorySleep            = 5
	CategoryVitals           = 6
	CategoryWellness         = 7
)

// 日次集計の関数名。
const (
	FuncMean  = "mean"
	FuncMin   = "min"
	FuncMax   = "max"
	FuncSum   = "sum"
	FuncCount = "count"
)

// WindowAll は生データを全期間出すことを表す。
const WindowAll = "all"

// Policy は種別ごとの集計・出力の方針。
type Policy struct {
	// Window は生データタブに出す期間。"all" か "<日数>d"。
	Window string
	// Daily は daily_summary に出す集計関数。
	Daily []string
	// Dedupe が真なら、日次集計の前にアプリ優先度で重複排除する。
	Dedupe bool
	// Category は Dedupe が真のときに優先度を引くカテゴリ。
	Category int
}

var windowDaysRe = regexp.MustCompile(`^([0-9]+)d$`)

// WindowDuration は Window を解釈する。"all" のとき unlimited=true。
func (p Policy) WindowDuration() (time.Duration, bool, error) {
	if p.Window == WindowAll {
		return 0, true, nil
	}
	m := windowDaysRe.FindStringSubmatch(p.Window)
	if m == nil {
		return 0, false, fmt.Errorf("kind: invalid window %q (must be %q or \"<days>d\")", p.Window, WindowAll)
	}
	days, err := strconv.Atoi(m[1])
	if err != nil || days <= 0 {
		return 0, false, fmt.Errorf("kind: invalid window %q", p.Window)
	}
	return time.Duration(days) * 24 * time.Hour, false, nil
}

// Kind は1種別。保存用モデルの実体は各種別のファイルに閉じており、外からは
// 「保存する行」「集計用モデル」「生データの行」の形でだけ見える。
type Kind interface {
	// Key は種別キー。累積DBのテーブル名・出力の列名・タブ名に効くため変えない。
	Key() string
	// Policy は集計・出力の方針。
	Policy() Policy
	// Table は累積DBの保存先の宣言。
	Table() cumdb.Table
	// ExportRows はエクスポートDBを読み、保存する行（Table.Columns と同じ順）と
	// 置き換える現地日を返す。
	ExportRows(ctx context.Context, exportDB *sql.DB) (rows [][]any, dates []string, err error)
	// Aggregate は累積DBの全期間を集計用モデルへ直す。
	Aggregate(ctx context.Context, cum *sql.DB) ([]model.AggRecord, error)
	// ValueNames は daily_summary に出す値名（辞書順）。
	ValueNames() []string
	// RawHeader は生データタブのヘッダ。
	RawHeader() []any
	// RawRows は生データタブの行（ヘッダを除く）。sinceMs 以降を時刻の昇順で返す。
	RawRows(ctx context.Context, cum *sql.DB, sinceMs int64) ([][]any, error)
}

// --- 種別のファイルから使う共通部品 ---

// envelope は多くの種別が共通で持つ列の宣言。**宣言を返すだけ**で、種別の型を
// 共通化するものではない。睡眠ステージのように別の列が要る種別は自分で並べる。
func envelope(values ...cumdb.Column) []cumdb.Column {
	cols := []cumdb.Column{
		{Name: "uuid", Type: "TEXT"},
		{Name: "start_time", Type: "INTEGER"},
		{Name: "end_time", Type: "INTEGER"},
		{Name: "zone_offset", Type: "INTEGER"},
		{Name: "app_id", Type: "TEXT"},
	}
	return append(cols, values...)
}

// envelopeTable は envelope の列を持つ種別の保存先を宣言する。
func envelopeTable(key string, values ...cumdb.Column) cumdb.Table {
	return cumdb.Table{
		Name:       "record_" + key,
		Columns:    envelope(values...),
		TimeColumn: "start_time",
		DateExpr:   cumdb.LocalDateExpr("start_time", "zone_offset"),
	}
}

// localDate は時刻（UTC epoch ms）とオフセット（秒）から現地日を返す。
func localDate(ms int64, zoneOffset int32) string {
	return time.Unix(ms/1000+int64(zoneOffset), 0).UTC().Format("2006-01-02")
}

// localTime は生データタブに出す現地時刻。
func localTime(ms int64, zoneOffset int32) time.Time {
	return time.Unix(ms/1000, 0).UTC().Add(time.Duration(zoneOffset) * time.Second)
}

// rawHeader は生データタブのヘッダを組み立てる。先頭4列はどの種別も共通。
func rawHeader(valueNames ...string) []any {
	out := []any{"local_date", "local_start", "local_end", "app_id"}
	for _, n := range valueNames {
		out = append(out, n)
	}
	return out
}

// loadAll は累積DBの行を種別の型へ読み出す。走査（scan）は種別が渡す。
func loadAll[T any](ctx context.Context, db *sql.DB, t cumdb.Table, sinceMs int64, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := t.Query(ctx, db, sinceMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("kind: scan %s: %w", t.Name, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kind: rows %s: %w", t.Name, err)
	}
	return out, nil
}

// uniqueDates は現地日の重複を除いて返す（日ごと置き換えの対象）。
func uniqueDates(dates []string) []string {
	seen := make(map[string]bool, len(dates))
	out := make([]string, 0, len(dates))
	for _, d := range dates {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// nullable は生データタブに出す値。NULL は空文字にする（出力の既存仕様）。
func nullable(v sql.NullFloat64) any {
	if !v.Valid {
		return ""
	}
	return v.Float64
}
