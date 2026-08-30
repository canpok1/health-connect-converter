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

// defaultGridCols は新規タブの列数の下限。Google Sheets のデフォルトタブ幅に合わせる。
const defaultGridCols = 26

// sheetMeta はキャッシュ済みのタブのID・現在のグリッドサイズ。
type sheetMeta struct {
	id   int64
	rows int64
	cols int64
}

// Sink は1個の既存スプレッドシートのタブへ行を書き込む。
type Sink struct {
	svc           *sheets.Service
	spreadsheetID string
	sheets        map[string]sheetMeta
	loaded        bool
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
		sheets:        make(map[string]sheetMeta),
	}
}

// WriteTab は title という名前のタブへ rows を書く。タブが無ければ作り、
// 書く前に既存内容を全消しする。rows が空でもタブの作成・全消しは行う。
//
// values.update はグリッドの範囲外への書き込みを自動拡張しない（実機検証で
// 確認済み：既定サイズのタブへ5000行超を書くと2チャンク目以降が
// "exceeds grid limits" で失敗する）。そのため書き込み前に必要な行数・列数を
// 自分で計算し、グリッドが足りなければ明示的にリサイズする。
func (s *Sink) WriteTab(ctx context.Context, title string, rows [][]any) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}

	meta, exists := s.sheets[title]
	if !exists {
		var err error
		meta, err = s.addSheet(ctx, title, neededRows(rows), neededCols(rows))
		if err != nil {
			return err
		}
	} else if err := s.growIfNeeded(ctx, title, &meta, neededRows(rows), neededCols(rows)); err != nil {
		return err
	}
	s.sheets[title] = meta

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

// neededRows は書き込みに必要な行数。0行のときも既存グリッドを壊さないよう
// 最低1を返す。
func neededRows(rows [][]any) int64 {
	if len(rows) == 0 {
		return 1
	}
	return int64(len(rows))
}

// neededCols は書き込みに必要な列数。全行の中で最大の列数を採る
// （通常はヘッダ行と一致する）。
func neededCols(rows [][]any) int64 {
	var max int64
	for _, r := range rows {
		if n := int64(len(r)); n > max {
			max = n
		}
	}
	if max < defaultGridCols {
		return defaultGridCols
	}
	return max
}

// ensureLoaded は既存タブのID・グリッドサイズを初回だけ取得しキャッシュする。
func (s *Sink) ensureLoaded(ctx context.Context) error {
	if s.loaded {
		return nil
	}

	spreadsheet, err := s.svc.Spreadsheets.Get(s.spreadsheetID).
		Fields("sheets(properties(sheetId,title,gridProperties(rowCount,columnCount)))").
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("sheetssink: get spreadsheet: %w", err)
	}

	for _, sh := range spreadsheet.Sheets {
		if sh.Properties == nil {
			continue
		}
		meta := sheetMeta{id: sh.Properties.SheetId}
		if gp := sh.Properties.GridProperties; gp != nil {
			meta.rows = gp.RowCount
			meta.cols = gp.ColumnCount
		}
		s.sheets[sh.Properties.Title] = meta
	}
	s.loaded = true
	return nil
}

// addSheet は title という名前のタブを rows x cols のグリッドで新規作成し、
// 成功したらキャッシュへ足す。
func (s *Sink) addSheet(ctx context.Context, title string, rows, cols int64) (sheetMeta, error) {
	req := &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{
						Title:          title,
						GridProperties: &sheets.GridProperties{RowCount: rows, ColumnCount: cols},
					},
				},
			},
		},
	}

	resp, err := s.svc.Spreadsheets.BatchUpdate(s.spreadsheetID, req).Context(ctx).Do()
	if err != nil {
		return sheetMeta{}, fmt.Errorf("sheetssink: add sheet %q: %w", title, err)
	}
	if len(resp.Replies) == 0 || resp.Replies[0].AddSheet == nil || resp.Replies[0].AddSheet.Properties == nil {
		// sheetId が取れないと後続のリサイズが別タブ（ID 0）を誤って
		// 更新しうるため、0埋めせず即エラーにする。
		return sheetMeta{}, fmt.Errorf("sheetssink: add sheet %q: response missing sheetId", title)
	}

	return sheetMeta{
		id:   resp.Replies[0].AddSheet.Properties.SheetId,
		rows: rows,
		cols: cols,
	}, nil
}

// growIfNeeded はタブの現在のグリッドが rows x cols に満たなければ拡張する。
// 縮小はしない。
func (s *Sink) growIfNeeded(ctx context.Context, title string, meta *sheetMeta, rows, cols int64) error {
	newRows := max(meta.rows, rows)
	newCols := max(meta.cols, cols)
	if newRows == meta.rows && newCols == meta.cols {
		return nil
	}

	req := &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
					Properties: &sheets.SheetProperties{
						SheetId:        meta.id,
						GridProperties: &sheets.GridProperties{RowCount: newRows, ColumnCount: newCols},
					},
					Fields: "gridProperties.rowCount,gridProperties.columnCount",
				},
			},
		},
	}
	if _, err := s.svc.Spreadsheets.BatchUpdate(s.spreadsheetID, req).Context(ctx).Do(); err != nil {
		return fmt.Errorf("sheetssink: resize tab %q: %w", title, err)
	}

	meta.rows = newRows
	meta.cols = newCols
	return nil
}

// quoteSheetTitle はタブ名をA1形式レンジ内で使える形にする。
// タブ名に含まれるシングルクォートは2つに重ねてエスケープする。
func quoteSheetTitle(title string) string {
	return "'" + strings.ReplaceAll(title, "'", "''") + "'"
}
