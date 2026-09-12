// Package e2e はエクスポートDBから出力タブまでを通しで動かし、行列を期待値
// ファイル（testdata）と突き合わせる。
//
// 個々のパッケージのテストは自分の担当範囲だけを見るため、取り込み・重複排除・
// 日次集計・出力の噛み合わせが変わったことに気付けない。ここは出力そのものを
// 固定して、意図しない変化を落とす役を担う。
//
// 期待値を更新するには HC_UPDATE_GOLDEN=1 を付けて実行し、差分を必ず目で見る。
//
//	HC_UPDATE_GOLDEN=1 go test ./internal/e2e/
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"health-connect-converter/internal/hcreader"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/report"
	"health-connect-converter/internal/store"
)

// fixedNow は生データタブの窓（直近N日）の基準。合成データの時刻に合わせて固定する。
var fixedNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func TestExportToTabs(t *testing.T) {
	ctx := context.Background()
	kinds := kind.All()

	exportDB, err := hcreader.Open(newExportFixture(t))
	if err != nil {
		t.Fatalf("hcreader.Open: %v", err)
	}
	defer func() { _ = exportDB.Close() }()

	st, err := store.Open(filepath.Join(t.TempDir(), "cumulative.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx, kinds); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	prios, err := hcreader.ReadAppPriorities(ctx, exportDB)
	if err != nil {
		t.Fatalf("ReadAppPriorities: %v", err)
	}
	if err := st.SetAppPriorities(ctx, prios); err != nil {
		t.Fatalf("SetAppPriorities: %v", err)
	}
	tableRows, err := hcreader.ReadTableRows(ctx, exportDB, discardLogger())
	if err != nil {
		t.Fatalf("ReadTableRows: %v", err)
	}

	for _, k := range kinds {
		if _, err := st.Ingest(ctx, k, exportDB); err != nil {
			t.Fatalf("Ingest(%s): %v", k.Key(), err)
		}
	}

	daily, err := report.BuildDailySummary(ctx, st, kinds)
	if err != nil {
		t.Fatalf("BuildDailySummary: %v", err)
	}
	compareGolden(t, report.DailySummaryTitle, daily)

	for _, k := range kinds {
		rows, err := report.BuildRawTab(ctx, st, k, fixedNow)
		if err != nil {
			t.Fatalf("BuildRawTab(%s): %v", k.Key(), err)
		}
		compareGolden(t, report.RawTabTitle(k.Key()), rows)
	}

	meta, err := report.BuildMeta(ctx, st, kinds, fixedNow, fixedNow, tableRows)
	if err != nil {
		t.Fatalf("BuildMeta: %v", err)
	}
	compareGolden(t, report.MetaTitle, meta)
}

// discardLogger はテスト中のログを捨てる。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// compareGolden は行列を testdata/<name>.json と突き合わせる。
func compareGolden(t *testing.T, name string, rows [][]any) {
	t.Helper()

	got, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", name+".json")
	if os.Getenv("HC_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("期待値を更新: %s（%d行）", path, len(rows))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("期待値ファイルが読めない: %v（HC_UPDATE_GOLDEN=1 で作成できる）", err)
	}
	if string(got) == string(want) {
		return
	}

	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := "(なし)", "(なし)"
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("%s が期待値と違う（%d行目）\n  実際: %s\n  期待: %s\n(行数: 実際 %d / 期待 %d)",
				name, i+1, g, w, len(gotLines), len(wantLines))
			return
		}
	}
}
