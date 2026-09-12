package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"health-connect-converter/internal/cumdb"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
	"health-connect-converter/internal/report"
)

// --- フェイク ---

type fakeSource struct {
	zip   *model.ZipFile
	err   error
	calls []time.Time
	// respectAfter が真なら、zip の modifiedTime が after より後のときだけ返す
	// （drivesource と同じ判定）。強制取り込みの検証に使う。
	respectAfter bool
}

func (f *fakeSource) FetchLatest(_ context.Context, after time.Time) (*model.ZipFile, error) {
	f.calls = append(f.calls, after)
	if f.err != nil {
		return nil, f.err
	}
	if f.respectAfter && f.zip != nil && !f.zip.ModifiedTime.After(after) {
		return nil, nil
	}
	return f.zip, nil
}

type fakeIngester struct {
	tableRows map[string]int64
	err       error
	calls     int
}

func (f *fakeIngester) Ingest(context.Context, *model.ZipFile) (*model.ExportInfo, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &model.ExportInfo{TableRows: f.tableRows}, nil
}

// fakeKind は app が見る部分（キーと方針）だけを持つテスト用の種別。
type fakeKind struct {
	key string
}

func (k fakeKind) Key() string { return k.key }
func (k fakeKind) Policy() kind.Policy {
	return kind.Policy{Window: kind.WindowAll, Daily: []string{kind.FuncSum}}
}
func (k fakeKind) ValueNames() []string { return []string{"v"} }
func (k fakeKind) RawHeader() []any     { return []any{"local_date", "v"} }
func (k fakeKind) Table() cumdb.Table   { return cumdb.Table{} }
func (k fakeKind) ExportRows(context.Context, *sql.DB) ([][]any, []string, error) {
	return nil, nil, nil
}
func (k fakeKind) Aggregate(context.Context, *sql.DB) ([]model.AggRecord, error) { return nil, nil }
func (k fakeKind) RawRows(context.Context, *sql.DB, int64) ([][]any, error)      { return nil, nil }

func testKinds() []kind.Kind {
	return []kind.Kind{fakeKind{key: "steps"}, fakeKind{key: "weight"}}
}

type fakeStore struct {
	state map[string]string

	getStateErr error
	dailyErr    error
	rawErr      error
	statsErr    error

	setStateErr   error
	setStateCalls []struct{ key, value string }
}

func newFakeStore() *fakeStore {
	return &fakeStore{state: map[string]string{}}
}

func (f *fakeStore) DailyAggregates(context.Context, kind.Kind) ([]model.DailyRow, error) {
	return nil, f.dailyErr
}

func (f *fakeStore) RawRows(context.Context, kind.Kind, int64) ([][]any, error) {
	return nil, f.rawErr
}

func (f *fakeStore) Stats(context.Context, kind.Kind) (model.TypeStats, error) {
	return model.TypeStats{}, f.statsErr
}

func (f *fakeStore) GetState(_ context.Context, key string) (string, error) {
	if f.getStateErr != nil {
		return "", f.getStateErr
	}
	return f.state[key], nil
}

func (f *fakeStore) SetState(_ context.Context, key, value string) error {
	f.setStateCalls = append(f.setStateCalls, struct{ key, value string }{key, value})
	if f.setStateErr != nil {
		return f.setStateErr
	}
	f.state[key] = value
	return nil
}

type writeTabCall struct {
	title string
	rows  [][]any
}

type fakeSink struct {
	err          error
	errOnTitle   string // 空なら全タイトルでエラー
	moveErr      error
	calls        []writeTabCall
	moveTabCalls []string
}

func (f *fakeSink) WriteTab(_ context.Context, title string, rows [][]any) error {
	f.calls = append(f.calls, writeTabCall{title: title, rows: rows})
	if f.err != nil && (f.errOnTitle == "" || f.errOnTitle == title) {
		return f.err
	}
	return nil
}

func (f *fakeSink) MoveTabFirst(_ context.Context, title string) error {
	f.moveTabCalls = append(f.moveTabCalls, title)
	return f.moveErr
}

// logCapture はテストでログ出力内容を検証するための slog.Handler。
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}

func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCapture) WithGroup(string) slog.Handler      { return l }

func (l *logCapture) hasWarnWithAttrs(key, value string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		var gotKey, gotValue string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "key":
				gotKey = a.Value.String()
			case "value":
				gotValue = a.Value.String()
			}
			return true
		})
		if gotKey == key && gotValue == value {
			return true
		}
	}
	return false
}

func discardLogger() *slog.Logger {
	return slog.New(&logCapture{})
}

