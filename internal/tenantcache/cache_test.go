package tenantcache_test

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/tenantcache"
)

const (
	tA = model.TenantID("t-alpha")
	tB = model.TenantID("t-bravo")
)

func TestTenantCache_同じキーでも混ざらない(t *testing.T) {
	c := tenantcache.New[string](0)
	// 両テナントに**同じキー**で別の値
	c.Put(tA, "robot1", "Aの値")
	c.Put(tB, "robot1", "Bの値")

	if v, _ := c.Get(tA, "robot1"); v != "Aの値" {
		t.Errorf("A: %q", v)
	}
	if v, _ := c.Get(tB, "robot1"); v != "Bの値" {
		t.Errorf("B: %q", v)
	}
	// A から B のキーは見えない、が成り立つのは「同じキー空間ではない」から
	c.Delete(tA, "robot1")
	if _, ok := c.Get(tB, "robot1"); !ok {
		t.Error("A の Delete が B を消した（テナント越え削除）")
	}
}

// プロパティテスト: ランダムな (tenant,key,value) を大量に入れ、
// どのテナントから引いても、そのテナントに入れた値しか返らない。
func TestTenantCache_プロパティ_越えない(t *testing.T) {
	c := tenantcache.New[string](0)
	tenants := []model.TenantID{"t1", "t2", "t3"}
	// 真実の対照表（テスト側で持つ）
	truth := map[model.TenantID]map[string]string{
		"t1": {}, "t2": {}, "t3": {},
	}
	rng := rand.New(rand.NewSource(7))

	for i := 0; i < 5000; i++ {
		tn := tenants[rng.Intn(len(tenants))]
		key := fmt.Sprintf("k%d", rng.Intn(50)) // キーは全テナントで衝突する
		val := fmt.Sprintf("%s:%d", tn, i)
		c.Put(tn, key, val)
		truth[tn][key] = val
	}
	// 全テナント・全キーで、キャッシュと真実が一致する（越えが無い）
	for _, tn := range tenants {
		for k := 0; k < 50; k++ {
			key := fmt.Sprintf("k%d", k)
			got, ok := c.Get(tn, key)
			want, wok := truth[tn][key]
			if ok != wok || got != want {
				t.Fatalf("[%s/%s] got=(%q,%v) want=(%q,%v)", tn, key, got, ok, want, wok)
			}
		}
	}
	t.Logf("3テナント×50キー衝突・5000書き込みで越え0")
}

func TestTenantCache_上限とEvict(t *testing.T) {
	c := tenantcache.New[int](3) // テナントごと最大3件
	for i := 0; i < 3; i++ {
		if !c.Put(tA, fmt.Sprintf("k%d", i), i) {
			t.Fatalf("3件目までは入るはず: %d", i)
		}
	}
	// 4件目（新規キー）は満杯で false
	if c.Put(tA, "k3", 3) {
		t.Error("上限超過の新規キーは false のはず")
	}
	// 既存キーの更新は満杯でも通る
	if !c.Put(tA, "k0", 99) {
		t.Error("既存キーの更新は通るはず")
	}
	// B は A と別枠（A が満杯でも B は入る）
	if !c.Put(tB, "k0", 1) {
		t.Error("別テナントは別枠のはず")
	}
	// lease を失った想定で A を丸ごと捨てる
	c.EvictTenant(tA)
	if c.Len(tA) != 0 {
		t.Error("EvictTenant で A は空になるはず")
	}
	if c.Len(tB) != 1 {
		t.Error("B は残るはず")
	}
}

func TestTenantCache_並行アクセスで壊れない(t *testing.T) {
	c := tenantcache.New[int](0)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tn := model.TenantID(fmt.Sprintf("t%d", w%3))
			for i := 0; i < 1000; i++ {
				c.Put(tn, fmt.Sprintf("k%d", i%20), i)
				_, _ = c.Get(tn, fmt.Sprintf("k%d", i%20))
			}
		}(w)
	}
	wg.Wait() // -race で回して壊れないこと
}
