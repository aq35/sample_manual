// Package retentionlab は EXP-48（保持期間の運用）の実験本体。
//
// 履歴は増え続ける。保持期間を決め、古い日付を「丸ごと捨てる」。日ごとの RANGE パーティションに
// しておけば、DROP PARTITION で一瞬（DELETE は undo 肥大で高い・EXP-15）。current は書けるまま、
// パーティション数は有界に保つ（ロールフォワード）。
package retentionlab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 日付パーティションつき履歴表（partitioned）と、比較用のフラット表（非 partitioned）。
func Setup(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		"DROP TABLE IF EXISTS ret_hist",
		`CREATE TABLE ret_hist (
		   tenant_id VARCHAR(32) NOT NULL,
		   d         DATE        NOT NULL,
		   id        BIGINT      NOT NULL,
		   PRIMARY KEY (tenant_id, d, id)
		 ) ENGINE=InnoDB
		 PARTITION BY RANGE COLUMNS(d) (
		   PARTITION p20260901 VALUES LESS THAN ('2026-09-02'),
		   PARTITION p20260902 VALUES LESS THAN ('2026-09-03'),
		   PARTITION p20260903 VALUES LESS THAN ('2026-09-04'),
		   PARTITION p20260904 VALUES LESS THAN ('2026-09-05'),
		   PARTITION p20260905 VALUES LESS THAN ('2026-09-06'),
		   PARTITION pmax      VALUES LESS THAN (MAXVALUE)
		 )`,
		"DROP TABLE IF EXISTS ret_flat",
		`CREATE TABLE ret_flat (
		   tenant_id VARCHAR(32) NOT NULL,
		   d         DATE        NOT NULL,
		   id        BIGINT      NOT NULL,
		   PRIMARY KEY (tenant_id, d, id),
		   KEY byd (d)
		 ) ENGINE=InnoDB`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("retention setup: %w", err)
		}
	}
	return nil
}

var days = []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04", "2026-09-05"}

// Seed は各日に perDay 行を、partitioned とフラットの両方へ入れる。
func Seed(ctx context.Context, db *sql.DB, tenant string, perDay int) error {
	for _, tbl := range []string{"ret_hist", "ret_flat"} {
		for _, d := range days {
			const chunk = 500
			for start := 0; start < perDay; start += chunk {
				end := start + chunk
				if end > perDay {
					end = perDay
				}
				var b strings.Builder
				b.WriteString("INSERT INTO " + tbl + " (tenant_id, d, id) VALUES ")
				var args []any
				for i := start; i < end; i++ {
					if i > start {
						b.WriteString(",")
					}
					b.WriteString("(?,?,?)")
					args = append(args, tenant, d, i)
				}
				//smlint:allow loopquery 理由: 実験用 seed。500 件ずつまとめた bulk INSERT をチャンクで回している
				if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// DropOldPartitions は cutoff より前の日パーティションを DROP する（保持期間の適用・partitioned）。
func DropOldPartitions(ctx context.Context, db *sql.DB, parts ...string) (time.Duration, error) {
	t0 := time.Now()
	for _, p := range parts {
		if _, err := db.ExecContext(ctx, "ALTER TABLE ret_hist DROP PARTITION "+p); err != nil {
			return 0, err
		}
	}
	return time.Since(t0), nil
}

// DeleteOldFlat は同じ古い範囲を DELETE で消す（比較・非 partitioned）。
func DeleteOldFlat(ctx context.Context, db *sql.DB, before string) (time.Duration, error) {
	t0 := time.Now()
	//smlint:allow rowsaffected 理由: DROP との速度比較用の DELETE。消える行数は前提でなく計測対象
	if _, err := db.ExecContext(ctx, "DELETE FROM ret_flat WHERE d < ?", before); err != nil {
		return 0, err
	}
	return time.Since(t0), nil
}

// RollForward は「次の日のパーティションを用意する」（pmax を割って新パーティション＋pmax に）。
func RollForward(ctx context.Context, db *sql.DB, newPart, lessThan string) error {
	q := fmt.Sprintf(
		"ALTER TABLE ret_hist REORGANIZE PARTITION pmax INTO (PARTITION %s VALUES LESS THAN ('%s'), PARTITION pmax VALUES LESS THAN (MAXVALUE))",
		newPart, lessThan)
	_, err := db.ExecContext(ctx, q)
	return err
}

// CountDay は partitioned 表の d の行数。
func CountDay(ctx context.Context, db *sql.DB, tenant, d string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ret_hist WHERE tenant_id=? AND d=?", tenant, d).Scan(&n)
	return n, err
}

// PartitionCount は ret_hist のパーティション数（有界かの確認）。
func PartitionCount(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.partitions WHERE table_schema=DATABASE() AND table_name='ret_hist' AND partition_name IS NOT NULL").Scan(&n)
	return n, err
}

// InsertOne は指定日に1行入れる（current が書けることの確認）。
func InsertOne(ctx context.Context, db *sql.DB, tenant, d string, id int64) error {
	_, err := db.ExecContext(ctx, "INSERT INTO ret_hist (tenant_id, d, id) VALUES (?,?,?)", tenant, d, id)
	return err
}
