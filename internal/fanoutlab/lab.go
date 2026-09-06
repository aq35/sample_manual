// Package fanoutlab は EXP-14（ポーリングの fan-out を畳む）の実験本体。
//
// 問い: 多数テナントを担当する1コンテナが、テナントごとに別々にポーリングすると
// 問い合わせが テナント数 に比例して増える（EXP-12 の fan-out 問題）。
// これを「1回の問い合わせ」に畳めるか。**畳んでもテナント分離を壊さないか。**
//
// ★利用者の懸念（正しい）: テナントを跨いで全取得し、各ワーカーで処理するのは怖い。
// 対策の原則:
//  1. 畳む問い合わせは「**このコンテナが担当（lease）するテナントに限定**した読み取り選択」だけ。
//     全テナント無条件の SELECT は絶対にしない（IN リストは非空・担当集合に限る）。
//  2. 取得した行は**即座にテナント別へ切り分ける**。処理は必ずテナント単位のハンドラで行い、
//     1つのワーカーが複数テナントの行を混ぜて処理しない。
//  3. 切り分けの正しさ（誤ルーティング 0）を検査する。
package fanoutlab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

// SeedTenants は tenants それぞれに perTenant 件の命令を、window に散らして入れる。
func SeedTenants(ctx context.Context, db *sql.DB, tenants []string, perTenant int, window time.Duration, start time.Time) error {
	// まず対象テナントぶんを消す
	for _, tn := range tenants {
		//smlint:allow loopquery 理由: 実験前の後片付け。テナントごとに1回
		//smlint:allow rowsaffected 理由: 後片付け。消える行が 0 でも正しい
		if _, err := db.ExecContext(ctx, "DELETE FROM cmd_command WHERE tenant_id = ?", tn); err != nil {
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO cmd_command (tenant_id, command_id, robot_id, type, payload, state, idem_key, fence, scheduled_for)
		 VALUES (?,?,?,?,?, 'pending', ?, 0, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, tn := range tenants {
		for i := 0; i < perTenant; i++ {
			off := time.Duration(int64(window) * int64(i) / int64(maxi(perTenant, 1)))
			id := fmt.Sprintf("%s-c%05d", tn, i)
			//smlint:allow loopquery 理由: 実験データ投入。準備済み文で入れる
			//smlint:allow rowsaffected 理由: 投入。入るかはエラーで判る
			if _, err := stmt.ExecContext(ctx, tn, id, fmt.Sprintf("r%03d", i%50),
				"move", "", "idem-"+id, start.Add(off)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Result は測定結果。
type Result struct {
	PollQueries int64 // ポーリングの SELECT 回数（fan-out の指標）
	Dispatched  int64
	Misrouted   int64 // ★誤ルーティング（別テナントのハンドラへ行った行）。0 でなければ分離の穴
	Rounds      int64
	Latency     expkit.LatencyStats
	Elapsed     time.Duration
}

// handler はテナント単位の処理。row の tenant が自分と違えば misroute。
type tenantWorker struct {
	tenant    string
	processed int64
	misrouted int64
}

func (w *tenantWorker) handle(rowTenant, _ string, sch time.Time, lat *expkit.Latency) {
	if rowTenant != w.tenant {
		w.misrouted++ // ★ここに来てはいけない。テナント越えの処理
		return
	}
	w.processed++
	lat.Record(time.Since(sch))
}

// PerTenantPoll は「テナントごとに別々にポーリング」する（fan-out する版）。
// 問い合わせはテナント数 × ラウンド数になる。
func PerTenantPoll(ctx context.Context, db *sql.DB, tenants []string, batch int, interval time.Duration) (Result, error) {
	lat := expkit.NewLatency()
	var res Result
	workers := map[string]*tenantWorker{}
	for _, tn := range tenants {
		workers[tn] = &tenantWorker{tenant: tn}
	}
	start := time.Now()

	for {
		res.Rounds++
		remaining := 0
		for _, tn := range tenants {
			now := time.Now()
			//smlint:allow loopquery 理由: これが測定対象そのもの（テナントごとに引く fan-out）
			rows, err := db.QueryContext(ctx,
				`SELECT command_id, scheduled_for FROM cmd_command
				  WHERE tenant_id = ? AND state = 'pending' AND scheduled_for <= ?
				  ORDER BY scheduled_for LIMIT ?`, tn, now, batch)
			res.PollQueries++
			if err != nil {
				return res, err
			}
			var ids []string
			for rows.Next() {
				var id string
				var sch time.Time
				if err := rows.Scan(&id, &sch); err != nil {
					_ = rows.Close()
					return res, err
				}
				ids = append(ids, id)
				// テナント別ワーカーへ（この版はそもそもテナント単位で引いている）
				workers[tn].handle(tn, id, sch, lat)
			}
			_ = rows.Close()
			if err := claim(ctx, db, tn, ids); err != nil {
				return res, err
			}
			res.Dispatched += int64(len(ids))
			remaining += pendingCount(ctx, db, tn)
		}
		if remaining == 0 {
			break
		}
		if interval > 0 {
			time.Sleep(interval)
		}
		if time.Since(start) > 30*time.Second {
			break
		}
	}
	res.Misrouted = sumMisrouted(workers)
	res.Elapsed = time.Since(start)
	res.Latency = lat.Stats()
	return res, nil
}

// FoldedPoll は「担当テナント集合を1クエリで畳んで引き、テナント別に切り分けて処理」する。
//
// ★怖さへの対策がここ:
//   - 引くのは owned（担当）テナントに限定した IN。全テナント無条件ではない。
//   - 取得後、tenant_id で bucket に分け、テナント別ワーカーへ渡す。
//   - 各ワーカーは自分のテナントの行しか処理しない（違えば misrouted としてカウント）。
func FoldedPoll(ctx context.Context, db *sql.DB, owned []string, batch int, interval time.Duration) (Result, error) {
	if len(owned) == 0 {
		return Result{}, fmt.Errorf("担当テナントが空。全テナント無条件のポーリングは禁止")
	}
	lat := expkit.NewLatency()
	var res Result
	workers := map[string]*tenantWorker{}
	for _, tn := range owned {
		workers[tn] = &tenantWorker{tenant: tn}
	}
	start := time.Now()

	// IN プレースホルダ（担当テナントに限定。これが分離の要）
	ph := strings.TrimSuffix(strings.Repeat("?,", len(owned)), ",")
	query := `SELECT tenant_id, command_id, scheduled_for FROM cmd_command
	           WHERE tenant_id IN (` + ph + `) AND state = 'pending' AND scheduled_for <= ?
	        ORDER BY scheduled_for LIMIT ?`

	for {
		res.Rounds++
		now := time.Now()
		args := make([]any, 0, len(owned)+2)
		for _, tn := range owned {
			args = append(args, tn)
		}
		args = append(args, now, batch*len(owned)) // 担当ぶんまとめて

		//smlint:allow loopquery 理由: これは N+1 の逆。1ラウンド1クエリに畳んでいる実験の核
		rows, err := db.QueryContext(ctx, query, args...)
		res.PollQueries++ // ★1ラウンド1クエリ
		if err != nil {
			return res, err
		}
		// テナント別 bucket に切り分ける
		byTenant := map[string][]string{}
		type item struct {
			tenant, id string
			sch        time.Time
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.tenant, &it.id, &it.sch); err != nil {
				_ = rows.Close()
				return res, err
			}
			items = append(items, it)
			byTenant[it.tenant] = append(byTenant[it.tenant], it.id)
		}
		_ = rows.Close()

		// テナント別ワーカーへ渡す（自分の行しか処理しない）
		for _, it := range items {
			w, ok := workers[it.tenant]
			if !ok {
				// 担当外のテナントが返ってきた = IN の絞り込みが壊れている
				res.Misrouted++
				continue
			}
			w.handle(it.tenant, it.id, it.sch, lat)
		}
		// claim もテナント別に
		for tn, ids := range byTenant {
			if err := claim(ctx, db, tn, ids); err != nil {
				return res, err
			}
			res.Dispatched += int64(len(ids))
		}

		remaining := 0
		for _, tn := range owned {
			remaining += pendingCount(ctx, db, tn)
		}
		if remaining == 0 {
			break
		}
		if interval > 0 {
			time.Sleep(interval)
		}
		if time.Since(start) > 30*time.Second {
			break
		}
	}
	res.Misrouted += sumMisrouted(workers)
	res.Elapsed = time.Since(start)
	res.Latency = lat.Stats()
	return res, nil
}

func claim(ctx context.Context, db *sql.DB, tenant string, ids []string) error {
	for _, id := range ids {
		//smlint:allow loopquery 理由: claim。テナント境界を守るため tenant_id 付きで1件ずつ確定
		//smlint:allow rowsaffected 理由: claim。状態遷移で担保
		if _, err := db.ExecContext(ctx,
			`UPDATE cmd_command SET state='dispatched', dispatched_at=?, attempts=attempts+1
			  WHERE tenant_id=? AND command_id=? AND state='pending'`,
			time.Now(), tenant, id); err != nil {
			return err
		}
	}
	return nil
}

func pendingCount(ctx context.Context, db *sql.DB, tenant string) int {
	var n int
	//smlint:allow loopquery 理由: ラウンド終了判定の残数確認
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cmd_command WHERE tenant_id=? AND state='pending'", tenant).Scan(&n)
	return n
}

func sumMisrouted(ws map[string]*tenantWorker) int64 {
	var n int64
	for _, w := range ws {
		n += w.misrouted
	}
	return n
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
