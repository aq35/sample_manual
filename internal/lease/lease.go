// Package lease は §2.8「マルチコンテナでの担当決め」を実装する。
//
// 「テナント T のワーカーを、いまどのコンテナが持っているか」が決まっていないと、
// 同じワーカーが複数コンテナで同時に動く。接続数も書き込みも二重になり、
// ★普段は動いてしまうので気づかない。
//
// §3 のメモリ方式を採るなら、これは「あったほうがいい」ではなく「無いと壊れる」。
package lease

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aq35/sample_manual/internal/model"
)

// Lease は取得できた担当権。Fence は担当が変わるたびに増える番号で、
// 古い担当が遅れて書き込むのを弾くのに使える。
type Lease struct {
	Tenant    model.TenantID
	Owner     string
	ExpiresAt time.Time
	Fence     uint64
}

// Manager はリースを DB で管理する。Redis でも同じ形になる。
type Manager struct {
	db  *sql.DB
	ttl time.Duration
}

func NewManager(db *sql.DB, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Manager{db: db, ttl: ttl}
}

// TTL はリースの有効期間。
func (m *Manager) TTL() time.Duration { return m.ttl }

// Acquire は担当を取りに行く。すでに他のコンテナが持っていて期限内なら false。
//
// 1文だけで済ませているのは、SELECT ... FOR UPDATE でロックを持ち回すと
// テナント数ぶん詰まるため（§2.7 の「排他ロックで守ろうとしないこと」）。
//
// ★ON DUPLICATE KEY UPDATE の代入は左から順に評価され、後の式は「更新後」の値を見る。
// これを踏むと、期限切れの担当を引き継げないのに、エラーも出ないまま動き続ける。
//
//	悪い順: expires_at を先に伸ばす → 次の owner の条件 (expires_at <= NOW) が
//	        もう成立せず、担当が永久に交代しない
//	この順: fence（旧 owner / 旧 expires_at を見る）→ owner を決める →
//	        expires_at は「owner が自分になったか」だけで判断する
//
// ★`AS new` を付けると、UPDATE 部の裸の列名は「既存行と new のどちらか」が
// 曖昧になり Error 1052 になる。既存行側は必ず表名で修飾する。
func (m *Manager) Acquire(ctx context.Context, tenant model.TenantID, owner string) (Lease, bool, error) {
	const q = `
INSERT INTO worker_lease (tenant_id, owner, expires_at, fence)
VALUES (?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), 1) AS new
ON DUPLICATE KEY UPDATE
  fence      = worker_lease.fence + IF(worker_lease.owner <> new.owner AND worker_lease.expires_at <= NOW(3), 1, 0),
  owner      = IF(worker_lease.owner = new.owner OR worker_lease.expires_at <= NOW(3), new.owner, worker_lease.owner),
  expires_at = IF(worker_lease.owner = new.owner, new.expires_at, worker_lease.expires_at)`

	if _, err := m.db.ExecContext(ctx, q, string(tenant), owner, m.ttl.Microseconds()); err != nil {
		return Lease{}, false, fmt.Errorf("acquire: %w", err)
	}
	l, err := m.Get(ctx, tenant)
	if err != nil {
		return Lease{}, false, err
	}
	return l, l.Owner == owner, nil
}

// Renew は担当を延長する。自分が持っている間だけ成功する。
// 期限切れのまま Renew が通ってしまうと、その隙に別コンテナが取った担当と
// 二重になるので、条件に expires_at > NOW(3) を入れてある。
func (m *Manager) Renew(ctx context.Context, tenant model.TenantID, owner string) (bool, error) {
	const q = `
UPDATE worker_lease
   SET expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND)
 WHERE tenant_id = ? AND owner = ? AND expires_at > NOW(3)`
	res, err := m.db.ExecContext(ctx, q, m.ttl.Microseconds(), string(tenant), owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Release は自分の担当を明け渡す（正常終了時）。落ちた場合は期限切れで拾われる。
func (m *Manager) Release(ctx context.Context, tenant model.TenantID, owner string) error {
	const q = `UPDATE worker_lease SET expires_at = NOW(3) WHERE tenant_id = ? AND owner = ?`
	//smlint:allow rowsaffected 理由: 明け渡しは 0 行でも正しい（すでに期限切れ／別の担当）
	_, err := m.db.ExecContext(ctx, q, string(tenant), owner)
	return err
}

// Get は現在の担当を読む。
func (m *Manager) Get(ctx context.Context, tenant model.TenantID) (Lease, error) {
	const q = `SELECT owner, expires_at, fence FROM worker_lease WHERE tenant_id = ?`
	var l Lease
	l.Tenant = tenant
	err := m.db.QueryRowContext(ctx, q, string(tenant)).Scan(&l.Owner, &l.ExpiresAt, &l.Fence)
	return l, err
}
