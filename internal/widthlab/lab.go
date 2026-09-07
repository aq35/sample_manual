// Package widthlab は EXP-21（VARCHAR と TEXT の重さ・SELECT * の効き）の実験本体。
//
// 問い:
//   - 太い memo を VARCHAR で同居させると、memo を SELECT しなくても舐めが重いのか。
//   - TEXT にすると舐めは軽いのか（本体が行の外だから）。
//   - では TEXT を SELECT *（memo 込み）で取ると、また重くなるのか。
//
// 「舐める」= 行を順に読むこと（COUNT や範囲スキャン）。索引で狙い撃ちできないぶんは端から読む。
package widthlab

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
			return fmt.Errorf("width schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// Seed は 3KB を varchar / text_small へ、24KB を text_big へ、narrow には memo 無しで入れる。
// 狙い: 同じ TEXT でも「行に収まる 3KB は inline」「収まらない 24KB は off-page」を作り分ける。
func Seed(ctx context.Context, db *sql.DB, tenant string, rows int) error {
	for _, t := range []string{"w_varchar", "w_text_small", "w_text_big", "w_narrow"} {
		//smlint:allow loopquery 理由: 実験前の後片付け
		//smlint:allow rowsaffected 理由: 後片付け
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	memoSmall := strings.Repeat("理由", 500)  // 約 3KB（行に収まる）
	memoBig := strings.Repeat("理由", 4000)   // 約 24KB（行に収まらない→off-page）
	insert := func(table, memo string, withMemo bool) error {
		const chunk = 200
		for start := 0; start < rows; start += chunk {
			end := start + chunk
			if end > rows {
				end = rows
			}
			var b strings.Builder
			if withMemo {
				b.WriteString("INSERT INTO " + table + " (tenant_id, id, status, memo) VALUES ")
			} else {
				b.WriteString("INSERT INTO " + table + " (tenant_id, id, status) VALUES ")
			}
			var args []any
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				if withMemo {
					b.WriteString("(?,?,?,?)")
					args = append(args, tenant, i, i%5, memo)
				} else {
					b.WriteString("(?,?,?)")
					args = append(args, tenant, i, i%5)
				}
			}
			//smlint:allow loopquery 理由: 一括投入
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := insert("w_varchar", memoSmall, true); err != nil {
		return err
	}
	if err := insert("w_text_small", memoSmall, true); err != nil {
		return err
	}
	if err := insert("w_text_big", memoBig, true); err != nil {
		return err
	}
	return insert("w_narrow", "", false)
}

// CountScan はテナント全体を舐める集計（memo は SELECT しない）。
// status 索引が無いので、PK 範囲を読みながら status を確かめる＝行を順に読む。
// 行が太い（varchar 同居）ほどページ数が増えて重い。
func CountScan(ctx context.Context, db *sql.DB, table, tenant string, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		var n int64
		return db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE tenant_id=? AND status=?", tenant, 1).Scan(&n)
	})
}

// RangeReadNoMemo は範囲を読むが memo は取らない（SELECT id, status）。
func RangeReadNoMemo(ctx context.Context, db *sql.DB, table, tenant string, upto, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id, status FROM "+table+" WHERE tenant_id=? AND id < ?", tenant, upto)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}

// RangeReadWithMemo は範囲を読み、memo も取る（SELECT id, status, memo ＝ SELECT * 相当）。
// TEXT では本体が行の外にあるので、1行ごとに外を取りに行く（余計な IO）。
func RangeReadWithMemo(ctx context.Context, db *sql.DB, table, tenant string, upto, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx,
			"SELECT id, status, memo FROM "+table+" WHERE tenant_id=? AND id < ?", tenant, upto)
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
