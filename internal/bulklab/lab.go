// Package bulklab は EXP-56（bulk INSERT の正規化）の実験本体。
//
// 同じ N 行を入れるのに、やり方で往復数とコミット数が桁で変わる。autocommit の単発 × N は
// 「N 往復 × N コミット（fsync）」で最悪。1トランザクションに包むとコミットが1回に。multi-row
// INSERT（chunk 件を1文）は往復が N/chunk に。件/秒で正規化して比べる。
package bulklab

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Setup は空の宛先表を作る。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS bulk_dst",
		`CREATE TABLE bulk_dst (
		   id BIGINT NOT NULL AUTO_INCREMENT,
		   tenant VARCHAR(32) NOT NULL, payload VARCHAR(100) NOT NULL,
		   PRIMARY KEY (id)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func truncate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "TRUNCATE TABLE bulk_dst")
	return err
}

const pad = "payload-payload-payload-payload"

// SingleAutocommit は autocommit のまま単発 INSERT を N 回（N 往復・N コミット）。
func SingleAutocommit(ctx context.Context, db *sql.DB, tenant string, n int) (time.Duration, error) {
	if err := truncate(ctx, db); err != nil {
		return 0, err
	}
	t0 := time.Now()
	for i := 0; i < n; i++ {
		//smlint:allow loopquery 理由: これが測定対象（素朴な単発 INSERT ループ）
		//smlint:allow rowsaffected 理由: 実験の INSERT。1行に当たる
		if _, err := db.ExecContext(ctx, "INSERT INTO bulk_dst (tenant, payload) VALUES (?,?)", tenant, pad); err != nil {
			return 0, err
		}
	}
	return time.Since(t0), nil
}

// SingleInTx は1トランザクションに包んで単発 INSERT を N 回（N 往復・1 コミット）。
func SingleInTx(ctx context.Context, db *sql.DB, tenant string, n int) (time.Duration, error) {
	if err := truncate(ctx, db); err != nil {
		return 0, err
	}
	t0 := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	for i := 0; i < n; i++ {
		//smlint:allow loopquery 理由: 測定対象（tx 内の単発 INSERT ループ）
		//smlint:allow rowsaffected 理由: 実験の INSERT。1行に当たる
		if _, err := tx.ExecContext(ctx, "INSERT INTO bulk_dst (tenant, payload) VALUES (?,?)", tenant, pad); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return time.Since(t0), nil
}

// PreparedInTx は1 tx＋prepared statement 再利用で単発 INSERT を N 回（パース1回）。
func PreparedInTx(ctx context.Context, db *sql.DB, tenant string, n int) (time.Duration, error) {
	if err := truncate(ctx, db); err != nil {
		return 0, err
	}
	t0 := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO bulk_dst (tenant, payload) VALUES (?,?)")
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	for i := 0; i < n; i++ {
		//smlint:allow loopquery 理由: 測定対象（prepared 再利用の単発 INSERT ループ）
		//smlint:allow rowsaffected 理由: 実験の INSERT。1行に当たる
		if _, err := stmt.ExecContext(ctx, tenant, pad); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return 0, err
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return time.Since(t0), nil
}

// MultiRow は chunk 件を1文にまとめた multi-row INSERT（往復 N/chunk）。
func MultiRow(ctx context.Context, db *sql.DB, tenant string, n, chunk int) (time.Duration, error) {
	if err := truncate(ctx, db); err != nil {
		return 0, err
	}
	t0 := time.Now()
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		var b strings.Builder
		b.WriteString("INSERT INTO bulk_dst (tenant, payload) VALUES ")
		var args []any
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			b.WriteString("(?,?)")
			args = append(args, tenant, pad)
		}
		//smlint:allow loopquery 理由: 測定対象（chunk 件ずつの multi-row INSERT）
		//smlint:allow rowsaffected 理由: 実験の INSERT。chunk 行に当たる
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			return 0, err
		}
	}
	return time.Since(t0), nil
}

// Count は宛先の行数（正しく N 入ったかの確認）。
func Count(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM bulk_dst").Scan(&n)
	return n, err
}
