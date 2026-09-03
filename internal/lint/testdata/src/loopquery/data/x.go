package data

import (
	"context"
	"database/sql"
)

func Run(ctx context.Context, db *sql.DB, ids []int) error {
	for _, id := range ids {
		var v int
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = ?", id).Scan(&v); err != nil { // want "ループの中で1件ずつ"
			return err
		}
	}
	return nil
}
