// Package app は取得→読み出し→UPSERT→出力生成→書き込み→state更新の1周と、
// それを繰り返す常駐ループを持つ中核パッケージ。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"health-connect-converter/internal/kind"
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

// Ingester はZIPを展開して全種別を累積DBへ取り込み、付随情報を返す。
// internal/ingest が満たす。
type Ingester interface {
	Ingest(ctx context.Context, zip *model.ZipFile) (*model.ExportInfo, error)
}

// Store は累積DBと state の永続化。internal/store が満たす。
// report.Querier のメソッドを含むため、そのまま report.Querier として渡せる。
type Store interface {
	DailyAggregates(ctx context.Context, k kind.Kind) ([]model.DailyRow, error)
	RawRows(ctx context.Context, k kind.Kind, sinceMs int64) ([][]any, error)
	Stats(ctx context.Context, k kind.Kind) (model.TypeStats, error)
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
	kinds  []kind.Kind
	src    Source
	ing    Ingester
	st     Store
	sink   Sink
	logger *slog.Logger
	now    func() time.Time

	// startupIngestDone はプロセス起動後の強制取り込みが済んだかを持つ。
	// Watchtower はイメージ更新時にコンテナを作り直すため、起動は設定・コードの
	// 変更とほぼ同義になる。最初の1周だけ新着判定を省いて取り込むことで、
	// 変更検知の仕組みを足さずに「変えたら次の1周で反映される」を得る（ADR 0010）。
	startupIngestDone bool
}

// New はAppを組み立てる。now が nil なら time.Now を使う。
func New(kinds []kind.Kind, src Source, ing Ingester, st Store, sink Sink, logger *slog.Logger, now func() time.Time) *App {
	if now == nil {
		now = time.Now
	}
	return &App{
		kinds:  kinds,
		src:    src,
		ing:    ing,
		st:     st,
		sink:   sink,
		logger: logger,
		now:    now,
	}
}

// RunOnce は1周ぶんの処理をする。起動後の最初の1周は新着ZIPの有無によらず
// 取り込む。以降は新着が無ければ取り込みを行わず、daily_summary を先頭タブへ
// 戻す是正だけを行う。
func (a *App) RunOnce(ctx context.Context) error {
	started := time.Now()

	after, err := a.lastProcessedModifiedTime(ctx)
	if err != nil {
		return err
	}
	if !a.startupIngestDone {
		a.logger.Info("起動後の初回のため新着判定を省略", "last_processed", after)
		after = time.Time{}
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

	info, err := a.ing.Ingest(ctx, zip)
	if err != nil {
		return fmt.Errorf("app: ingest: %w", err)
	}

	if err := a.writeDailySummary(ctx); err != nil {
		return err
	}
	if err := a.writeRawTabs(ctx); err != nil {
		return err
	}

	lastSuccess := a.now()
	if err := a.writeMeta(ctx, lastSuccess, zip.ModifiedTime, info.TableRows); err != nil {
		return err
	}

	// writeDailySummary がタブを作った後に呼ぶ。初回実行ではこの位置でしか移動できない。
	if err := a.ensureDailySummaryFirst(ctx); err != nil {
		return err
	}

	if err := a.updateState(ctx, zip, lastSuccess); err != nil {
		return err
	}

	// 取り込みまで到達した周でだけ消化する。ZIPが取れなかった周で消化すると、
	// 起動直後にDriveが落ちていたときに強制取り込みの機会を失う。
	a.startupIngestDone = true

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
	rows, err := report.BuildDailySummary(ctx, a.st, a.kinds)
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
	for _, k := range a.kinds {
		rows, err := report.BuildRawTab(ctx, a.st, k, a.now())
		if err != nil {
			return fmt.Errorf("app: build raw tab for %q: %w", k.Key(), err)
		}
		title := report.RawTabTitle(k.Key())
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

func (a *App) writeMeta(ctx context.Context, lastSuccess, zipModified time.Time, tableRows map[string]int64) error {
	rows, err := report.BuildMeta(ctx, a.st, a.kinds, lastSuccess, zipModified, tableRows)
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
		// Drive の modifiedTime はミリ秒を持つ。RFC3339（秒精度）で保存すると
		// 復元値が常に実際より古くなり、同じZIPが毎周回「新着」と判定される。
		{stateKeyLastProcessedModifiedTime, zip.ModifiedTime.Format(time.RFC3339Nano)},
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
