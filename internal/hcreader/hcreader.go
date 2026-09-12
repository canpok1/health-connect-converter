// Package hcreader は Health Connect のエクスポートZIPを展開し、その中の SQLite を
// 開いて「種別に依らない情報」を読む。
//
// 種別ごとのデータは種別自身が読む（ADR 0012）。ここが扱うのは ZIP の展開、DBの
// 開き方、アプリ優先度、全テーブルの行数。
package hcreader

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"health-connect-converter/internal/model"
)

// ExtractDB は zipData の中から拡張子 ".db" のエントリ（複数あれば最大サイズのもの）を
// destPath へ書き出す。
func ExtractDB(zipData []byte, destPath string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("hcreader: open zip: %w", err)
	}

	var target *zip.File
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".db") {
			continue
		}
		if target == nil || f.UncompressedSize64 > target.UncompressedSize64 {
			target = f
		}
	}
	if target == nil {
		return fmt.Errorf("hcreader: no .db entry found in zip")
	}

	rc, err := target.Open()
	if err != nil {
		return fmt.Errorf("hcreader: open zip entry %s: %w", target.Name, err)
	}
	defer func() { _ = rc.Close() }()

	if dir := filepath.Dir(destPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("hcreader: mkdir %s: %w", dir, err)
		}
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("hcreader: create %s: %w", destPath, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("hcreader: write %s: %w", destPath, err)
	}
	return nil
}

// Open はエクスポートDBを読み取り専用で開く。
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		return nil, fmt.Errorf("hcreader: open %s: %w", path, err)
	}
	return db, nil
}

// readTableRows はエクスポートDB内の全テーブルの行数を数える。config に登録済みの
// 種別へ絞らないのは、登録していないテーブルへ書き込みが始まったことに気づくのが
// 目的だから。名前の形（*_record_table など）でも絞らない。エクスポートDBは
// CamelCase のテーブルも持つため、形で絞ると取りこぼす。
func ReadTableRows(ctx context.Context, db *sql.DB, logger *slog.Logger) (map[string]int64, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// LIKE の `_` は任意1文字に当たるため ESCAPE で literal にする。エスケープ
	// しないと sqlite + 任意1文字で始まる実在のテーブルまで除外され、未登録の
	// テーブルに気づくというこの関数の目的を損なう。
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		 ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("hcreader: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("hcreader: scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hcreader: list tables: %w", err)
	}

	counts := make(map[string]int64, len(names))
	for _, name := range names {
		var n int64
		// テーブル名はプレースホルダに置けないため連結する。sqlite_master から
		// 得た実在の名前だが、クォートと `"` のエスケープで識別子として閉じる。
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoteIdentifier(name))
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			// 1テーブルの破損で取り込み全体を止めない。
			logger.Warn("テーブルの件数取得に失敗", "table", name, "error", err)
			continue
		}
		counts[name] = n
	}
	return counts, nil
}

// quoteIdentifier は SQLite の識別子を二重引用符で囲む。名前に含まれる `"` は
// `""` へエスケープする。
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

const priorityTable = "health_data_category_priority_table"

// readAppPriorities は Health Connect の「アプリの優先度」設定を読む。
// app_id_priority_order は application_info_table.row_id をカンマで並べた
// 文字列で、先頭が最優先。テーブルが無いエクスポートもありうるため、
// 存在しなければ空を返す（優先度が無くても取り込みは続けられる）。
func ReadAppPriorities(ctx context.Context, db *sql.DB) (model.AppPriorities, error) {
	var exists int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", priorityTable).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("hcreader: look up %s: %w", priorityTable, err)
	}
	if exists == 0 {
		return model.AppPriorities{}, nil
	}

	apps, err := readAppNames(ctx, db)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, "SELECT health_data_category, app_id_priority_order FROM "+priorityTable)
	if err != nil {
		return nil, fmt.Errorf("hcreader: query %s: %w", priorityTable, err)
	}
	defer func() { _ = rows.Close() }()

	prios := make(model.AppPriorities)
	for rows.Next() {
		var category int
		var order sql.NullString
		if err := rows.Scan(&category, &order); err != nil {
			return nil, fmt.Errorf("hcreader: scan %s: %w", priorityTable, err)
		}
		var names []string
		for _, field := range strings.Split(order.String, ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			id, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("hcreader: %s: invalid app_id_priority_order %q: %w", priorityTable, order.String, err)
			}
			// 対応するアプリが application_info_table から消えていることがある。
			// 順序だけ残っても使えないため飛ばす。
			if name, ok := apps[id]; ok {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			prios[category] = names
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hcreader: query %s: %w", priorityTable, err)
	}
	return prios, nil
}

func readAppNames(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT row_id, package_name FROM application_info_table")
	if err != nil {
		return nil, fmt.Errorf("hcreader: query application_info_table: %w", err)
	}
	defer func() { _ = rows.Close() }()

	apps := make(map[int64]string)
	for rows.Next() {
		var id int64
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("hcreader: scan application_info_table: %w", err)
		}
		if name.Valid && name.String != "" {
			apps[id] = name.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hcreader: query application_info_table: %w", err)
	}
	return apps, nil
}
