// Package statuslab は EXP-22（ステータスでテーブルを分けるべきか）の実験本体。
//
// 状態: 0=pending 1=queued 2=in-progress（=active・少数）/ 3=done 4=failed 5=cancelled（=terminal・大量）
//
// 問い:
//   - status で絞る一覧は、1テーブル＋status索引で足りるのか（分割しなくても速いか）。
//   - 状態を別テーブルに分けると、遷移は「引っ越し（DELETE+INSERT）」になり UPDATE より重いのか。
//   - 終端行が溜まると、テーブル全体を舐める操作（全体 COUNT・GROUP BY）は重くなるのか
//     （＝hot/cold 分離やアーカイブの動機になるか）。
package statuslab

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

// 状態コード。0=pending 1=queued 2=in-progress（active）/ 3=done 4=failed 5=cancelled（terminal）。
const statusPending = 0

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定 DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("status schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// ActiveStart は active 行が始まる id（＝古い順に処理が進み、直近ぶんだけ active な状態を模す）。
// 現実のキュー: 古い id はほぼ terminal（処理済）、直近の id が active。だから
// 「古い順(id ASC)に pending を取る」worker クエリは、索引が無いと大量の terminal を舐める。
func ActiveStart(total, activeCount int) int { return total - activeCount }

// Seed は直近 activeCount 件（id が大きい方）を active、それ以前を terminal として入れる。
//   - st_one / st_noidx: 全件（active + 大量の terminal が溜まった状態）
//   - st_active: active 行のみ / st_terminal: terminal 行のみ（分割案）
//   - st_hot: active 行のみ（hot/cold の hot）
func Seed(ctx context.Context, db *sql.DB, tenant string, total, activeCount int) error {
	for _, t := range []string{"st_one", "st_noidx", "st_active", "st_terminal", "st_hot"} {
		//smlint:allow loopquery 理由: 実験前の後片付け
		//smlint:allow rowsaffected 理由: 後片付け
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	start := ActiveStart(total, activeCount)
	// status: 直近（id >= start）は active(id%3=pending/queued/in-progress)、それ以前は terminal(3 + id%3)
	statusOf := func(id int) int {
		if id >= start {
			return id % 3
		}
		return 3 + id%3
	}
	insert := func(table string, from, to int) error {
		const chunk = 500
		for start := from; start < to; start += chunk {
			end := start + chunk
			if end > to {
				end = to
			}
			var b strings.Builder
			b.WriteString("INSERT INTO " + table + " (tenant_id, id, status, created_at) VALUES ")
			var args []any
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?,?,?,?)")
				args = append(args, tenant, i, statusOf(i), time.Now().Add(-time.Duration(i)*time.Second))
			}
			//smlint:allow loopquery 理由: 一括投入（500行/文）
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := insert("st_one", 0, total); err != nil {
		return err
	}
	if err := insert("st_noidx", 0, total); err != nil {
		return err
	}
	if err := insert("st_active", start, total); err != nil {
		return err
	}
	if err := insert("st_terminal", 0, start); err != nil {
		return err
	}
	return insert("st_hot", start, total)
}

// FindPending は worker の定番クエリ「pending を古い順に1ページ取る」。
// status=0 を id 昇順に LIMIT。古い id はほぼ terminal なので、索引が無いと大量に舐める。
func FindPending(ctx context.Context, db *sql.DB, table, tenant string, limit, samples int) expkit.LatencyStats {
	q := "SELECT id, status FROM " + table + " WHERE tenant_id=? AND status=? ORDER BY id LIMIT ?"
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx, q, tenant, statusPending, limit)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}

// CountAll はテナントの全行を数える（terminal 込み。全体を舐める）。
func CountAll(ctx context.Context, db *sql.DB, table, tenant string, samples int) expkit.LatencyStats {
	return measure(ctx, samples, func() error {
		var n int64
		return db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", tenant).Scan(&n)
	})
}

// TransitionOneTable は「1テーブルで UPDATE 一発の状態遷移」を count 回、別々の active 行に対して行う。
// active(startID..startID+count-1) を done(3) に落とす。返すのは1遷移あたりのレイテンシ統計。
func TransitionOneTable(ctx context.Context, db *sql.DB, tenant string, startID, count int) expkit.LatencyStats {
	lat := expkit.NewLatency()
	for i := 0; i < count; i++ {
		t0 := time.Now()
		//smlint:allow loopquery 理由: 状態遷移を1件ずつ測る（遷移コストの測定そのもの）
		//smlint:allow rowsaffected 理由: 測定対象は所要時間で、件数は使わない
		if _, err := db.ExecContext(ctx,
			"UPDATE st_one SET status=3 WHERE tenant_id=? AND id=?", tenant, startID+i); err != nil {
			return expkit.LatencyStats{}
		}
		lat.Record(time.Since(t0))
	}
	return lat.Stats()
}

// TransitionSplit は「状態を別テーブルに分けたときの遷移」を count 回行う。
// active 表から DELETE し terminal 表へ INSERT する（＝別テーブルへの引っ越し）。トランザクションで囲う。
func TransitionSplit(ctx context.Context, db *sql.DB, tenant string, startID, count int) expkit.LatencyStats {
	lat := expkit.NewLatency()
	for i := 0; i < count; i++ {
		t0 := time.Now()
		if err := moveOne(ctx, db, tenant, startID+i); err != nil {
			return expkit.LatencyStats{}
		}
		lat.Record(time.Since(t0))
	}
	return lat.Stats()
}

func moveOne(ctx context.Context, db *sql.DB, tenant string, id int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	//smlint:allow rowsaffected 理由: 引っ越しの所要時間を測る。件数は使わない
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM st_active WHERE tenant_id=? AND id=?", tenant, id); err != nil {
		return err
	}
	//smlint:allow rowsaffected 理由: 同上
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO st_terminal (tenant_id, id, status, created_at) VALUES (?,?,3,NOW())",
		tenant, id); err != nil {
		return err
	}
	return tx.Commit()
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