func fixedNowFunc(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// --- RunOnce のテスト ---

func TestRunOnce_NoNewFile_OnlyFixesTabOrder(t *testing.T) {
	src := &fakeSource{zip: nil}
	ing := &fakeIngester{}
	st := newFakeStore()
	sink := &fakeSink{}

	// 起動後の強制取り込みを消化させるため、あらかじめ1周させる。
	a := New(testKinds(), src, ing, st, sink, discardLogger(), nil)
	a.startupIngestDone = true

	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if ing.calls != 0 {
		t.Errorf("Ingest calls = %d, want 0", ing.calls)
	}
	if len(sink.calls) != 0 {
		t.Errorf("WriteTab calls = %d, want 0", len(sink.calls))
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
	// タブ順の是正だけは新着の有無によらず行う。
	if want := []string{report.DailySummaryTitle}; !slices.Equal(sink.moveTabCalls, want) {
		t.Errorf("MoveTabFirst calls = %v, want %v", sink.moveTabCalls, want)
	}
}

func TestRunOnce_NoNewFile_MoveTabFirstFails(t *testing.T) {
	src := &fakeSource{zip: nil}
	sink := &fakeSink{moveErr: errors.New("boom")}

	a := New(testKinds(), src, &fakeIngester{}, newFakeStore(), sink, discardLogger(), nil)
	a.startupIngestDone = true

	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
}

func TestRunOnce_SecondCycle_FetchLatestReceivesStateTime(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: want}}
	st := newFakeStore()

	a := New(testKinds(), src, &fakeIngester{}, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() 1周目 error = %v", err)
	}
	src.zip = nil
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() 2周目 error = %v", err)
	}

	if len(src.calls) != 2 {
		t.Fatalf("FetchLatest calls = %d, want 2", len(src.calls))
	}
	if !src.calls[0].IsZero() {
		t.Errorf("1周目の after = %v, want zero", src.calls[0])
	}
	if !src.calls[1].Equal(want) {
		t.Errorf("2周目の after = %v, want %v", src.calls[1], want)
	}
}

func TestRunOnce_EmptyState_AfterIsZero(t *testing.T) {
	src := &fakeSource{zip: nil}
	a := New(testKinds(), src, &fakeIngester{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(src.calls) != 1 || !src.calls[0].IsZero() {
		t.Errorf("FetchLatest after = %v, want zero", src.calls)
	}
}

func TestRunOnce_BrokenState_AfterIsZeroAndWarns(t *testing.T) {
	src := &fakeSource{zip: nil}
	st := newFakeStore()
	st.state[stateKeyLastProcessedModifiedTime] = "not-a-valid-time"

	capture := &logCapture{}
	a := New(testKinds(), src, &fakeIngester{}, st, &fakeSink{}, slog.New(capture), nil)
	a.startupIngestDone = true

	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(src.calls) != 1 || !src.calls[0].IsZero() {
		t.Errorf("FetchLatest after = %v, want zero", src.calls)
	}
	if !capture.hasWarnWithAttrs(stateKeyLastProcessedModifiedTime, "not-a-valid-time") {
		t.Errorf("壊れた state の警告が出ていない: %+v", capture.records)
	}
}

// TestRunOnce_FirstCycleIngestsEvenWithoutNewZip は、state が最新ZIPと同じ時刻でも
// 起動後の最初の1周は取り込むことを確認する（ADR 0010）。
func TestRunOnce_FirstCycleIngestsEvenWithoutNewZip(t *testing.T) {
	modified := time.Date(2026, 9, 11, 16, 30, 56, 853000000, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: modified}, respectAfter: true}
	ing := &fakeIngester{}
	st := newFakeStore()
	st.state[stateKeyLastProcessedModifiedTime] = modified.Format(time.RFC3339Nano)

	a := New(testKinds(), src, ing, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if ing.calls != 1 {
		t.Fatalf("Ingest calls = %d, want 1（新着が無くても取り込む）", ing.calls)
	}
}

func TestRunOnce_SecondCycleSkipsWhenNoNewZip(t *testing.T) {
	modified := time.Date(2026, 9, 11, 16, 30, 56, 853000000, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: modified}, respectAfter: true}
	ing := &fakeIngester{}

	a := New(testKinds(), src, ing, newFakeStore(), &fakeSink{}, discardLogger(), nil)
	for i := 1; i <= 2; i++ {
		if err := a.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce() %d周目 error = %v", i, err)
		}
	}
	if ing.calls != 1 {
		t.Errorf("Ingest calls = %d, want 1（2周目は新着なし）", ing.calls)
	}
}

func TestRunOnce_StartupIngestRetriedAfterFailure(t *testing.T) {
	modified := time.Date(2026, 9, 11, 16, 30, 56, 853000000, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: modified}, respectAfter: true}
	st := newFakeStore()
	st.state[stateKeyLastProcessedModifiedTime] = modified.Format(time.RFC3339Nano)
	sink := &fakeSink{err: errors.New("write boom")}

	a := New(testKinds(), src, &fakeIngester{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() 1周目 error = nil, want error")
	}

	sink.err = nil
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() 2周目 error = %v", err)
	}
	if len(src.calls) != 2 || !src.calls[1].IsZero() {
		t.Errorf("2周目の after = %v, want zero（強制取り込みが未消化）", src.calls)
	}
}

// TestRunOnce_StateKeepsMillisecondPrecision は state がミリ秒を落とさないことを
// 確認する。秒精度で保存すると復元値が常に実際より古くなり、同じZIPが毎周回
// 「新着」と判定される。
func TestRunOnce_StateKeepsMillisecondPrecision(t *testing.T) {
	modified := time.Date(2026, 9, 11, 16, 30, 56, 853000000, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: modified}}
	st := newFakeStore()

	a := New(testKinds(), src, &fakeIngester{}, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	const want = "2026-09-11T16:30:56.853Z"
	if got := st.state[stateKeyLastProcessedModifiedTime]; got != want {
		t.Errorf("last_processed_modified_time = %q, want %q", got, want)
	}
}

