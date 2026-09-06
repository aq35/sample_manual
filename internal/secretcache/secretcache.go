// Package secretcache は「有効期限つきの秘密」を扱うキャッシュ。
//
// SecretManager 相当（取得が遅く・課金され・レート制限がある）を前提に:
//   - 取得結果をメモリにキャッシュする（毎回叩かない）。ディスクには書かない。
//   - **期限つきのリースとして扱う**（EXP-2 の lease と同じ発想）。
//     期限の手前で先回りして更新し、期限ちょうどで切れるのを待たない。
//   - **テナントで分離する**（(tenant,name) をキーにする。フラットな map[name] にしない）。
//   - **thundering herd を防ぐ**（期限切れの瞬間に全 goroutine が一斉に取りに行かないよう
//     singleflight で1本にまとめる）。
//
// 権威は SecretManager。これは消えても取り直せるキャッシュ。
package secretcache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aq35/sample_manual/internal/model"
)

// Fetch は元（SecretManager 等）から取ってくる関数。値と有効期限を返す。
type Fetch[V any] func(ctx context.Context, tenant model.TenantID, name string) (V, time.Time, error)

// entry はキャッシュ1件。
type entry[V any] struct {
	val       V
	expiresAt time.Time
}

// Cache は期限つき秘密のキャッシュ。
type Cache[V any] struct {
	fetch         Fetch[V]
	refreshBefore time.Duration // 期限のこれだけ手前になったら先回りで更新

	mu   sync.RWMutex
	data map[string]entry[V] // キーは tenant\x00name（テナント分離）
	sf   singleflight.Group  // 同じキーの取得を1本にまとめる

	fetches   atomic.Int64 // 元を叩いた回数（thundering herd の検査用）
	refreshes atomic.Int64 // 先回り更新の回数
	now       func() time.Time
}

// Options は調整値。
type Options struct {
	RefreshBefore time.Duration    // 既定 30秒
	Now           func() time.Time // テスト用
}

// New はキャッシュを作る。
func New[V any](fetch Fetch[V], opt Options) *Cache[V] {
	if opt.RefreshBefore <= 0 {
		opt.RefreshBefore = 30 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Cache[V]{
		fetch: fetch, refreshBefore: opt.RefreshBefore,
		data: map[string]entry[V]{}, now: opt.Now,
	}
}

func key(tenant model.TenantID, name string) string {
	return string(tenant) + "\x00" + name
}

// Get は (tenant, name) の秘密を返す。
//
//   - キャッシュにあり、期限まで余裕 → そのまま返す。
//   - 期限が近い（でもまだ有効）→ その値を返しつつ、裏で先回り更新する。
//   - 無い/期限切れ → singleflight で1本だけ取りに行き、全員でその結果を待つ。
func (c *Cache[V]) Get(ctx context.Context, tenant model.TenantID, name string) (V, error) {
	k := key(tenant, name)
	now := c.now()

	c.mu.RLock()
	e, ok := c.data[k]
	c.mu.RUnlock()

	if ok && now.Before(e.expiresAt) {
		// まだ有効。期限が近ければ裏で更新（値は今のを返す）。
		if e.expiresAt.Sub(now) <= c.refreshBefore {
			c.refreshAsync(tenant, name, k)
		}
		return e.val, nil
	}

	// 無い or 期限切れ → 1本だけ取りに行く（他は待って結果を共有）。
	v, err, _ := c.sf.Do(k, func() (any, error) {
		val, exp, err := c.doFetch(ctx, tenant, name)
		if err != nil {
			return nil, err
		}
		c.store(k, val, exp)
		return val, nil
	})
	if err != nil {
		// 取れないとき、期限切れでも古い値を握っていれば**それを返す**（即座に使用不能にしない）。
		// ただし完全に無い場合はエラー。
		if ok {
			return e.val, nil
		}
		var zero V
		return zero, err
	}
	return v.(V), nil
}

// refreshAsync は先回り更新（1本だけ）。値の差し替えはできたときだけ。
func (c *Cache[V]) refreshAsync(tenant model.TenantID, name, k string) {
	go func() {
		c.sf.Do(k+"\x01refresh", func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			val, exp, err := c.doFetch(ctx, tenant, name)
			if err != nil {
				return nil, err // 失敗しても古い値は残る（fail-open で延命）
			}
			c.store(k, val, exp)
			c.refreshes.Add(1)
			return val, nil
		})
	}()
}

func (c *Cache[V]) doFetch(ctx context.Context, tenant model.TenantID, name string) (V, time.Time, error) {
	c.fetches.Add(1)
	return c.fetch(ctx, tenant, name)
}

func (c *Cache[V]) store(k string, val V, exp time.Time) {
	c.mu.Lock()
	c.data[k] = entry[V]{val: val, expiresAt: exp}
	c.mu.Unlock()
}

// EvictTenant はテナントの秘密を丸ごと捨てる（担当が移ったとき等）。
func (c *Cache[V]) EvictTenant(tenant model.TenantID) {
	prefix := string(tenant) + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.data, k)
		}
	}
}

// Fetches は元を叩いた回数（thundering herd が起きていないかの検査用）。
func (c *Cache[V]) Fetches() int64 { return c.fetches.Load() }

// Refreshes は先回り更新の回数。
func (c *Cache[V]) Refreshes() int64 { return c.refreshes.Load() }

// String は状態の要約。
func (c *Cache[V]) String() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return fmt.Sprintf("secretcache{entries=%d fetches=%d refreshes=%d}",
		len(c.data), c.fetches.Load(), c.refreshes.Load())
}
