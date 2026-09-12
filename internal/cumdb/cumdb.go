// Package cumdb は累積SQLiteのテーブル操作の定型処理を提供する。
//
// 列の構成は種別ごとのコードが宣言する（ADR 0012）。ここは宣言された列に対して
// 作成・追加更新・日ごと置き換え・期間クエリ・統計を行うだけで、どの種別が
// どんな列を持つかは知らない。
package cumdb

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"health-connect-converter/internal/model"
)

var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// deleteDatesChunk は SQLite のプレースホルダ上限に収めるための1回ぶんの件数。
const deleteDatesChunk = 500

// LocalDateExpr は時刻列（UTC epoch ms）とオフセット列（秒）から現地日を返す
// SQL 式を作る。集計側（agg）の現地日の求め方と同じ規則にすること。
func LocalDateExpr(timeCol, offsetCol string) string {
	for _, c := range []string{timeCol, offsetCol} {
		if !identifierRe.MatchString(c) {
			// 呼び出し側はコード内の定数を渡す。ここに来るのは実装の誤り。
			panic(fmt.Sprintf("cumdb: invalid column %q", c))
		}
	}
	return fmt.Sprintf("strftime('%%Y-%%m-%%d', (%s / 1000 + %s), 'unixepoch')", timeCol, offsetCol)
}

// Column は1列の宣言。Type は SQLite の型名（TEXT / INTEGER / REAL）。
type Column struct {
	Name string
	Type string
}

// Table は種別1つぶんの保存先。
type Table struct {
	// Name はテーブル名。既存データを引き継ぐため現行の命名（record_<種別キー>）を保つ。
	Name string
	// Columns は全列。先頭は必ず一意キーの列で、PRIMARY KEY になる。
	Columns []Column
	// TimeColumn は期間の絞り込みと最新レコードの判定に使う列。
	TimeColumn string
	// DateExpr は日ごと置き換えの基準にする「現地日を返すSQL式」。
	// LocalDateExpr で組み立てる（コード内の宣言から作ること。外部入力を渡さない）。
	DateExpr string
}

// ColumnNames は列名を宣言順で返す。
func (t Table) ColumnNames() []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}

func (t Table) validate() error {
	if !identifierRe.MatchString(t.Name) {
		return fmt.Errorf("cumdb: invalid table name %q", t.Name)
	}
	if len(t.Columns) == 0 {
		return fmt.Errorf("cumdb: table %q has no columns", t.Name)
	}
	if !identifierRe.MatchString(t.TimeColumn) {
		return fmt.Errorf("cumdb: table %q: invalid time column %q", t.Name, t.TimeColumn)
	}
	if t.DateExpr == "" {
		return fmt.Errorf("cumdb: table %q: DateExpr is required", t.Name)
	}
	for _, c := range t.Columns {
		if !identifierRe.MatchString(c.Name) {
			return fmt.Errorf("cumdb: table %q: invalid column %q", t.Name, c.Name)
		}
		switch c.Type {
		case "TEXT", "INTEGER", "REAL":
		default:
			return fmt.Errorf("cumdb: table %q: column %q has unsupported type %q", t.Name, c.Name, c.Type)
		}
	}
	return nil
}