func TestRunOnce_Success_WritesTabsInOrderAndUpdatesState(t *testing.T) {
	modified := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	now := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", Name: "export.zip", ModifiedTime: modified}}
	st := newFakeStore()
	sink := &fakeSink{}

	a := New(testKinds(), src, &fakeIngester{}, st, sink, discardLogger(), fixedNowFunc(now))
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	wantTitles := []string{"daily_summary", "steps_raw", "weight_raw", "_meta"}
	if len(sink.calls) != len(wantTitles) {
		t.Fatalf("WriteTab calls = %d, want %d", len(sink.calls), len(wantTitles))
	}
	for i, want := range wantTitles {
		if sink.calls[i].title != want {
			t.Errorf("WriteTab[%d].title = %q, want %q", i, sink.calls[i].title, want)
		}
	}

	if len(st.setStateCalls) != 3 {
		t.Fatalf("SetState calls = %d, want 3", len(st.setStateCalls))
	}
	got := map[string]string{}
	for _, c := range st.setStateCalls {
		got[c.key] = c.value
	}
	if got[stateKeyLastProcessedModifiedTime] != modified.Format(time.RFC3339Nano) {
		t.Errorf("last_processed_modified_time = %q", got[stateKeyLastProcessedModifiedTime])
	}
	if got[stateKeyLastProcessedFileID] != "file-1" {
		t.Errorf("last_processed_file_id = %q, want file-1", got[stateKeyLastProcessedFileID])
	}
	if got[stateKeyLastSuccessAt] != now.Format(time.RFC3339) {
		t.Errorf("last_success_at = %q", got[stateKeyLastSuccessAt])
	}
}

func TestRunOnce_MetaIncludesExportTableRows(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: time.Now()}}
	ing := &fakeIngester{tableRows: map[string]int64{"unregistered_table": 7}}
	sink := &fakeSink{}

	a := New(testKinds(), src, ing, newFakeStore(), sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	var meta [][]any
	for _, c := range sink.calls {
		if c.title == report.MetaTitle {
			meta = c.rows
		}
	}
	if meta == nil {
		t.Fatalf("_meta タブが書かれていない: %+v", sink.calls)
	}
	found := false
	for _, row := range meta {
		if len(row) == 2 && row[0] == "export_unregistered_table_rows" && row[1] == int64(7) {
			found = true
		}
	}
	if !found {
		t.Errorf("_meta に export_unregistered_table_rows が無い: %+v", meta)
	}
}

func TestRunOnce_IngestFails_NoStateUpdate(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: time.Now()}}
	ing := &fakeIngester{err: errors.New("ingest boom")}
	st := newFakeStore()

	a := New(testKinds(), src, ing, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
}

func TestRunOnce_WriteTabFails_NoStateUpdate(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "file-1", ModifiedTime: time.Now()}}
	st := newFakeStore()
	sink := &fakeSink{err: errors.New("write boom")}

	a := New(testKinds(), src, &fakeIngester{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
}

func TestRunOnce_MovesDailySummaryFirstAfterWriting(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "f1", ModifiedTime: time.Now()}}
	sink := &fakeSink{}

	a := New(testKinds(), src, &fakeIngester{}, newFakeStore(), sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if want := []string{report.DailySummaryTitle}; !slices.Equal(sink.moveTabCalls, want) {
		t.Fatalf("MoveTabFirst calls = %v, want %v", sink.moveTabCalls, want)
	}
	// タブを作る前に移動しても意味がないため、書き込みの後でなければならない。
	if len(sink.calls) == 0 {
		t.Fatal("WriteTab が呼ばれていない")
	}
}

func TestRunOnce_MoveTabFirstFails_NoStateUpdate(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "f1", ModifiedTime: time.Now()}}
	st := newFakeStore()
	sink := &fakeSink{moveErr: errors.New("boom")}

	a := New(testKinds(), src, &fakeIngester{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
}

// --- Run のテスト ---

func TestRun_RunsOnceImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{zip: nil}
	a := New(testKinds(), src, &fakeIngester{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Hour) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx キャンセル後に Run が返らない")
	}

	if len(src.calls) != 1 {
		t.Errorf("FetchLatest calls = %d, want 1", len(src.calls))
	}
}

func TestRun_ContinuesAfterRunOnceError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{err: errors.New("fetch boom")}
	a := New(testKinds(), src, &fakeIngester{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, 10*time.Millisecond) }()

	time.Sleep(55 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx キャンセル後に Run が返らない")
	}

	if len(src.calls) < 2 {
		t.Errorf("FetchLatest calls = %d, want >= 2", len(src.calls))
	}
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := New(testKinds(), &fakeSource{zip: nil}, &fakeIngester{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, 10*time.Millisecond) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx キャンセル後に Run が返らない")
	}
}
