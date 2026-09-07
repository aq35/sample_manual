// Package redundancylab は EXP-30（冗長化＝複数レプリカでアプリはどうあるべきか）の実験本体。
//
// 冗長化すると同じ仕事を N 個のレプリカが同時に見る。安全に動かす鍵は3つ:
//   1. 原子的な claim（条件つき UPDATE）で、1件を取れるのは1レプリカだけにする（二重処理を防ぐ）。
//   2. fence（世代番号）で、遅れた/切り離された古い担当の書き込みを弾く（正しさ）。
//   3. 接続予算をレプリカ数で割る（N 台 × 1台の取り分 ≤ DB の上限）。
//
// ここは並行の意味論を見るので生の *sql.DB を直接使う。
package redundancylab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
)

//go:embed schema.sql
var schemaSQL string

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("redundancy schema: %w", err)
		}
	}
	return nil
}

// SeedQueue は tenant に pending を n 件入れる。
func SeedQueue(ctx context.Context, db *sql.DB, tenant string, n int) error {
	//smlint:allow rowsaffected 理由: シードの後片付け。件数は使わない
	if _, err := db.ExecContext(ctx, "DELETE FROM redq WHERE tenant_id=?", tenant); err != nil {
		return err
	}
	const chunk = 500
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		var b strings.Builder
		b.WriteString("INSERT INTO redq (tenant_id, id, state) VALUES ")
		var args []any
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			b.WriteString("(?,?, 'pending')")
			args = append(args, tenant, i)
		}
		//smlint:allow loopquery 理由: シード。500行/文の一括投入をチャンクごとに流すだけ
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// ClaimRace は replicas 個のレプリカが同じキューを取り合う。各レプリカは
// 「pending を1件見つける → その id を条件つきで claim する（取れたら自分のもの）」を繰り返す。
// 原子的な claim なので、同じ id を2つのレプリカが同時に見ても、UPDATE が当たるのは片方だけ。
// 返り値は owner ごとの claim 数。
func ClaimRace(ctx context.Context, db *sql.DB, tenant string, replicas int) (map[string]int64, error) {
	counts := make(map[string]int64, replicas)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for r := 0; r < replicas; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			owner := fmt.Sprintf("replica-%d", r)
			var mine int64
			for {
				// pending を1件見つける
				var id int64
				//smlint:allow loopquery 理由: キューから1件ずつ取り合う claim ループそのもの（実験対象）
				err := db.QueryRowContext(ctx,
					"SELECT id FROM redq WHERE tenant_id=? AND state='pending' ORDER BY id LIMIT 1", tenant).Scan(&id)
				if err == sql.ErrNoRows {
					break // もう無い
				}
				if err != nil {
					errs[r] = err
					return
				}
				// その id を条件つきで claim（取れたら1行、他レプリカに先を越されたら0行）
				//smlint:allow loopquery 理由: claim ループ本体（実験対象）
				//smlint:allow rowsaffected 理由: affected は下で取れたか判定に使っている
				res, err := db.ExecContext(ctx,
					"UPDATE redq SET state='claimed', owner=? WHERE tenant_id=? AND id=? AND state='pending'",
					owner, tenant, id)
				if err != nil {
					errs[r] = err
					return
				}
				if aff, _ := res.RowsAffected(); aff == 1 {
					mine++
				}
				// aff==0 は他レプリカが取った。ループして次の pending へ。
			}
			mu.Lock()
			counts[owner] = mine
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return counts, nil
}

// QueueStats は claimed 総数と、まだ pending の数を返す（取りこぼし・二重の検査用）。
func QueueStats(ctx context.Context, db *sql.DB, tenant string) (claimed, pending int64, err error) {
	if err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM redq WHERE tenant_id=? AND state='claimed'", tenant).Scan(&claimed); err != nil {
		return
	}
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM redq WHERE tenant_id=? AND state='pending'", tenant).Scan(&pending)
	return
}

// InitFence は (tenant, robot) の状態行を fence とともに初期化する。
func InitFence(ctx context.Context, db *sql.DB, tenant, robot string, fence int64, val string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO redstate (tenant_id, robot_id, val, fence) VALUES (?,?,?,?)
		 ON DUPLICATE KEY UPDATE val=VALUES(val), fence=VALUES(fence)`,
		tenant, robot, val, fence)
	return err
}

// GuardedWrite は fence で守った書き込み。書き手の fence が、今保存されている fence 以上のときだけ
// 通す（＝古い担当＝小さい fence の書き込みは弾かれる）。accepted=false なら弾かれた。
func GuardedWrite(ctx context.Context, db *sql.DB, tenant, robot, val string, writerFence int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		"UPDATE redstate SET val=?, fence=? WHERE tenant_id=? AND robot_id=? AND fence <= ?",
		val, writerFence, tenant, robot, writerFence)
	if err != nil {
		return false, err
	}
	aff, _ := res.RowsAffected()
	return aff == 1, nil
}

// CurrentVal は今の値と fence を返す（検証用）。
func CurrentVal(ctx context.Context, db *sql.DB, tenant, robot string) (string, int64, error) {
	var v string
	var f int64
	err := db.QueryRowContext(ctx,
		"SELECT val, fence FROM redstate WHERE tenant_id=? AND robot_id=?", tenant, robot).Scan(&v, &f)
	return v, f, err
}
