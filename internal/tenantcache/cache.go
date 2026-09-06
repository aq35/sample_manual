// Package tenantcache は「テナントを跨がせない」メモリキャッシュ。
//
// DB 側は repo.Scope（:tenant 束縛）でテナント越えを防いでいる。
// メモリでも同じ事故が起きる: フラットな map[robotID]State に複数テナントを入れると、
// robot_id が衝突した瞬間に別テナントの値が見える（tenant_isolation の罠と同型）。
//
// ★守り方は DB と同じ「構造で禁じる」:
//   - キーは必ず (tenant, key) の2段。tenant 無しでは引けない型にする。
//   - 取得 API は tenant を必須引数にする（Scope が :tenant を必須にするのと同じ）。
//
// 権威（source of truth）は DB。これは消えても DB から再構築できるキャッシュで、
// 「メモリにだけある状態」を作らないのが前提（EXP-1 の outbox 参照）。
package tenantcache

import (
	"sync"

	"github.com/aq35/sample_manual/internal/model"
)

// Cache はテナントごとに隔離した key→value のキャッシュ。
//
// 実装は map[tenant]map[key]value の2段。1段目でテナントを分けるので、
// 別テナントの key と衝突しても混ざらない。
type Cache[V any] struct {
	mu   sync.RWMutex
	data map[model.TenantID]map[string]V
	// max はテナントごとの上限（0 で無制限）。1テナントがメモリを食い潰さないため。
	max int
}

// New は空のキャッシュ。perTenantMax はテナントごとの最大件数（0 で無制限）。
func New[V any](perTenantMax int) *Cache[V] {
	return &Cache[V]{data: map[model.TenantID]map[string]V{}, max: perTenantMax}
}

// Put は (tenant, key) に値を入れる。★tenant を必須にしているのが要点。
// 上限に達していて新規キーなら false（入れなかった）を返す。
func (c *Cache[V]) Put(tenant model.TenantID, key string, v V) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.data[tenant]
	if m == nil {
		m = map[string]V{}
		c.data[tenant] = m
	}
	if c.max > 0 {
		if _, exists := m[key]; !exists && len(m) >= c.max {
			return false // このテナントは満杯。呼び出し側が捨てる/coalesce を決める（EXP-4）
		}
	}
	m[key] = v
	return true
}

// Get は (tenant, key) の値。tenant を跨いでは絶対に返さない。
func (c *Cache[V]) Get(tenant model.TenantID, key string) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var zero V
	m := c.data[tenant]
	if m == nil {
		return zero, false
	}
	v, ok := m[key]
	return v, ok
}

// Delete は (tenant, key) を消す。
func (c *Cache[V]) Delete(tenant model.TenantID, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.data[tenant]; m != nil {
		delete(m, key)
	}
}

// Len はそのテナントの件数。
func (c *Cache[V]) Len(tenant model.TenantID) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data[tenant])
}

// Snapshot はそのテナントの全件のコピー（他テナントは含まない）。
// 呼び出し側が map を書き換えても内部に影響しないよう、コピーを返す。
func (c *Cache[V]) Snapshot(tenant model.TenantID) map[string]V {
	c.mu.RLock()
	defer c.mu.RUnlock()
	src := c.data[tenant]
	out := make(map[string]V, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// EvictTenant はテナント丸ごと捨てる（担当が別コンテナへ移ったとき等）。
//
// ★lease を失ったテナントのキャッシュは必ず捨てる。持ち続けると、
// 別コンテナが担当している間に古い値を配ってしまう（fencing の逆流）。
func (c *Cache[V]) EvictTenant(tenant model.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, tenant)
}

// Tenants は今キャッシュを持っているテナント一覧（運用の可視化用）。
func (c *Cache[V]) Tenants() []model.TenantID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]model.TenantID, 0, len(c.data))
	for t := range c.data {
		out = append(out, t)
	}
	return out
}
