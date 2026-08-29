package sheetssink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

const testSpreadsheetID = "test-spreadsheet-id"

// recordedCall はフェイクSheets APIサーバーが受けたHTTPリクエスト1件を記録する。
type recordedCall struct {
	method string
	path   string
	query  string
	body   []byte
}

// fakeSheetsAPI は Google Sheets API を模したインメモリのフェイク。
// 既存タブ名と受けたリクエストをすべて記録する。
type fakeSheetsAPI struct {
	mu           sync.Mutex
	calls        []recordedCall
	titles       map[string]bool
	failAddSheet bool
}

func newFakeSheetsAPI(initialTitles []string, failAddSheet bool) *fakeSheetsAPI {
	f := &fakeSheetsAPI{titles: make(map[string]bool), failAddSheet: failAddSheet}
	for _, title := range initialTitles {
		f.titles[title] = true
	}
	return f
}

func (f *fakeSheetsAPI) callsSnapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeSheetsAPI) countMatching(pred func(recordedCall) bool) int {
	n := 0
	for _, c := range f.callsSnapshot() {
		if pred(c) {
			n++
		}
	}
	return n
}

func (f *fakeSheetsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		body:   body,
	})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v4/spreadsheets/"), "/"):
		f.handleGet(w)
	case strings.HasSuffix(r.URL.Path, ":batchUpdate"):
		f.handleBatchUpdate(w, body)
	case strings.HasSuffix(r.URL.Path, ":clear"):
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case r.Method == http.MethodPut:
		_ = json.NewEncoder(w).Encode(map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSheetsAPI) handleGet(w http.ResponseWriter) {
	f.mu.Lock()
	sheetsList := make([]map[string]any, 0, len(f.titles))
	for title := range f.titles {
		sheetsList = append(sheetsList, map[string]any{
			"properties": map[string]any{"title": title},
		})
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"sheets": sheetsList})
}

