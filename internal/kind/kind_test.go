package kind

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

// TestAll_KeysAreSortedAndUnique は一覧の並びと重複を確認する。並びは出力に
// 効く（daily_summary の列順・タブの順）ため辞書順で固定する。
func TestAll_KeysAreSortedAndUnique(t *testing.T) {
	kinds := All()
	if len(kinds) == 0 {
		t.Fatal("種別が1つも無い")
	}

	keys := make([]string, len(kinds))
	for i, k := range kinds {
		keys[i] = k.Key()
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("種別キーが辞書順でない: %v", keys)
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Errorf("種別キーが重複している: %s", key)
		}
		seen[key] = true
	}
}

// TestAll_TablesAndPoliciesAreValid は各種別の宣言が破綻していないことを確認する。
// 設定ファイルを廃止して宣言をコードへ移したため、代わりにここで通す。
func TestAll_TablesAndPoliciesAreValid(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cum.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	tables := map[string]string{}
	for _, k := range All() {
		tbl := k.Table()

		// 宣言どおりにテーブルが作れること（列名・型の検証を通ること）。
		if err := tbl.Migrate(ctx, db); err != nil {
			t.Errorf("%s: Migrate: %v", k.Key(), err)
		}
		if prev, ok := tables[tbl.Name]; ok {
			t.Errorf("%s と %s が同じテーブル名 %q を宣言している", prev, k.Key(), tbl.Name)
		}
		tables[tbl.Name] = k.Key()
		if tbl.Name != "record_"+k.Key() {
			t.Errorf("%s のテーブル名 = %q, want record_%s（既存データを引き継ぐため）", k.Key(), tbl.Name, k.Key())
		}

		// 窓が解釈できること。
		if _, _, err := k.Policy().WindowDuration(); err != nil {
			t.Errorf("%s: 窓が不正: %v", k.Key(), err)
		}

		// 日次集計の関数が既知のものだけであること。
		for _, fn := range k.Policy().Daily {
			switch fn {
			case FuncMean, FuncMin, FuncMax, FuncSum, FuncCount:
			default:
				t.Errorf("%s: 未知の集計関数 %q", k.Key(), fn)
			}
		}
		if len(k.Policy().Daily) == 0 {
			t.Errorf("%s: 日次集計の関数が空", k.Key())
		}

		// 重複排除を使うならカテゴリが必要（優先度はカテゴリ単位で引くため）。
		if k.Policy().Dedupe && k.Policy().Category == 0 {
			t.Errorf("%s: dedupe を使うのにカテゴリが未指定", k.Key())
		}

		// 値名は辞書順で重複が無いこと。重複すると daily_summary に同名の列が
		// 2本出る。
		names := k.ValueNames()
		if len(names) == 0 {
			t.Errorf("%s: 値名が空", k.Key())
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("%s: 値名が辞書順でない: %v", k.Key(), names)
		}
		seenName := map[string]bool{}
		for _, n := range names {
			if seenName[n] {
				t.Errorf("%s: 値名が重複している: %v", k.Key(), names)
			}
			seenName[n] = true
		}

		// 生データのヘッダは先頭4列（local_date / local_start / local_end / app_id）に
		// 続けて種別ごとの列を持つこと。値名と一致するかは種別による（睡眠ステージは
		// stage と minutes の2列で出す）。
		header := k.RawHeader()
		if len(header) < 5 {
			t.Errorf("%s: 生データのヘッダが短い: %v", k.Key(), header)
		}
		wantHead := []any{"local_date", "local_start", "local_end", "app_id"}
		for i, want := range wantHead {
			if header[i] != want {
				t.Errorf("%s: 生データのヘッダ %d列目 = %v, want %v", k.Key(), i+1, header[i], want)
			}
		}
	}
}

// --- 単位変換 ---
//
// エクスポートDBは内部表現で値を持つ。倍率を間違えると出力が静かに壊れるため、
// 種別ごとに実測値で固定する。