// Migrate はテーブルを作り、宣言に増えた列を追加する。列の削除・型変更はしない。
func (t Table) Migrate(ctx context.Context, db *sql.DB) error {
	if err := t.validate(); err != nil {
		return err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE IF NOT EXISTS %s (\n", t.Name)
	for i, c := range t.Columns {
		if i > 0 {
			sb.WriteString(",\n")
		}
		fmt.Fprintf(&sb, "\t%s %s", c.Name, c.Type)
		if i == 0 {
			sb.WriteString(" PRIMARY KEY")
		}
	}
	sb.WriteString("\n)")
	if _, err := db.ExecContext(ctx, sb.String()); err != nil {
		return fmt.Errorf("cumdb: create table %s: %w", t.Name, err)
	}

	existing, err := t.existingColumns(ctx, db)
	if err != nil {
		return err
	}
	for _, c := range t.Columns {
		if existing[c.Name] {
			continue
		}
		alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", t.Name, c.Name, c.Type)
		if _, err := db.ExecContext(ctx, alter); err != nil {
			return fmt.Errorf("cumdb: add column %s to %s: %w", c.Name, t.Name, err)
		}
	}

	// 索引は列を追加した後に作る。既存のテーブルに無い列を TimeColumn として
	// 宣言した場合、先に索引を作ると「まだ無い列」を参照して移行が止まる。
	idx := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s(%s)", t.indexName(), t.Name, t.TimeColumn)
	if _, err := db.ExecContext(ctx, idx); err != nil {
		return fmt.Errorf("cumdb: create index on %s: %w", t.Name, err)
	}

	// 設定ファイル駆動だった頃は idx_<テーブル>_start という名前で start_time に
	// 索引を張っていた。同じ列に2本あっても速くならず書き込みが遅くなるだけなので、
	// 既存のデータベースから落とす。TimeColumn が start_time のときだけ行う
	// （別の列を見る種別では、旧索引が現役の可能性がある）。
	if t.TimeColumn == "start_time" {
		legacy := fmt.Sprintf("DROP INDEX IF EXISTS idx_%s_start", t.Name)
		if _, err := db.ExecContext(ctx, legacy); err != nil {
			return fmt.Errorf("cumdb: drop legacy index on %s: %w", t.Name, err)
		}
	}
	return nil
}

// indexName は TimeColumn 用の索引名。列名から作るため、時刻列を変えた種別で
// 名前が衝突しない。
func (t Table) indexName() string {
	return fmt.Sprintf("idx_%s_%s", t.Name, t.TimeColumn)
}

func (t Table) existingColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", t.Name))
	if err != nil {
		return nil, fmt.Errorf("cumdb: table_info %s: %w", t.Name, err)
	}
	defer func() { _ = rows.Close() }()

	cols := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return nil, fmt.Errorf("cumdb: scan table_info %s: %w", t.Name, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cumdb: table_info %s: %w", t.Name, err)
	}
	return cols, nil
}

// Replace は rows を1トランザクションで書き込む。dates に挙げた現地日の既存行は
// 先に削除する（エクスポートに含まれる日は、その日ぶんを丸ごと置き換える。
// 上流で消えたレコードを残さないため。ADR 0008）。
// rows の各行は Columns と同じ順で値を並べる。
func (t Table) Replace(ctx context.Context, db *sql.DB, dates []string, rows [][]any) (int, error) {
	if err := t.validate(); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	names := t.ColumnNames()
	placeholders := make([]string, len(names))
	setParts := make([]string, 0, len(names)-1)
	for i, n := range names {
		placeholders[i] = "?"
		if i > 0 {
			setParts = append(setParts, fmt.Sprintf("%s = excluded.%s", n, n))
		}
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
		t.Name, strings.Join(names, ", "), strings.Join(placeholders, ", "), names[0], strings.Join(setParts, ", "))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("cumdb: begin tx for %s: %w", t.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := t.deleteDates(ctx, tx, dates); err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("cumdb: prepare insert for %s: %w", t.Name, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, row := range rows {
		if len(row) != len(names) {
			return 0, fmt.Errorf("cumdb: table %s: got %d values, want %d", t.Name, len(row), len(names))
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return 0, fmt.Errorf("cumdb: insert into %s: %w", t.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("cumdb: commit %s: %w", t.Name, err)
	}
	return len(rows), nil
}

func (t Table) deleteDates(ctx context.Context, tx *sql.Tx, dates []string) error {
	for start := 0; start < len(dates); start += deleteDatesChunk {
		end := min(start+deleteDatesChunk, len(dates))
		chunk := dates[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, d := range chunk {
			placeholders[i] = "?"
			args[i] = d
		}
		query := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", t.Name, t.DateExpr, strings.Join(placeholders, ", "))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("cumdb: delete %s for %d dates: %w", t.Name, len(chunk), err)
		}
	}
	return nil
}

// Query は TimeColumn が sinceMs 以降の行を時刻の昇順で返す。走査は呼び出し側が行う。
func (t Table) Query(ctx context.Context, db *sql.DB, sinceMs int64) (*sql.Rows, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s >= ? ORDER BY %s ASC",
		strings.Join(t.ColumnNames(), ", "), t.Name, t.TimeColumn, t.TimeColumn)
	rows, err := db.QueryContext(ctx, query, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("cumdb: query %s: %w", t.Name, err)
	}
	return rows, nil
}

// Stats は件数と最新レコードの時刻を返す。
//
// テーブルが無い場合はエラーになる。取り込み前に Migrate が全種別ぶんのテーブルを
// 作るため、無いのは実装か運用の誤りであり、黙ってゼロ件として出すより気付ける
// ほうがよい。
func (t Table) Stats(ctx context.Context, db *sql.DB) (model.TypeStats, error) {
	if err := t.validate(); err != nil {
		return model.TypeStats{}, err
	}
	query := fmt.Sprintf("SELECT COUNT(*), COALESCE(MAX(%s), 0) FROM %s", t.TimeColumn, t.Name)
	var stats model.TypeStats
	if err := db.QueryRowContext(ctx, query).Scan(&stats.Count, &stats.LatestStartTime); err != nil {
		return model.TypeStats{}, fmt.Errorf("cumdb: stats %s: %w", t.Name, err)
	}
	return stats, nil
}
