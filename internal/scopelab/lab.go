// Package scopelab は EXP-58（共有ワーカーのテナントスコープ強制）の実験本体。
//
// 共有ワーカー（1プロセスが全テナントを回す）は、クエリにテナント境界を書き忘れると
// 他テナントの行を読み・書きしてしまう（越境＝情報漏洩・データ破壊）。テナントを WHERE で
// 強制した場合と、書き忘れた場合で、他テナント行への「読み漏れ」「書き込み」を数える。
package scopelab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Setup は共有テーブル scope_item を作る（複数テナントの行が同居する）。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS scope_item",
		`CREATE TABLE scope_item (
		   id        BIGINT      NOT NULL AUTO_INCREMENT,
		   tenant_id VARCHAR(32) NOT NULL,
		   status    VARCHAR(16) NOT NULL,
		   PRIMARY KEY (id),
		   KEY k_ts (tenant_id, status),
		   KEY k_s (status)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("scope setup: %w", err)
		}
	}
	return nil
}

// Seed は各テナントに nEach 件の pending 行を入れる（他テナントの行も同じ表に同居）。
func Seed(ctx context.Context, db *sql.DB, tenants []string, nEach int) error {
	if _, err := db.ExecContext(ctx, "TRUNCATE TABLE scope_item"); err != nil {
		return err
	}
	const chunk = 500
	for _, t := range tenants {
		for start := 0; start < nEach; start += chunk {
			end := start + chunk
			if end > nEach {
				end = nEach
			}
			var b strings.Builder
			b.WriteString("INSERT INTO scope_item (tenant_id, status) VALUES ")
			var args []any
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?, 'pending')")
				args = append(args, t)
			}
			//smlint:allow loopquery 理由: 実験 seed。500 件ずつまとめた bulk INSERT
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProcessRead は「processing テナントの pending を読む」処理。scoped=false だとテナント境界を
// 書き忘れた状態（status だけで絞る）。返り値は自テナント行数・他テナント行数（＝読み漏れ＝越境）。
func ProcessRead(ctx context.Context, db *sql.DB, processing string, scoped bool) (mine, others int, err error) {
	q := "SELECT tenant_id FROM scope_item WHERE status='pending'"
	var args []any
	if scoped {
		q += " AND tenant_id=?" // ★テナント境界を強制
		args = append(args, processing)
	}
	q += " LIMIT 100000"
	//smlint:allow loopquery 理由: これ自体が1回のクエリ。行はループで読むだけ
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return 0, 0, err
		}
		if t == processing {
			mine++
		} else {
			others++ // 他テナントの行が見えた＝越境
		}
	}
	return mine, others, rows.Err()
}

// ProcessWrite は「processing テナントの pending を done にする」処理。scoped=false だと
// テナント境界を書き忘れ、他テナントの行まで done にしてしまう（データ破壊＋越境）。
// 返り値は自テナントで done にした数・他テナントで done にしてしまった数。
func ProcessWrite(ctx context.Context, db *sql.DB, processing string, scoped bool) (mineDone, othersDone int, err error) {
	q := "UPDATE scope_item SET status='done' WHERE status='pending'"
	var args []any
	if scoped {
		q += " AND tenant_id=?" // ★テナント境界を強制
		args = append(args, processing)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	if _, err := res.RowsAffected(); err != nil { // 影響行数を確かめる（捨てない）
		return 0, 0, err
	}
	// done になった行を owner 別に数える
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scope_item WHERE status='done' AND tenant_id=?", processing).Scan(&mineDone); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scope_item WHERE status='done' AND tenant_id<>?", processing).Scan(&othersDone); err != nil {
		return 0, 0, err
	}
	return mineDone, othersDone, nil
}
