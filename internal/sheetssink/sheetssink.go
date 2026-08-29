// Package sheetssink は既存のGoogleスプレッドシートのタブへ表形式データを書き込む。
// 新規スプレッドシートは作らない。サービスアカウントは自身の保存容量を持たず、
// 新規ファイル作成は容量エラーになるが、アクセス権のある既存ファイル内のタブ
// 追加・更新は問題なく行える。
package sheetssink

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// maxRowsPerRequest は spreadsheets.values.update 1回あたりに送る最大行数。
const maxRowsPerRequest = 5000

// Sink は1個の既存スプレッドシートのタブへ行を書き込む。
type Sink struct {
	svc           *sheets.Service
	spreadsheetID string
	sheetTitles   map[string]bool
	titlesLoaded  bool
}

// New はサービスアカウント鍵で認証済みの Sink を作る。
func New(ctx context.Context, saKeyPath, spreadsheetID string) (*Sink, error) {
	svc, err := sheets.NewService(ctx,
		option.WithCredentialsFile(saKeyPath), //nolint:staticcheck // サービスアカウント鍵ファイルのパスを直接受け取る仕様のため使用
		option.WithScopes(sheets.SpreadsheetsScope),
	)
	if err != nil {
		return nil, fmt.Errorf("sheetssink: create sheets client: %w", err)
	}
	return newWithService(svc, spreadsheetID), nil
}

// newWithService は構築済みの sheets.Service から Sink を作る。
// テストで httptest サーバーへ向けるために使う。
func newWithService(svc *sheets.Service, spreadsheetID string) *Sink {
	return &Sink{
		svc:           svc,
		spreadsheetID: spreadsheetID,
		sheetTitles:   make(map[string]bool),
	}
}

// WriteTab は title という名前のタブへ rows を書く。タブが無ければ作り、
// 書く前に既存内容を全消しする。rows が空でもタブの作成・全消しは行う。
func (s *Sink) WriteTab(ctx context.Context, title string, rows [][]any) error {
	if err := s.ensureTitlesLoaded(ctx); err != nil {
		return err
	}

	if !s.sheetTitles[title] {
		if err := s.addSheet(ctx, title); err != nil {
			return err
		}
	}

	quotedTitle := quoteSheetTitle(title)

	if _, err := s.svc.Spreadsheets.Values.Clear(s.spreadsheetID, quotedTitle, &sheets.ClearValuesRequest{}).
		Context(ctx).Do(); err != nil {
		return fmt.Errorf("sheetssink: clear tab %q: %w", title, err)
	}

	for start := 0; start < len(rows); start += maxRowsPerRequest {
		end := min(start+maxRowsPerRequest, len(rows))
		chunk := rows[start:end]

		rangeStr := fmt.Sprintf("%s!A%d", quotedTitle, start+1)
		valueRange := &sheets.ValueRange{Values: chunk}

		if _, err := s.svc.Spreadsheets.Values.Update(s.spreadsheetID, rangeStr, valueRange).
			ValueInputOption("RAW").
			Context(ctx).Do(); err != nil {
			return fmt.Errorf("sheetssink: update tab %q range %q: %w", title, rangeStr, err)
		}
	}

	return nil
}

// ensureTitlesLoaded は既存タブ名を初回だけ取得しキャッシュする。
func (s *Sink) ensureTitlesLoaded(ctx context.Context) error {
	if s.titlesLoaded {
		return nil
	}

	spreadsheet, err := s.svc.Spreadsheets.Get(s.spreadsheetID).
		Fields("sheets(properties(title))").
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("sheetssink: get spreadsheet: %w", err)
	}

	for _, sh := range spreadsheet.Sheets {
		if sh.Properties != nil {
			s.sheetTitles[sh.Properties.Title] = true
		}
	}
	s.titlesLoaded = true
	return nil
}

// addSheet は title という名前のタブを新規作成し、成功したらキャッシュへ足す。
func (s *Sink) addSheet(ctx context.Context, title string) error {
	req := &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{Title: title},
				},
			},
		},
	}

	if _, err := s.svc.Spreadsheets.BatchUpdate(s.spreadsheetID, req).Context(ctx).Do(); err != nil {
		return fmt.Errorf("sheetssink: add sheet %q: %w", title, err)
	}

	s.sheetTitles[title] = true
	return nil
}

// quoteSheetTitle はタブ名をA1形式レンジ内で使える形にする。
// タブ名に含まれるシングルクォートは2つに重ねてエスケープする。
func quoteSheetTitle(title string) string {
	return "'" + strings.ReplaceAll(title, "'", "''") + "'"
}
