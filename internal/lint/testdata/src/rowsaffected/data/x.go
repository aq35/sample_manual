package data

import (
	"context"
	"database/sql"
)

func Run(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "UPDATE t SET a = 1 WHERE tenant_id = 'x' AND id = 1") // want "影響行数を捨てている"
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, "DELETE FROM t WHERE tenant_id = 'x' AND id = 1")
	if err != nil {
		return err
	}
	_, err = res.RowsAffected()
	return err
}
