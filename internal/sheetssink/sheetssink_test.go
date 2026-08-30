package sheetssink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// fakeSheet はフェイクAPI内で管理する1タブぶんの状態。
type fakeSheet struct {
	id    int64
	rows  int64
	cols  int64
	index int64
}

// fakeSheetsAPI は Google Sheets API を模したインメモリのフェイク。
// 実機で確認した「values.update はグリッド範囲外への書き込みを自動拡張
// しない」という挙動を再現するため、書き込み範囲がタブの現在の
// グリッドサイズを超えたら本物のAPIと同じ400エラーを返す。
type fakeSheetsAPI struct {
	mu           sync.Mutex
	calls        []recordedCall
	sheets       map[string]*fakeSheet
	nextID       int64
	failAddSheet bool
}

// newFakeSheetsAPI は既存タブを Google Sheets の新規タブ既定サイズ
// （1000行 x 26列）で登録する。
func newFakeSheetsAPI(titles []string, failAddSheet bool) *fakeSheetsAPI {
	grids := make(map[string]fakeSheet, len(titles))
	for i, title := range titles {
		grids[title] = fakeSheet{rows: 1000, cols: 26, index: int64(i)}
	}
	return newFakeSheetsAPIWithGrids(grids, failAddSheet)
}

// newFakeSheetsAPIWithGrids はタブごとのグリッドサイズを指定して登録する。
// growIfNeeded のリサイズ挙動や、グリッド超過エラーの再現に使う。
func newFakeSheetsAPIWithGrids(grids map[string]fakeSheet, failAddSheet bool) *fakeSheetsAPI {
	f := &fakeSheetsAPI{sheets: make(map[string]*fakeSheet), nextID: 1, failAddSheet: failAddSheet}
	for title, g := range grids {
		grid := g
		if grid.id == 0 {
			grid.id = f.nextID
		}
		if grid.id >= f.nextID {
			f.nextID = grid.id + 1
		}
		f.sheets[title] = &grid
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

func (f *fakeSheetsAPI) gridOf(title string) (fakeSheet, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.sheets[title]
	if !ok {
		return fakeSheet{}, false
	}
	return *g, true
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
		f.handleValuesUpdate(w, r, body)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSheetsAPI) handleGet(w http.ResponseWriter) {
	f.mu.Lock()
	sheetsList := make([]map[string]any, 0, len(f.sheets))
	for title, g := range f.sheets {
		sheetsList = append(sheetsList, map[string]any{
			"properties": map[string]any{
				"sheetId": g.id,
				"title":   title,
				"index":   g.index,
				"gridProperties": map[string]any{
					"rowCount":    g.rows,
					"columnCount": g.cols,
				},
			},
		})
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"sheets": sheetsList})
}

type batchUpdateRequest struct {
	Requests []struct {
		AddSheet *struct {
			Properties struct {
				Title          string `json:"title"`
				GridProperties struct {
					RowCount    int64 `json:"rowCount"`
					ColumnCount int64 `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"addSheet"`
		UpdateSheetProperties *struct {
			Properties struct {
				SheetID        int64  `json:"sheetId"`
				Index          *int64 `json:"index"`
				GridProperties *struct {
					RowCount    int64 `json:"rowCount"`
					ColumnCount int64 `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
			Fields string `json:"fields"`
		} `json:"updateSheetProperties"`
	} `json:"requests"`
}

func (f *fakeSheetsAPI) handleBatchUpdate(w http.ResponseWriter, body []byte) {
	if f.failAddSheet {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": 400, "message": "simulated addSheet failure"},
		})
		return
	}

	var req batchUpdateRequest
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	var addedID int64
	for _, rq := range req.Requests {
		if rq.AddSheet != nil {
			id := f.nextID
			f.nextID++
			f.sheets[rq.AddSheet.Properties.Title] = &fakeSheet{
				id:    id,
				rows:  rq.AddSheet.Properties.GridProperties.RowCount,
				cols:  rq.AddSheet.Properties.GridProperties.ColumnCount,
				index: int64(len(f.sheets)),
			}
			addedID = id
		}
		if rq.UpdateSheetProperties != nil {
			p := rq.UpdateSheetProperties.Properties
			if p.GridProperties != nil {
				for _, g := range f.sheets {
					if g.id == p.SheetID {
						g.rows = p.GridProperties.RowCount
						g.cols = p.GridProperties.ColumnCount
					}
				}
			}
			if p.Index != nil {
				f.moveSheetLocked(p.SheetID, *p.Index)
			}
		}
	}
	f.mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]any{
		"replies": []map[string]any{{
			"addSheet": map[string]any{"properties": map[string]any{"sheetId": addedID}},
		}},
	})
}

// moveSheetLocked は sheetID のタブを newIndex へ移し、残りの index を詰め直す。
// 本物の Sheets API と同じく、タブ順は 0 から隙間なく振り直される。
// 呼び出し側が f.mu を保持していること。
func (f *fakeSheetsAPI) moveSheetLocked(sheetID, newIndex int64) {
	ordered := make([]*fakeSheet, 0, len(f.sheets))
	for _, g := range f.sheets {
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

	pos := -1
	for i, g := range ordered {
		if g.id == sheetID {
			pos = i
			break
		}
	}
	if pos < 0 {
		return
	}

	target := ordered[pos]
	ordered = append(ordered[:pos], ordered[pos+1:]...)

	newIndex = max(newIndex, 0)
	newIndex = min(newIndex, int64(len(ordered)))
	ordered = append(ordered, nil)
	copy(ordered[newIndex+1:], ordered[newIndex:])
	ordered[newIndex] = target

	for i, g := range ordered {
		g.index = int64(i)
	}
}

// titlesInOrder は現在のタブ順をタイトルの並びで返す。
func (f *fakeSheetsAPI) titlesInOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	type entry struct {
		title string
		index int64
	}
	entries := make([]entry, 0, len(f.sheets))
	for title, g := range f.sheets {
		entries = append(entries, entry{title: title, index: g.index})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].index < entries[j].index })

	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.title
	}
	return out
}

var rangeRe = regexp.MustCompile(`^(.*)!A(\d+)$`)

// handleValuesUpdate は本物の Sheets API と同じく、書き込み範囲がタブの
// 現在のグリッドサイズを超えていたら 400 を返す（自動拡張しない）。
func (f *fakeSheetsAPI) handleValuesUpdate(w http.ResponseWriter, r *http.Request, body []byte) {
	rangeParam := strings.TrimPrefix(r.URL.Path, "/v4/spreadsheets/"+testSpreadsheetID+"/values/")
	decoded, err := url.QueryUnescape(rangeParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var payload struct {
		Values [][]any `json:"values"`
	}
	_ = json.Unmarshal(body, &payload)

	title, startRow := parseRange(decoded)
	grid, ok := f.gridOf(title)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	endRow := startRow + int64(len(payload.Values)) - 1
	var maxCols int64
	for _, row := range payload.Values {
		if n := int64(len(row)); n > maxCols {
			maxCols = n
		}
	}
	if endRow > grid.rows || maxCols > grid.cols {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code": 400,
				"message": fmt.Sprintf(
					"Range (%s) exceeds grid limits. Max rows: %d, max columns: %d",
					decoded, grid.rows, grid.cols,
				),
			},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{})
}

// parseRange は "'title'!A123" 形式から title（クォート・エスケープ解除済み）
// と開始行番号を取り出す。
func parseRange(rangeStr string) (title string, startRow int64) {
	m := rangeRe.FindStringSubmatch(rangeStr)
	if m == nil {
		return "", 0
	}
	title = strings.TrimSuffix(strings.TrimPrefix(m[1], "'"), "'")
	title = strings.ReplaceAll(title, "''", "'")
	n, _ := strconv.ParseInt(m[2], 10, 64)
	return title, n
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

func TestWriteTab_NewSheetIsSizedForData(t *testing.T) {
	fake := newFakeSheetsAPI(nil, false)
	sink := newTestSink(t, fake)

	rows := make([][]any, 6000)
	for i := range rows {
		rows[i] = []any{i}
	}
	if err := sink.WriteTab(context.Background(), "steps_raw", rows); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	grid, ok := fake.gridOf("steps_raw")
	if !ok {
		t.Fatal("steps_raw not found")
	}
	if grid.rows < 6000 {
		t.Errorf("grid.rows = %d, want >= 6000", grid.rows)
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

func TestWriteTab_GrowsExistingSheetWhenTooSmall(t *testing.T) {
	fake := newFakeSheetsAPIWithGrids(map[string]fakeSheet{
		"steps_raw": {rows: 1000, cols: 26},
	}, false)
	sink := newTestSink(t, fake)

	rows := make([][]any, 6000)
	for i := range rows {
		rows[i] = []any{i}
	}
	if err := sink.WriteTab(context.Background(), "steps_raw", rows); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	if n := fake.countMatching(isBatchUpdate); n != 1 {
		t.Fatalf("batchUpdate called %d times, want 1 (resize)", n)
	}
	grid, _ := fake.gridOf("steps_raw")
	if grid.rows < 6000 {
		t.Errorf("grid.rows = %d, want >= 6000 after grow", grid.rows)
	}
}

func TestWriteTab_NoResizeWhenGridAlreadyBigEnough(t *testing.T) {
	fake := newFakeSheetsAPIWithGrids(map[string]fakeSheet{
		"existing_tab": {rows: 10000, cols: 26},
	}, false)
	sink := newTestSink(t, fake)

	if err := sink.WriteTab(context.Background(), "existing_tab", [][]any{{"a"}}); err != nil {
		t.Fatalf("WriteTab: %v", err)
	}

	if n := fake.countMatching(isBatchUpdate); n != 0 {
		t.Fatalf("batchUpdate called %d times, want 0 (no resize needed)", n)
	}
}

func TestWriteTab_WithoutResizeWouldFailAboveDefaultGrid(t *testing.T) {
	// リグレッション用の対照実験: リサイズをせずに1000行の既定グリッドへ
	// 5000行超を書こうとすると、フェイクAPIが本物と同じ400を返すことを
	// 確認する（=フェイクの模倣が正しいこと、かつ本番コードは常に事前リサイズ
	// するためこのエラーに到達しないことの裏付け）。
	fake := newFakeSheetsAPIWithGrids(map[string]fakeSheet{"t": {rows: 1000, cols: 26}}, false)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc, err := sheets.NewService(context.Background(),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("sheets.NewService: %v", err)
	}

	rows := make([][]any, 1500)
	for i := range rows {
		rows[i] = []any{i}
	}
	_, err = svc.Spreadsheets.Values.Update(testSpreadsheetID, "'t'!A1", &sheets.ValueRange{Values: rows}).
		ValueInputOption("RAW").Do()
	if err == nil {
		t.Fatal("want error writing beyond grid limit without resize, got nil")
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

	if _, cached := sink.sheets["failing_tab"]; cached {
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

func TestMoveTabFirst_MovesTabToIndexZero(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"other", "daily_summary"}, false)
	sink := newTestSink(t, fake)

	if err := sink.MoveTabFirst(context.Background(), "daily_summary"); err != nil {
		t.Fatalf("MoveTabFirst: %v", err)
	}

	want := []string{"daily_summary", "other"}
	if got := fake.titlesInOrder(); !slices.Equal(got, want) {
		t.Errorf("tab order = %v, want %v", got, want)
	}
}

// 人手で先頭にシートを挿入されてもキャッシュは追従しないため、現在の index を
// 見て「既に先頭」と判断すると是正が永久に飛ぶ。毎回無条件に送ること。
func TestMoveTabFirst_SendsRequestEvenWhenAlreadyFirst(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"daily_summary", "other"}, false)
	sink := newTestSink(t, fake)

	for i := range 2 {
		if err := sink.MoveTabFirst(context.Background(), "daily_summary"); err != nil {
			t.Fatalf("MoveTabFirst #%d: %v", i+1, err)
		}
	}

	if n := fake.countMatching(isBatchUpdate); n != 2 {
		t.Errorf("batchUpdate calls = %d, want 2", n)
	}
}

// Index は omitempty のため、ForceSendFields を怠ると 0 が JSON から消える。
func TestMoveTabFirst_SendsExplicitZeroIndex(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"other", "daily_summary"}, false)
	sink := newTestSink(t, fake)

	if err := sink.MoveTabFirst(context.Background(), "daily_summary"); err != nil {
		t.Fatalf("MoveTabFirst: %v", err)
	}

	var body []byte
	for _, c := range fake.callsSnapshot() {
		if isBatchUpdate(c) {
			body = c.body
		}
	}
	if body == nil {
		t.Fatal("batchUpdate request not sent")
	}

	var req batchUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if len(req.Requests) != 1 || req.Requests[0].UpdateSheetProperties == nil {
		t.Fatalf("unexpected request body: %s", body)
	}

	usp := req.Requests[0].UpdateSheetProperties
	if usp.Properties.Index == nil {
		t.Fatalf("index omitted from request body: %s", body)
	}
	if *usp.Properties.Index != 0 {
		t.Errorf("index = %d, want 0", *usp.Properties.Index)
	}
	if usp.Fields != "index" {
		t.Errorf("fields = %q, want %q", usp.Fields, "index")
	}
}

func TestMoveTabFirst_MissingTabIsNoOp(t *testing.T) {
	fake := newFakeSheetsAPI([]string{"other"}, false)
	sink := newTestSink(t, fake)

	if err := sink.MoveTabFirst(context.Background(), "daily_summary"); err != nil {
		t.Fatalf("MoveTabFirst: %v", err)
	}

	if n := fake.countMatching(isBatchUpdate); n != 0 {
		t.Errorf("batchUpdate calls = %d, want 0", n)
	}
}

func TestMoveTabFirst_DoesNotResizeGrid(t *testing.T) {
	fake := newFakeSheetsAPIWithGrids(map[string]fakeSheet{
		"daily_summary": {rows: 4000, cols: 30, index: 1},
		"other":         {rows: 1000, cols: 26, index: 0},
	}, false)
	sink := newTestSink(t, fake)

	if err := sink.MoveTabFirst(context.Background(), "daily_summary"); err != nil {
		t.Fatalf("MoveTabFirst: %v", err)
	}

	grid, ok := fake.gridOf("daily_summary")
	if !ok {
		t.Fatal("daily_summary missing")
	}
	if grid.rows != 4000 || grid.cols != 30 {
		t.Errorf("grid = %dx%d, want 4000x30", grid.rows, grid.cols)
	}
}
