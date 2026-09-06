package secretcache_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/secretcache"
)

func TestThunderingHerd_期限切れに一斉アクセスしても取得は1回(t *testing.T) {
	var calls atomic.Int64
	fetch := func(ctx context.Context, tn model.TenantID, name string) (string, time.Time, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond) // 遅い元（SecretManager）を模擬
		return fmt.Sprintf("%s/%s/v%d", tn, name, calls.Load()), time.Now().Add(time.Hour), nil
	}
	c := secretcache.New(fetch, secretcache.Options{})

	// 100 goroutine が同じ (tenant,name) を一斉に取りに行く（初回=キャッシュ空）
	const g = 100
	var wg sync.WaitGroup
	got := make([]string, g)
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.Get(context.Background(), "t1", "db-password")
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			got[i] = v
		}(i)
	}
	wg.Wait()

	// ★元を叩いたのは1回だけ（singleflight）。100回叩いていない。
	if n := c.Fetches(); n != 1 {
		t.Errorf("thundering herd: 元を %d 回叩いた（1 回のはず）", n)
	}
	// 全員が同じ値を得ている
	for i := 1; i < g; i++ {
		if got[i] != got[0] {
			t.Fatalf("goroutine ごとに違う値: %q vs %q", got[i], got[0])
		}
	}
	t.Logf("100 goroutine 同時アクセスで元は %d 回だけ / %s", c.Fetches(), c)
}

func TestTenant分離_同じ名前でも混ざらない(t *testing.T) {
	fetch := func(ctx context.Context, tn model.TenantID, name string) (string, time.Time, error) {
		return string(tn) + "-secret", time.Now().Add(time.Hour), nil
	}
	c := secretcache.New(fetch, secretcache.Options{})
	a, _ := c.Get(context.Background(), "t-alpha", "db-password") // 同じ name
	b, _ := c.Get(context.Background(), "t-bravo", "db-password")
	if a != "t-alpha-secret" || b != "t-bravo-secret" {
		t.Errorf("テナント越え: a=%q b=%q", a, b)
	}
	// A を捨てても B は残る
	c.EvictTenant("t-alpha")
	if b2, _ := c.Get(context.Background(), "t-bravo", "db-password"); b2 != "t-bravo-secret" {
		t.Errorf("A の Evict が B に波及: %q", b2)
	}
}

func TestExpiry_期限切れで取り直す(t *testing.T) {
	var calls atomic.Int64
	now := time.Now()
	clock := now
	fetch := func(ctx context.Context, tn model.TenantID, name string) (string, time.Time, error) {
		n := calls.Add(1)
		return fmt.Sprintf("v%d", n), clock.Add(100 * time.Millisecond), nil // 100ms で切れる
	}
	c := secretcache.New(fetch, secretcache.Options{
		RefreshBefore: 10 * time.Millisecond,
		Now:           func() time.Time { return clock },
	})
	if v, _ := c.Get(context.Background(), "t", "k"); v != "v1" {
		t.Fatalf("初回 %q", v)
	}
	// 期限内は取り直さない
	clock = now.Add(50 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "t", "k"); v != "v1" {
		t.Errorf("期限内なのに取り直した: %q", v)
	}
	if c.Fetches() != 1 {
		t.Errorf("期限内で %d 回叩いた", c.Fetches())
	}
	// 期限を過ぎたら取り直す
	clock = now.Add(200 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "t", "k"); v != "v2" {
		t.Errorf("期限切れで取り直せていない: %q", v)
	}
}

func TestFailOpen_取得失敗でも古い値を延命(t *testing.T) {
	var fail atomic.Bool
	now := time.Now()
	clock := now
	fetch := func(ctx context.Context, tn model.TenantID, name string) (string, time.Time, error) {
		if fail.Load() {
			return "", time.Time{}, fmt.Errorf("SecretManager 落ちてる")
		}
		return "good", clock.Add(100 * time.Millisecond), nil
	}
	c := secretcache.New(fetch, secretcache.Options{Now: func() time.Time { return clock }})
	if v, _ := c.Get(context.Background(), "t", "k"); v != "good" {
		t.Fatal("初回失敗")
	}
	// 期限切れ後、元が落ちていても、古い値を返して延命する（即座に使用不能にしない）
	fail.Store(true)
	clock = now.Add(200 * time.Millisecond)
	v, err := c.Get(context.Background(), "t", "k")
	if err != nil || v != "good" {
		t.Errorf("fail-open で延命できていない: v=%q err=%v", v, err)
	}
	t.Log("元が落ちても、握っている古い値で延命する（完全に無い場合だけエラー）")
}

func TestFailClosed_一度も取れていなければエラー(t *testing.T) {
	fetch := func(ctx context.Context, tn model.TenantID, name string) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("落ちてる")
	}
	c := secretcache.New(fetch, secretcache.Options{})
	if _, err := c.Get(context.Background(), "t", "k"); err == nil {
		t.Error("一度も取れていないのにエラーにならない（fail-closed のはず）")
	}
}
