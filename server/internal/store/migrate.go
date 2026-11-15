package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrationFiles は migrations/ の SQL をバイナリに埋め込む。
// コンテナに SQL ファイルを別途コピーする必要がなく、「この版のバイナリが期待するスキーマ」が
// 必ず一緒に配られる。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey は pg_advisory_lock に渡す任意の番号（このアプリのマイグレーション専用）。
const migrationLockKey = 3_420_260_116

// Migrate は未適用のマイグレーションをファイル名の順に適用する。
//
// 仕組み（外部ツール無しの最小実装）:
//   - schema_migrations テーブルに「適用済みのファイル名」を記録する。
//   - 起動のたびに呼ぶ。記録に無いファイルだけを、1ファイル＝1トランザクションで実行する。
//     SQL の途中で失敗したらそのファイルの変更は全部取り消され、記録も付かない。
//
// 運用の決まり: 一度コミットした SQL ファイルは書き換えない。変更は新しい番号のファイルで足す
// （適用済みの環境では古いファイルは二度と実行されないため）。
//
// 副作用: スキーマを変更する。
// 失敗条件: SQL の誤り、権限不足、接続断。
func Migrate(ctx context.Context, db *sql.DB) (applied []string, err error) {
	// アドバイザリロックは「接続」に紐づくので、プールから1本を借りて最後まで使う。
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 2つのプロセスが同時に起動してもマイグレーションが二重に走らないようにする
	// （ローリング更新の一瞬など。MVP は1レプリカだが、安全側に倒しておく）。
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("マイグレーションのロックを取れません: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("schema_migrations を作れません: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		var done bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return applied, err
		}
		if done {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return applied, err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return applied, err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("マイグレーション %s に失敗: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return applied, err
		}
		if err := tx.Commit(); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// migrationNames は埋め込まれた SQL ファイル名を昇順で返す（0001_..., 0002_... の順）。
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
