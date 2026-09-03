package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
	"health-connect-converter/internal/report"
)

// --- フェイク ---

type fakeSource struct {
	zip   *model.ZipFile
	err   error
	calls []time.Time
}

func (f *fakeSource) FetchLatest(_ context.Context, after time.Time) (*model.ZipFile, error) {
	f.calls = append(f.calls, after)
	if f.err != nil {
		return nil, f.err
	}
	return f.zip, nil
}

type fakeReader struct {
	recs  map[string][]model.Record
	prios model.AppPriorities
	err   error
	calls int
}

func (f *fakeReader) Read(_ *model.ZipFile, _ *config.Config) (*model.ExportData, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &model.ExportData{Records: f.recs, Priorities: f.prios}, nil
}

type upsertCall struct {
	typeKey string
	recs    []model.Record
}

type fakeStore struct {
	state map[string]string

	getStateErr error

	upsertErr   error
	upsertCalls []upsertCall

	prios            model.AppPriorities
	setPrioritiesErr error

	dailyErr   error
	recordsErr error
	statsErr   error

	setStateErr   error
	setStateCalls []struct{ key, value string }
}

func newFakeStore() *fakeStore {
	return &fakeStore{state: map[string]string{}}
}

func (f *fakeStore) SetAppPriorities(_ context.Context, prios model.AppPriorities) error {
	f.prios = prios
	return f.setPrioritiesErr
}

func (f *fakeStore) ReplaceRecords(_ context.Context, typeKey string, _ config.TypeConfig, recs []model.Record) (int, error) {
	f.upsertCalls = append(f.upsertCalls, upsertCall{typeKey: typeKey, recs: recs})
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	return len(recs), nil
}

func (f *fakeStore) DailyAggregates(_ context.Context, _ string, _ config.TypeConfig) ([]model.DailyRow, error) {
	return nil, f.dailyErr
}

func (f *fakeStore) RecordsSince(_ context.Context, _ string, _ config.TypeConfig, _ int64) ([]model.Record, error) {
	return nil, f.recordsErr
}