func newInstantDB(t *testing.T, table, column string, value float64) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`,
		`INSERT INTO application_info_table (row_id, package_name) VALUES (1, 'com.example.app')`,
		`CREATE TABLE ` + table + ` (uuid BLOB, time INTEGER, zone_offset INTEGER, app_info_id INTEGER, ` + column + ` REAL)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO `+table+` (uuid, time, zone_offset, app_info_id, `+column+`) VALUES (x'01', 1000, 32400, 1, ?)`, value); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return db
}

func TestUnitConversions(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		table  string
		column string
		raw    float64
		want   float64
	}{
		{"体重はグラムから kg へ", weightKind{}, "weight_record_table", "weight", 64000, 64},
		{"身長はメートルから cm へ", heightKind{}, "height_record_table", "height", 1.67, 167},
		{"基礎代謝はワットから kcal/日 へ", basalMetabolicRateKind{}, "basal_metabolic_rate_record_table", "basal_metabolic_rate", 70.5, 70.5 * 20.65},
		{"体脂肪率は変換しない", bodyFatKind{}, "body_fat_record_table", "percentage", 20.8, 20.8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newInstantDB(t, c.table, c.column, c.raw)
			rows, dates, err := c.kind.ExportRows(context.Background(), db)
			if err != nil {
				t.Fatalf("ExportRows: %v", err)
			}
			if len(rows) != 1 || len(dates) != 1 {
				t.Fatalf("rows=%d dates=%d, want 1, 1", len(rows), len(dates))
			}
			got, ok := rows[0][5].(sql.NullFloat64)
			if !ok {
				t.Fatalf("値の型 = %T, want sql.NullFloat64", rows[0][5])
			}
			// 浮動小数の積なので厳密一致ではなく誤差で比べる。
			if !got.Valid || math.Abs(got.Float64-c.want) > 1e-9 {
				t.Errorf("値 = %v, want %v", got, c.want)
			}
		})
	}
}

// TestExportRows_NullValueIsKept は値が NULL の行を落とさず、NULL のまま保存する
// ことを確認する（測定できなかった記録も件数には入る）。
func TestExportRows_NullValueIsKept(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`,
		`CREATE TABLE respiratory_rate_record_table (uuid BLOB, time INTEGER, zone_offset INTEGER, app_info_id INTEGER, rate REAL)`,
		`INSERT INTO respiratory_rate_record_table (uuid, time, zone_offset, app_info_id, rate) VALUES (x'01', 1000, 32400, NULL, NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	rows, _, err := respiratoryRateKind{}.ExportRows(context.Background(), db)
	if err != nil {
		t.Fatalf("ExportRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("件数 = %d, want 1", len(rows))
	}
	if v := rows[0][5].(sql.NullFloat64); v.Valid {
		t.Errorf("NULL の値が Valid になっている: %+v", v)
	}
}

// TestSleepStage_SkipsUnknownStageTypes は対応表に無いステージ種別を取り込まない
// ことを確認する（ADR 0011）。
func TestSleepStage_SkipsUnknownStageTypes(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE application_info_table (row_id INTEGER PRIMARY KEY, package_name TEXT)`,
		`CREATE TABLE sleep_session_record_table (row_id INTEGER PRIMARY KEY, uuid BLOB,
			start_time INTEGER, start_zone_offset INTEGER, end_time INTEGER, end_zone_offset INTEGER, app_info_id INTEGER)`,
		`CREATE TABLE sleep_stages_table (parent_key INTEGER, stage_start_time INTEGER,
			stage_end_time INTEGER, stage_type INTEGER)`,
		`INSERT INTO sleep_session_record_table (row_id, uuid, start_time, start_zone_offset, end_time, end_zone_offset, app_info_id)
			VALUES (1, x'01', 0, 32400, 3600000, 32400, NULL)`,
		// 4=浅い（取り込む）、2=睡眠（対応表に無いので落ちる）
		`INSERT INTO sleep_stages_table (parent_key, stage_start_time, stage_end_time, stage_type)
			VALUES (1, 0, 600000, 4), (1, 600000, 1200000, 2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	rows, dates, err := sleepStageKind{}.ExportRows(context.Background(), db)
	if err != nil {
		t.Fatalf("ExportRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("件数 = %d, want 1（未対応の種別は落ちる）", len(rows))
	}
	if rows[0][6] != int64(stageLight) {
		t.Errorf("残った種別 = %v, want %d", rows[0][6], stageLight)
	}
	// 日ごと置き換えの基準は親セッションの終了日（起床日）。
	if len(dates) != 1 || dates[0] != localDate(3600000, 32400) {
		t.Errorf("dates = %v, want 起床日", dates)
	}
}

func TestPolicy_WindowDuration(t *testing.T) {
	if _, unlimited, err := (Policy{Window: WindowAll}).WindowDuration(); err != nil || !unlimited {
		t.Errorf(`"all" = unlimited=%v err=%v, want true, nil`, unlimited, err)
	}
	d, unlimited, err := (Policy{Window: "30d"}).WindowDuration()
	if err != nil || unlimited || d.Hours() != 24*30 {
		t.Errorf(`"30d" = %v, %v, %v`, d, unlimited, err)
	}
	for _, bad := range []string{"", "30", "d", "0d", "-1d", "１d"} {
		if _, _, err := (Policy{Window: bad}).WindowDuration(); err == nil {
			t.Errorf("窓 %q がエラーにならなかった", bad)
		}
	}
}
