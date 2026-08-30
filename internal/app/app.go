// Package app は取得→読み出し→UPSERT→出力生成→書き込み→state更新の1周と、
// それを繰り返す常駐ループを持つ中核パッケージ。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"health-connect-converter/internal/config"
	"health-connect-converter/internal/model"
	"health-connect-converter/internal/report"
)

// state のキー。
const (
	stateKeyLastProcessedModifiedTime = "last_processed_modified_time"
	stateKeyLastProcessedFileID       = "last_processed_file_id"
	stateKeyLastSuccessAt             = "last_success_at"
)

// Source はエクスポートZIPの取得元。internal/drivesource が満たす。
type Source interface {
	FetchLatest(ctx context.Context, after time.Time) (*model.ZipFile, error)
}

// Reader はZIPから種別ごとのレコードを読み出す。internal/hcreader が満たす。
type Reader interface {
	Read(zip *model.ZipFile, cfg *config.Config) (map[string][]model.Record, error)
}

// Store は累積DBと state の永続化。internal/store が満たす。
// report.Querier のメソッドを含むため、そのまま report.Querier として渡せる。
type Store interface {
	UpsertRecords(ctx context.Context, typeKey string, tc config.TypeConfig, recs []model.Record) (int, error)
	DailyAggregates(ctx context.Context, typeKey string, tc config.TypeConfig) ([]model.DailyRow, error)
	RecordsSince(ctx context.Context, typeKey string, tc config.TypeConfig, sinceMs int64) ([]model.Record, error)
	TypeStats(ctx context.Context, typeKey string) (model.TypeStats, error)
	GetState(ctx context.Context, key string) (string, error)
	SetState(ctx context.Context, key, value string) error
}

// Sink はスプレッドシートへの書き込み先。internal/sheetssink が満たす。
type Sink interface {
	WriteTab(ctx context.Context, title string, rows [][]any) error
	MoveTabFirst(ctx context.Context, title string) error
}

// App は1周ぶんの処理とポーリングループを持つ。
type App struct {
	cfg    *config.Config
	src    Source
	rd     Reader
	st     Store
	sink   Sink
	logger *slog.Logger
	now    func() time.Time
}

// New はAppを組み立てる。now が nil なら time.Now を使う。
func New(cfg *config.Config, src Source, rd Reader, st Store, sink Sink, logger *slog.Logger, now func() time.Time) *App {
	if now == nil {
		now = time.Now
	}
	return &App{
		cfg:    cfg,
		src:    src,
		rd:     rd,
		st:     st,
		sink:   sink,
		logger: logger,
		now:    now,
	}
}

// RunOnce は1周ぶんの処理をする。新着が無ければ取り込みは行わず、
// daily_summary を先頭タブへ戻す是正だけを行う。
func (a *App) RunOnce(ctx context.Context) error {
	started := time.Now()

	after, err := a.lastProcessedModifiedTime(ctx)
	if err != nil {
		return err
	}

	zip, err := a.src.FetchLatest(ctx, after)
	if err != nil {
		return fmt.Errorf("app: fetch latest: %w", err)
	}
	if zip == nil {
		a.logger.Info("新着なし", "after", after)
		return a.ensureDailySummaryFirst(ctx)
	}
	a.logger.Info("ZIP取得",
		"file_id", zip.FileID,
		"name", zip.Name,
		"modified_time", zip.ModifiedTime,
		"size", len(zip.Data),
	)

	recsByType, err := a.rd.Read(zip, a.cfg)
	if err != nil {
		return fmt.Errorf("app: read zip: %w", err)
	}

	for _, tk := range a.cfg.TypeKeys() {
		recs, ok := recsByType[tk]
		if !ok {
			continue
		}
		n, err := a.st.UpsertRecords(ctx, tk, a.cfg.Types[tk], recs)
		if err != nil {
			return fmt.Errorf("app: upsert records for %q: %w", tk, err)
		}
		a.logger.Info("UPSERT完了", "type", tk, "count", n)
	}

	if err := a.writeDailySummary(ctx); err != nil {
		return err
	}
	if err := a.writeRawTabs(ctx); err != nil {
		return err
	}

	lastSuccess := a.now()
	if err := a.writeMeta(ctx, lastSuccess, zip.ModifiedTime); err != nil {
		return err
	}

	// writeDailySummary がタブを作った後に呼ぶ。初回実行ではこの位置でしか移動できない。
	if err := a.ensureDailySummaryFirst(ctx); err != nil {
		return err
	}

	if err := a.updateState(ctx, zip, lastSuccess); err != nil {
		return err
	}

	a.logger.Info("1周完了", "duration", time.Since(started))
	return nil
}