func (f *fakeStore) TypeStats(_ context.Context, _ string) (model.TypeStats, error) {
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

// --- テスト共通のセットアップ ---

func testConfig() *config.Config {
	return &config.Config{
		Types: map[string]config.TypeConfig{
			"steps": {
				SourceTable: "steps_record",
				TimeLayout:  config.LayoutInstant,
				Columns:     map[string]config.ColumnConfig{"count": {Column: "count", Scale: 1}},
				Window:      "all",
				Daily:       []string{"sum"},
			},
			"weight": {
				SourceTable: "weight_record",
				TimeLayout:  config.LayoutInstant,
				Columns:     map[string]config.ColumnConfig{"kg": {Column: "weight", Scale: 1}},
				Window:      "all",
				Daily:       []string{"mean"},
			},
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(&logCapture{})
}

func fixedNowFunc(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// --- RunOnce のテスト ---

func TestRunOnce_NoNewFile_NoOtherCalls(t *testing.T) {
	src := &fakeSource{zip: nil}
	rd := &fakeReader{}
	st := newFakeStore()
	sink := &fakeSink{}

	a := New(testConfig(), src, rd, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if rd.calls != 0 {
		t.Errorf("Reader.Read calls = %d, want 0", rd.calls)
	}
	if len(st.upsertCalls) != 0 {
		t.Errorf("UpsertRecords calls = %d, want 0", len(st.upsertCalls))
	}
	if len(sink.calls) != 0 {
		t.Errorf("Sink.WriteTab calls = %d, want 0", len(sink.calls))
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}

	// タブ順の是正だけは新着の有無によらず行う。
	if want := []string{report.DailySummaryTitle}; !slices.Equal(sink.moveTabCalls, want) {
		t.Errorf("Sink.MoveTabFirst calls = %v, want %v", sink.moveTabCalls, want)
	}
}

// 人手で先頭にシートを挿入されても、新着ZIPを待たずに次の周回で是正する。
func TestRunOnce_NoNewFile_MoveTabFirstFails(t *testing.T) {
	src := &fakeSource{zip: nil}
	st := newFakeStore()
	sink := &fakeSink{moveErr: errors.New("boom")}

	a := New(testConfig(), src, &fakeReader{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
}

func TestRunOnce_MovesDailySummaryFirstAfterWriting(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "f1", ModifiedTime: time.Now()}}
	st := newFakeStore()
	sink := &fakeSink{}

	a := New(testConfig(), src, &fakeReader{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if want := []string{report.DailySummaryTitle}; !slices.Equal(sink.moveTabCalls, want) {
		t.Fatalf("Sink.MoveTabFirst calls = %v, want %v", sink.moveTabCalls, want)
	}
	// タブを作る前に移動しても意味がないため、書き込みの後でなければならない。
	if len(sink.calls) == 0 {
		t.Fatal("WriteTab was not called")
	}
}

// 是正に失敗したら state を進めない。次の周回で同じZIPを取り直して再試行する。
func TestRunOnce_MoveTabFirstFails_NoStateUpdate(t *testing.T) {
	src := &fakeSource{zip: &model.ZipFile{FileID: "f1", ModifiedTime: time.Now()}}
	st := newFakeStore()
	sink := &fakeSink{moveErr: errors.New("boom")}

	a := New(testConfig(), src, &fakeReader{}, st, sink, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}

	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
}

func TestRunOnce_FetchLatestReceivesStateTime(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	src := &fakeSource{zip: nil}
	st := newFakeStore()
	st.state[stateKeyLastProcessedModifiedTime] = want.Format(time.RFC3339)

	a := New(testConfig(), src, &fakeReader{}, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if len(src.calls) != 1 {
		t.Fatalf("FetchLatest calls = %d, want 1", len(src.calls))
	}
	if !src.calls[0].Equal(want) {
		t.Errorf("FetchLatest after = %v, want %v", src.calls[0], want)
	}
}

func TestRunOnce_EmptyState_AfterIsZero(t *testing.T) {
	src := &fakeSource{zip: nil}
	st := newFakeStore()

	a := New(testConfig(), src, &fakeReader{}, st, &fakeSink{}, discardLogger(), nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if len(src.calls) != 1 {
		t.Fatalf("FetchLatest calls = %d, want 1", len(src.calls))
	}
	if !src.calls[0].IsZero() {
		t.Errorf("FetchLatest after = %v, want zero", src.calls[0])
	}
}

func TestRunOnce_BrokenState_AfterIsZeroAndWarns(t *testing.T) {
	src := &fakeSource{zip: nil}
	st := newFakeStore()
	st.state[stateKeyLastProcessedModifiedTime] = "not-a-valid-time"

	capture := &logCapture{}
	logger := slog.New(capture)

	a := New(testConfig(), src, &fakeReader{}, st, &fakeSink{}, logger, nil)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	if len(src.calls) != 1 {
		t.Fatalf("FetchLatest calls = %d, want 1", len(src.calls))
	}
	if !src.calls[0].IsZero() {
		t.Errorf("FetchLatest after = %v, want zero", src.calls[0])
	}
	if !capture.hasWarnWithAttrs(stateKeyLastProcessedModifiedTime, "not-a-valid-time") {
		t.Errorf("expected Warn log for broken state, records = %+v", capture.records)
	}
}

func TestRunOnce_Success_UpdatesStateAndWritesTabsInOrder(t *testing.T) {
	modified := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	zip := &model.ZipFile{FileID: "file-1", Name: "export.zip", ModifiedTime: modified, Data: []byte("dummy")}
	now := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)

	src := &fakeSource{zip: zip}
	rd := &fakeReader{recs: map[string][]model.Record{
		"steps": {{UUID: "a", StartTime: 1000, EndTime: 1000, Values: map[string]float64{"count": 10}}},
	}}
	st := newFakeStore()
	sink := &fakeSink{}

	a := New(testConfig(), src, rd, st, sink, discardLogger(), fixedNowFunc(now))
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	// steps のみ Read 結果にあるので UPSERT は1回だけ（weight はスキップ）。
	if len(st.upsertCalls) != 1 || st.upsertCalls[0].typeKey != "steps" {
		t.Errorf("upsertCalls = %+v, want only steps", st.upsertCalls)
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
	if got[stateKeyLastProcessedModifiedTime] != modified.Format(time.RFC3339) {
		t.Errorf("last_processed_modified_time = %q, want %q", got[stateKeyLastProcessedModifiedTime], modified.Format(time.RFC3339))
	}
	if got[stateKeyLastProcessedFileID] != "file-1" {
		t.Errorf("last_processed_file_id = %q, want %q", got[stateKeyLastProcessedFileID], "file-1")
	}
	if got[stateKeyLastSuccessAt] != now.Format(time.RFC3339) {
		t.Errorf("last_success_at = %q, want %q", got[stateKeyLastSuccessAt], now.Format(time.RFC3339))
	}
}

func TestRunOnce_UpsertFails_NoStateUpdate(t *testing.T) {
	zip := &model.ZipFile{FileID: "file-1", ModifiedTime: time.Now()}
	src := &fakeSource{zip: zip}
	rd := &fakeReader{recs: map[string][]model.Record{"steps": {{UUID: "a"}}}}
	st := newFakeStore()
	st.upsertErr = errors.New("upsert boom")
	sink := &fakeSink{}

	a := New(testConfig(), src, rd, st, sink, discardLogger(), nil)
	err := a.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() error = nil, want error")
	}
	if len(st.setStateCalls) != 0 {
		t.Errorf("SetState calls = %d, want 0", len(st.setStateCalls))
	}
}

func TestRunOnce_WriteTabFails_NoStateUpdate(t *testing.T) {
	zip := &model.ZipFile{FileID: "file-1", ModifiedTime: time.Now()}
	src := &fakeSource{zip: zip}
	rd := &fakeReader{recs: map[string][]model.Record{}}
	st := newFakeStore()
	sink := &fakeSink{err: errors.New("write boom")}

	a := New(testConfig(), src, rd, st, sink, discardLogger(), nil)
	err := a.RunOnce(context.Background())
	if err == nil {
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
	a := New(testConfig(), src, &fakeReader{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

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
		t.Fatal("Run did not return after ctx cancel")
	}

	if len(src.calls) != 1 {
		t.Errorf("FetchLatest calls = %d, want 1", len(src.calls))
	}
}

func TestRun_ContinuesAfterRunOnceError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{err: errors.New("fetch boom")}
	a := New(testConfig(), src, &fakeReader{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

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
		t.Fatal("Run did not return after ctx cancel")
	}

	if len(src.calls) < 2 {
		t.Errorf("FetchLatest calls = %d, want >= 2", len(src.calls))
	}
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{zip: nil}
	a := New(testConfig(), src, &fakeReader{}, newFakeStore(), &fakeSink{}, discardLogger(), nil)

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
		t.Fatal("Run did not return after ctx cancel")
	}
}
