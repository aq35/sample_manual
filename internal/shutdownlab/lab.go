// Package shutdownlab は EXP-3（graceful shutdown）の実験本体。
//
// 確かめること:
//   - 受付の停止が、処理の停止より先に行われる
//   - 未処理のものが「捨てられた」のか「戻された」のかが分かる
//   - commit 済みと未 commit が区別される（ack 済み消失を出さない）
//   - goroutine・ticker・接続が残らない
//   - 期限を超えた場合に、何が残っているかが記録される
package shutdownlab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	return db, nil
}

func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		var lines []string
		for _, ln := range strings.Split(stmt, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		s := strings.TrimSpace(strings.Join(lines, "\n"))
		if s == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func Reset(ctx context.Context, db *sql.DB, tenant string) error {
	for _, t := range []string{"shutdown_item", "shutdown_lease"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	return nil
}

// CommittedIDs は DB に入っている item の ID。
func CommittedIDs(ctx context.Context, db *sql.DB, tenant string) ([]int, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT item_id FROM shutdown_item WHERE tenant_id = ? ORDER BY item_id", tenant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