func (a *App) lastProcessedModifiedTime(ctx context.Context) (time.Time, error) {
	v, err := a.st.GetState(ctx, stateKeyLastProcessedModifiedTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("app: get state %q: %w", stateKeyLastProcessedModifiedTime, err)
	}
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		a.logger.Warn("stateのパースに失敗", "key", stateKeyLastProcessedModifiedTime, "value", v)
		return time.Time{}, nil
	}
	return t, nil
}

func (a *App) writeDailySummary(ctx context.Context) error {
	rows, err := report.BuildDailySummary(ctx, a.st, a.cfg)
	if err != nil {
		return fmt.Errorf("app: build daily summary: %w", err)
	}
	if err := a.sink.WriteTab(ctx, report.DailySummaryTitle, rows); err != nil {
		return fmt.Errorf("app: write tab %q: %w", report.DailySummaryTitle, err)
	}
	a.logger.Info("タブ書き込み", "tab", report.DailySummaryTitle, "rows", len(rows))
	return nil
}

func (a *App) writeRawTabs(ctx context.Context) error {
	for _, tk := range a.cfg.TypeKeys() {
		rows, err := report.BuildRawTab(ctx, a.st, a.cfg, tk, a.now())
		if err != nil {
			return fmt.Errorf("app: build raw tab for %q: %w", tk, err)
		}
		title := report.RawTabTitle(tk)
		if err := a.sink.WriteTab(ctx, title, rows); err != nil {
			return fmt.Errorf("app: write tab %q: %w", title, err)
		}
		a.logger.Info("タブ書き込み", "tab", title, "rows", len(rows))
	}
	return nil
}

// ensureDailySummaryFirst は daily_summary を先頭タブへ戻す。
//
// Drive の text/csv エクスポートは先頭タブだけを返すため、Claude が読むのは
// 実質この1タブになる（ADR 0007）。人手でシートが先頭へ挿入されると読み先が
// すり替わるので、新着ZIPの有無によらず毎周回で呼ぶ。
func (a *App) ensureDailySummaryFirst(ctx context.Context) error {
	if err := a.sink.MoveTabFirst(ctx, report.DailySummaryTitle); err != nil {
		return fmt.Errorf("app: move tab %q to first: %w", report.DailySummaryTitle, err)
	}
	return nil
}

func (a *App) writeMeta(ctx context.Context, lastSuccess, zipModified time.Time) error {
	rows, err := report.BuildMeta(ctx, a.st, a.cfg, lastSuccess, zipModified)
	if err != nil {
		return fmt.Errorf("app: build meta: %w", err)
	}
	if err := a.sink.WriteTab(ctx, report.MetaTitle, rows); err != nil {
		return fmt.Errorf("app: write tab %q: %w", report.MetaTitle, err)
	}
	a.logger.Info("タブ書き込み", "tab", report.MetaTitle, "rows", len(rows))
	return nil
}

func (a *App) updateState(ctx context.Context, zip *model.ZipFile, lastSuccess time.Time) error {
	states := []struct{ key, value string }{
		{stateKeyLastProcessedModifiedTime, zip.ModifiedTime.Format(time.RFC3339)},
		{stateKeyLastProcessedFileID, zip.FileID},
		{stateKeyLastSuccessAt, lastSuccess.Format(time.RFC3339)},
	}
	for _, s := range states {
		if err := a.st.SetState(ctx, s.key, s.value); err != nil {
			return fmt.Errorf("app: set state %q: %w", s.key, err)
		}
	}
	return nil
}

// Run は起動直後に1回 RunOnce してから interval ごとに繰り返す。
// RunOnce のエラーはログに出して握りつぶし、ループは続行する。
// ただし ctx のキャンセルに起因するエラーは抜ける理由にする。
func (a *App) Run(ctx context.Context, interval time.Duration) error {
	if err := a.runOnceLogged(ctx, interval); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.runOnceLogged(ctx, interval); err != nil {
				return err
			}
		}
	}
}

// runOnceLogged は RunOnce を実行し、失敗時はログを出す。
// エラーが ctx のキャンセルに起因する場合のみ、そのエラーを呼び出し元へ返す。
func (a *App) runOnceLogged(ctx context.Context, interval time.Duration) error {
	err := a.RunOnce(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	a.logger.Error("1周の処理に失敗", "error", err, "interval", interval)
	return nil
}
