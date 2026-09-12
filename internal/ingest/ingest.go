// Package ingest はエクスポートZIPから累積DBへの取り込みを組み立てる。
//
// ZIPの展開・エクスポートDBの開閉・付随情報（アプリ優先度、全テーブルの行数）の
// 読み出しを担い、種別ごとのデータは各種別へ任せる（ADR 0012）。
package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"health-connect-converter/internal/hcreader"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// Store は取り込み先。internal/store が満たす。
type Store interface {
	SetAppPriorities(ctx context.Context, prios model.AppPriorities) error
	Ingest(ctx context.Context, k kind.Kind, exportDB *sql.DB) (int, error)
}

// Ingester は1回ぶんの取り込みを行う。
type Ingester struct {
	Kinds []kind.Kind
	Store Store
	// TempDir はZIPを展開する一時ファイルの置き場。
	TempDir string
	// Logger は nil なら slog.Default() を使う。
	Logger *slog.Logger
}

// Ingest は zip を展開して全種別を取り込み、付随情報を返す。
// 展開した一時ファイルは必ず削除する。
func (i *Ingester) Ingest(ctx context.Context, zip *model.ZipFile) (*model.ExportInfo, error) {
	logger := i.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := os.MkdirAll(i.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("ingest: mkdir %s: %w", i.TempDir, err)
	}
	tmp, err := os.CreateTemp(i.TempDir, "export-*.db")
	if err != nil {
		return nil, fmt.Errorf("ingest: create temp file: %w", err)
	}
	dbPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(dbPath) }()

	if err := hcreader.ExtractDB(zip.Data, dbPath); err != nil {
		return nil, err
	}

	db, err := hcreader.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	prios, err := hcreader.ReadAppPriorities(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := i.Store.SetAppPriorities(ctx, prios); err != nil {
		return nil, err
	}
	logger.Info("アプリ優先度を取得", "categories", len(prios))

	tableRows, err := hcreader.ReadTableRows(ctx, db, logger)
	if err != nil {
		return nil, err
	}

	for _, k := range i.Kinds {
		n, err := i.Store.Ingest(ctx, k, db)
		if err != nil {
			return nil, err
		}
		logger.Info("取り込み完了", "type", k.Key(), "count", n)
	}

	return &model.ExportInfo{TableRows: tableRows}, nil
}