func (f *fakeSheetsAPI) handleBatchUpdate(w http.ResponseWriter, body []byte) {
	if f.failAddSheet {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": 400, "message": "simulated addSheet failure"},
		})
		return
	}

	var req struct {
		Requests []struct {
			AddSheet *struct {
				Properties struct {
					Title string `json:"title"`
				} `json:"properties"`
			} `json:"addSheet"`
		} `json:"requests"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	for _, rq := range req.Requests {
		if rq.AddSheet != nil {
			f.titles[rq.AddSheet.Properties.Title] = true
		}
	}
	f.mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]any{})
}

// newTestSink は fake を backend にした httptest サーバーへ Sink を繋ぐ。
func newTestSink(t *testing.T, fake *fakeSheetsAPI) *Sink {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	svc, err := sheets.NewService(context.Background(),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("sheets.NewService: %v", err)
	}
	return newWithService(svc, testSpreadsheetID)
}

func isGet(c recordedCall) bool {
	return c.method == http.MethodGet
}

func isBatchUpdate(c recordedCall) bool {
	return strings.HasSuffix(c.path, ":batchUpdate")
}

func isClear(c recordedCall) bool {
	return strings.HasSuffix(c.path, ":clear")
}

func isUpdate(c recordedCall) bool {
	return c.method == http.MethodPut
}

func TestWriteTab_CreatesSheetWhenMissing(t *testing.T) {
	fake := newFakeSheetsAPI(nil, false)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "new_tab", [][]any{{"a", "b"}}); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	calls := fake.callsSnapshot()
	var kinds []string
	for _, c := range calls {
		switch {
		case isGet(c):
			kinds = append(kinds, "get")
		case isBatchUpdate(c):
			kinds = append(kinds, "batchUpdate")
		case isClear(c):
			kinds = append(kinds, "clear")
		case isUpdate(c):
			kinds = append(kinds, "update")
		}
	}
	want := []string{"get", "batchUpdate", "clear", "update"}
	if len(kinds) != len(want) {
		t.Fatalf("call sequence = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v", kinds, want)
		}
	}
}

func TestWriteTab_ExistingSheetSkipsAddSheet(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"existing_tab"}, false)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "existing_tab", [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	if n := fake.countMatching(isBatchUpdate); n != 0 {
		t.Fatalf("batchUpdate called %d times, want 0", n)
	}
	if n := fake.countMatching(isClear); n != 1 {
		t.Fatalf("clear called %d times, want 1", n)
	}
	if n := fake.countMatching(isUpdate); n != 1 {
		t.Fatalf("update called %d times, want 1", n)
	}
}

func TestWriteTab_CachesSpreadsheetGetAcrossCalls(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"existing_tab"}, false)
	sink := newTestSink(t, fake)
	ctx := context.Background()

	if err := sink.WriteTab(ctx, "existing_tab", [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab #1: %v", err)
	}
	if err := sink.WriteTab(ctx, "existing_tab", [][]any{{"b"}}); err != nil {
		t.Fatalf("WriteTab #2: %v", err)
	}

	if n := fake.countMatching(isGet); n != 1 {
		t.Fatalf("spreadsheets.get called %d times, want 1", n)
	}
}

func TestWriteTab_CachesAddSheetAcrossCalls(t *testing.T) {
	fake := newFakeSheetsAPI(nil, false)
	sink := newTestSink(t, fake)
	ctx := context.Background()

	if err := sink.WriteTab(ctx, "brand_new", [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab #1: %v", err)
	}
	if err := sink.WriteTab(ctx, "brand_new", [][]any{{"b"}}); err != nil {
		t.Fatalf("WriteTab #2: %v", err)
	}

	if n := fake.countMatching(isBatchUpdate); n != 1 {
		t.Fatalf("batchUpdate called %d times, want 1", n)
	}
}

func TestWriteTab_ChunksRowsInto5000RowBatches(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"big_tab"}, false)
	sink := newTestSink(t, fake)

	rows := make([][]any, 12000)
	for i := range rows {
		rows[i] = []any{i}
	}

	if err := sink.WriteTab(context.Background(), "big_tab", rows); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	var updateRanges []string
	for _, c := range fake.callsSnapshot() {
		if isUpdate(c) {
			updateRanges = append(updateRanges, c.path)
		}
	}
	if len(updateRanges) != 3 {
		t.Fatalf("update called %d times, want 3 (ranges: %v)", len(updateRanges), updateRanges)
	}
	wantSuffixes := []string{"!A1", "!A5001", "!A10001"}
	for i, suffix := range wantSuffixes {
		if !strings.HasSuffix(updateRanges[i], suffix) {
			t.Fatalf("update range #%d = %q, want suffix %q", i, updateRanges[i], suffix)
		}
	}
}

func TestWriteTab_EmptyRowsStillClearsButSkipsUpdate(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"empty_tab"}, false)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "empty_tab", nil); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	if n := fake.countMatching(isClear); n != 1 {
		t.Fatalf("clear called %d times, want 1", n)
	}
	if n := fake.countMatching(isUpdate); n != 0 {
		t.Fatalf("update called %d times, want 0", n)
	}
}

func TestWriteTab_UsesRawValueInputOption(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"raw_tab"}, false)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "raw_tab", [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	found := false
	for _, c := range fake.callsSnapshot() {
		if isUpdate(c) {
			found = true
			if !strings.Contains(c.query, "valueInputOption=RAW") {
				t.Fatalf("update query = %q, want valueInputOption=RAW", c.query)
			}
		}
	}
	if !found {
		t.Fatal("no update call recorded")
	}
}

func TestWriteTab_AddSheetErrorNotCached(t *testing.T) {
	fake := newFakeSheetsAPI(nil, true)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "failing_tab", [][]any{{"a"}}); err == nil {
		t.Fatal("WriteTab: want error, got nil")
	}

	if sink.sheetTitles["failing_tab"] {
		t.Fatal("failing_tab should not be cached after addSheet error")
	}
	if n := fake.countMatching(isClear); n != 0 {
		t.Fatalf("clear called %d times, want 0 after addSheet error", n)
	}
	if n := fake.countMatching(isUpdate); n != 0 {
		t.Fatalf("update called %d times, want 0 after addSheet error", n)
	}
}

func TestQuoteSheetTitle(t *testing.T) {
	cases := map[string]string{
		"bp_raw":       "'bp_raw'",
		"_meta":        "'_meta'",
		"it's a tab":   "'it''s a tab'",
		"''":           "''''''",
		"no_quote_tab": "'no_quote_tab'",
	}
	for in, want := range cases {
		if got := quoteSheetTitle(in); got != want {
			t.Errorf("quoteSheetTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteTab_EscapesSingleQuoteInTitleForRequestRanges(t *testing.T) {
	fake := newFakeSheetsAPI(nil, false)
	sink := newTestSink(t, fake)

	title := "it's a tab"
	if err := sink.WriteTab(context.Background(), title, [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	wantRangePrefix := "'it''s a tab'"
	for _, c := range fake.callsSnapshot() {
		switch {
		case isClear(c):
			if !strings.Contains(c.path, wantRangePrefix) {
				t.Errorf("clear path = %q, want to contain %q", c.path, wantRangePrefix)
			}
		case isUpdate(c):
			if !strings.Contains(c.path, wantRangePrefix) {
				t.Errorf("update path = %q, want to contain %q", c.path, wantRangePrefix)
			}
		}
	}
}
