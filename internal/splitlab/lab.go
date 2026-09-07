// Package splitlab は EXP-19（メモ列の縦分割）の実験本体。
//
// 問い: 失敗理由のような「めったに要らない大きなメモ列」を、一覧の行に同居させるか、
// 別テーブルに分けるか。一覧クエリ（メモを取らない）と、詳細取得（メモを取る）で測る。
package splitlab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

//go:embed schema.sql
var schemaSQL string

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定 DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("split schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// Seed は tenant に rows 件を、同居版・分割版の両方へ入れる（同じ id・同じ status・同じ detail）。
func Seed(ctx context.Context, db *sql.DB, tenant string, rows int) error {
	for _, t := range []string{"split_inline", "split_list", "split_detail"} {
		//smlint:allow loopquery 理由: 実験前の後片付け
		//smlint:allow rowsaffected 理由: 後片付け
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	detail := strings.Repeat("理由", 500) // 約 3KB 相当のメモ
	insertBatch := func(table string, withDetail bool) error {
		const chunk = 500
		for start := 0; start < rows; start += chunk {
			end := start + chunk
			if end > rows {
				end = rows
			}
			var b strings.Builder
			if withDetail {
				b.WriteString("INSERT INTO " + table + " (tenant_id, id, status, detail) VALUES ")
			} else if table == "split_detail" {
				b.WriteString("INSERT INTO split_detail (tenant_id, id, detail) VALUES ")
			} else {
				b.WriteString("INSERT INTO " + table + " (tenant_id, id, status) VALUES ")
			}
			var args []any
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				switch {
				case withDetail:
					b.WriteString("(?,?,?,?)")
					args = append(args, tenant, i, i%5, detail)
				case table == "split_detail":
					b.WriteString("(?,?,?)")
					args = append(args, tenant, i, detail)
				default:
					b.WriteString("(?,?,?)")
					args = append(args, tenant, i, i%5)
				}
			}
			//smlint:allow loopquery 理由: 一括投入（500行/文）
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := insertBatch("split_inline", true); err != nil {
		return err
	}
	if err := insertBatch("split_list", false); err != nil {
		return err
	}
	if err := insertBatch("split_detail", false); err != nil {
		return err
	}
	return nil
}

// ListInline は同居版の一覧（メモは取らないが、行にメモが同居している）。
func ListInline(ctx context.Context, db *sql.DB, tenant string, limit, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id, status FROM split_inline WHERE tenant_id=? AND status=? ORDER BY id LIMIT ?",
			tenant, 1, limit)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}

// ListSplit は分割版の一覧（狭い表だけ）。
func ListSplit(ctx context.Context, db *sql.DB, tenant string, limit, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id, status FROM split_list WHERE tenant_id=? AND status=? ORDER BY id LIMIT ?",
			tenant, 1, limit)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}

// PageThenDetailSplit は「一覧で1ページ取り、その id 群のメモをまとめて別表から引く」。
// GraphQL の DataLoader 相当（要求されたときだけ、バッチで）。
func PageThenDetailSplit(ctx context.Context, db *sql.DB, tenant string, limit, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM split_list WHERE tenant_id=? AND status=? ORDER BY id LIMIT ?", tenant, 1, limit)
		if err != nil {
			return err
		}
		var ids []any
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := append([]any{tenant}, ids...)
		dr, err := db.QueryContext(ctx,
			"SELECT id, detail FROM split_detail WHERE tenant_id=? AND id IN ("+ph+")", args...)
		if err != nil {
			return err
		}
		return drain(dr)
	})
}

// PageWithDetailInline は「同居版で一覧＋メモを1クエリで取る」（メモが要る画面）。
func PageWithDetailInline(ctx context.Context, db *sql.DB, tenant string, limit, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id, status, detail FROM split_inline WHERE tenant_id=? AND status=? ORDER BY id LIMIT ?",
			tenant, 1, limit)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}

func measure(ctx context.Context, samples int, fn func() error) expkit.LatencyStats {
	lat := expkit.NewLatency()
	for i := 0; i < samples; i++ {
		t0 := time.Now()
		if err := fn(); err != nil {
			return expkit.LatencyStats{}
		}
		lat.Record(time.Since(t0))
	}
	return lat.Stats()
}

func drain(rows *sql.Rows) error {
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	buf := make([]any, len(cols))
	ptr := make([]any, len(cols))
	for i := range buf {
		ptr[i] = &buf[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptr...); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ScanInline は同居版でテナント全体を走査する集計（メモを取らないが、行が太い）。
func ScanInline(ctx context.Context, db *sql.DB, tenant string, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		var n int64
		return db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM split_inline WHERE tenant_id=? AND status=?", tenant, 1).Scan(&n)
	})
}

// ScanSplit は分割版でテナント全体を走査する集計（狭い表）。
func ScanSplit(ctx context.Context, db *sql.DB, tenant string, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		var n int64
		return db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM split_list WHERE tenant_id=? AND status=?", tenant, 1).Scan(&n)
	})
}
