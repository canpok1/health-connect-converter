// Package store は Health Connect のエクスポートデータを累積保存する SQLite ストアを提供する。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/agg"
	"health-connect-converter/internal/kind"
	"health-connect-converter/internal/model"
)

// stateKeyAppPriorities は Health Connect の「アプリの優先度」を保存する state のキー。
const stateKeyAppPriorities = "app_priorities"

// Store は累積 SQLite への読み書きを担う。
type Store struct {
	db *sql.DB
}

// Open は path の SQLite ファイルを開く。親ディレクトリが無ければ作る。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// sqlite はマルチライタに弱いため、コネクションを1本に固定して直列化する。
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close はストアを閉じる。
func (s *Store) Close() error {
	return s.db.Close()
}

// Migrate は state テーブルと、種別ごとの保存先を作成・追加更新する。
func (s *Store) Migrate(ctx context.Context, kinds []kind.Kind) error {
	const stateDDL = `CREATE TABLE IF NOT EXISTS state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
)`
	if _, err := s.db.ExecContext(ctx, stateDDL); err != nil {
		return fmt.Errorf("store: migrate state table: %w", err)
	}

	for _, k := range kinds {
		if err := k.Table().Migrate(ctx, s.db); err != nil {
			return fmt.Errorf("store: migrate %s: %w", k.Key(), err)
		}
	}
	return nil
}

// Ingest は種別 k をエクスポートDBから読み、累積DBへ保存する。保存した件数を返す。
//
// エクスポートに含まれる現地日は、その日ぶんを丸ごと置き換える。UUID をキーに
// 追加するだけだと、端末側で作り直されたレコード（Health Connect は細かい
// レコードを後から日次合計へ書き換える）の古い版が残り続け、合計が膨らむ。
// エクスポートに現れない日は端末から消えていても残す（累積DBの目的。ADR 0008）。
func (s *Store) Ingest(ctx context.Context, k kind.Kind, exportDB *sql.DB) (int, error) {
	rows, dates, err := k.ExportRows(ctx, exportDB)
	if err != nil {
		return 0, fmt.Errorf("store: read export for %s: %w", k.Key(), err)
	}
	n, err := k.Table().Replace(ctx, s.db, dates, rows)
	if err != nil {
		return 0, fmt.Errorf("store: save %s: %w", k.Key(), err)
	}
	return n, nil
}

// DailyAggregates は種別 k のレコードを集計用モデルへ直し、日次集計を返す。
// 種別が重複排除を求める場合は、保存済みのアプリ優先度を渡す。
func (s *Store) DailyAggregates(ctx context.Context, k kind.Kind) ([]model.DailyRow, error) {
	recs, err := k.Aggregate(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("store: aggregate records for %s: %w", k.Key(), err)
	}

	policy := k.Policy()
	var order []string
	if policy.Dedupe {
		prios, err := s.AppPriorities(ctx)
		if err != nil {
			return nil, err
		}
		order = prios[policy.Category]
	}
	return agg.Daily(recs, agg.Options{Daily: policy.Daily, Dedupe: policy.Dedupe}, order), nil
}

// RawRows は種別 k の生データタブの行（ヘッダを除く）を返す。
func (s *Store) RawRows(ctx context.Context, k kind.Kind, sinceMs int64) ([][]any, error) {
	rows, err := k.RawRows(ctx, s.db, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("store: raw rows for %s: %w", k.Key(), err)
	}
	return rows, nil
}

// Stats は種別 k の件数と最新レコードの時刻を返す。
func (s *Store) Stats(ctx context.Context, k kind.Kind) (model.TypeStats, error) {
	stats, err := k.Table().Stats(ctx, s.db)
	if err != nil {
		return model.TypeStats{}, fmt.Errorf("store: stats for %s: %w", k.Key(), err)
	}
	return stats, nil
}

// SetAppPriorities は Health Connect のアプリ優先度を state へ保存する。
// 空のときは既存を消さない（優先度を持たないエクスポートを読んだだけで
// 重複排除が効かなくなるのを防ぐ）。
func (s *Store) SetAppPriorities(ctx context.Context, prios model.AppPriorities) error {
	if len(prios) == 0 {
		return nil
	}
	m := make(map[string][]string, len(prios))
	for category, apps := range prios {
		m[strconv.Itoa(category)] = apps
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode app priorities: %w", err)
	}
	return s.SetState(ctx, stateKeyAppPriorities, string(encoded))
}

// AppPriorities は保存済みのアプリ優先度を返す。未保存なら空。
func (s *Store) AppPriorities(ctx context.Context) (model.AppPriorities, error) {
	raw, err := s.GetState(ctx, stateKeyAppPriorities)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return model.AppPriorities{}, nil
	}
	var m map[string][]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("store: decode app priorities: %w", err)
	}
	prios := make(model.AppPriorities, len(m))
	for key, apps := range m {
		category, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("store: decode app priorities: invalid category %q: %w", key, err)
		}
		prios[category] = apps
	}
	return prios, nil
}

// GetState は state テーブルから key の値を返す。未設定なら ("", nil)。
func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get state %s: %w", key, err)
	}
	return value, nil
}

// SetState は state テーブルへ key/value を保存する（既存なら上書き）。
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("store: set state %s: %w", key, err)
	}
	return nil
}
