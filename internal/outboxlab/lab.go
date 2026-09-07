// Package outboxlab は EXP-44（トランザクショナル outbox で外部作用を exactly-once に）の実験本体。
//
// ワーカーは「DB 更新」と「外部呼び出し（ロボット指示）」をまたぐ。素朴にやると、間で落ちると
// 二重指示 or 未送信になる。outbox: (1) 業務変更と「送信意図(outbox 行)」を1トランザクションで
// 原子的に書く → (2) 別の relay が未送信を読み外部へ at-least-once 送信し sent に落とす。
// 外部が冪等（idem_key で重複を無視）なら、再送があっても効果は1回＝実質 exactly-once。
package outboxlab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// Robot は外部システム（ロボット）の擬似。呼ばれた回数(attempts)と、実際に適用された効果(effects)を数える。
type Robot struct {
	mu       sync.Mutex
	seen     map[string]bool
	attempts int
	effects  int
}

func NewRobot() *Robot { return &Robot{seen: map[string]bool{}} }

// SendIdempotent は冪等な受け側: 同じ idem_key の再送は効果を増やさない（重複を無視）。
func (r *Robot) SendIdempotent(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if !r.seen[key] {
		r.seen[key] = true
		r.effects++
	}
}

// SendNaive は非冪等な受け側: 呼ばれるたびに効果が起きる（再送＝二重効果）。
func (r *Robot) SendNaive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	r.effects++
}

func (r *Robot) Attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}
func (r *Robot) Effects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.effects
}

func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS outbox",
		`CREATE TABLE outbox (
		   tenant_id  VARCHAR(32) NOT NULL,
		   id         BIGINT      NOT NULL,
		   idem_key   VARCHAR(64) NOT NULL,
		   state      ENUM('pending','sent') NOT NULL DEFAULT 'pending',
		   PRIMARY KEY (tenant_id, id),
		   KEY pend (tenant_id, state, id)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// SeedOutbox は「業務変更＋送信意図」を1トランザクションで n 件ぶん書いた状態を作る
// （ここでは outbox 行の投入で代表。実際は業務行の INSERT/UPDATE と同じ tx に入れる）。
func SeedOutbox(ctx context.Context, db *sql.DB, tenant string, n int) error {
	//smlint:allow rowsaffected 理由: シードの後片付け。件数は使わない
	if _, err := db.ExecContext(ctx, "DELETE FROM outbox WHERE tenant_id=?", tenant); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var b strings.Builder
	b.WriteString("INSERT INTO outbox (tenant_id, id, idem_key) VALUES ")
	var args []any
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?,?)")
		args = append(args, tenant, i, fmt.Sprintf("%s-k%04d", tenant, i))
	}
	if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
		return err
	}
	return tx.Commit()
}

// Relay は未送信(pending)を1周ぶん処理する。各行: 外部へ SendIdempotent → sent に落とす。
// crashBeforeMark(id) が true の行は「送った直後・sent にする前に落ちた」を模し、pending のまま残す
// （次周で再送されるが、冪等なので効果は増えない）。
func Relay(ctx context.Context, db *sql.DB, tenant string, ext *Robot, crashBeforeMark func(int64) bool) error {
	rows, err := db.QueryContext(ctx,
		"SELECT id, idem_key FROM outbox WHERE tenant_id=? AND state='pending' ORDER BY id", tenant)
	if err != nil {
		return err
	}
	type row struct {
		id  int64
		key string
	}
	var pend []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key); err != nil {
			_ = rows.Close()
			return err
		}
		pend = append(pend, r)
	}
	_ = rows.Close()
	for _, r := range pend {
		ext.SendIdempotent(r.key) // at-least-once（外部へ送る）
		if crashBeforeMark != nil && crashBeforeMark(r.id) {
			continue // sent にする前に落ちた → pending のまま。次周で再送
		}
		//smlint:allow loopquery 理由: relay が pending を1件ずつ sent に落とす（処理そのもの・実験対象）
		//smlint:allow rowsaffected 理由: sent へ落とす。件数は使わない
		if _, err := db.ExecContext(ctx,
			"UPDATE outbox SET state='sent' WHERE tenant_id=? AND id=?", tenant, r.id); err != nil {
			return err
		}
	}
	return nil
}

// PendingCount は未送信の数。
func PendingCount(ctx context.Context, db *sql.DB, tenant string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM outbox WHERE tenant_id=? AND state='pending'", tenant).Scan(&n)
	return n, err
}

// NaiveDeliver は outbox 無し・非冪等の素朴配信（対照）。各命令: 外部へ送る → done 印。
// crashFirst(i) が true の命令は初回だけ「送った直後・done にする前に落ちた」を模す → 再送で二重効果。
func NaiveDeliver(n int, ext *Robot, crashFirst func(int) bool) {
	done := make([]bool, n)
	crashed := make([]bool, n)
	for pass := 0; pass < 3; pass++ {
		all := true
		for i := 0; i < n; i++ {
			if done[i] {
				continue
			}
			all = false
			ext.SendNaive() // 効果が起きる（非冪等）
			if !crashed[i] && crashFirst != nil && crashFirst(i) {
				crashed[i] = true
				continue // done にする前に落ちた → 次周で再送（二重効果）
			}
			done[i] = true
		}
		if all {
			break
		}
	}
}
