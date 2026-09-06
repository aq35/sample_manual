package poolbudget_test

import (
	"testing"

	"github.com/aq35/sample_manual/internal/poolbudget"
)

func TestBudget_直結の飽和(t *testing.T) {
	// max_connections=151（MySQL 既定）、予約 20、1コンテナ 20 本。
	p := poolbudget.Plan{DBMaxConnections: 151, Reserved: 20, Containers: 6, PerContainer: 20}
	if p.Budget() != 131 {
		t.Fatalf("Budget=%d want 131", p.Budget())
	}
	if p.Demand() != 120 {
		t.Fatalf("Demand=%d want 120", p.Demand())
	}
	if !p.Fits() {
		t.Errorf("6 コンテナは収まるはず: %s", p.Report())
	}
	// 7 コンテナで超える
	p.Containers = 7
	if p.Fits() {
		t.Errorf("7 コンテナ(140) は予算 131 を超えるはず")
	}
	if mc := p.MaxContainers(); mc != 6 {
		t.Errorf("MaxContainers=%d want 6", mc)
	}
	t.Logf("\n%s", p.Report())
}

func TestBudget_レプリカ読みを足すと早く尽きる(t *testing.T) {
	p := poolbudget.Plan{DBMaxConnections: 151, Reserved: 20, Containers: 4, PerContainer: 20, ReplicaPer: 10}
	// 4 × (20+10) = 120 ≤ 131 → 収まる。5 コンテナ(150)で超える。
	if !p.Fits() {
		t.Errorf("4 コンテナは収まるはず")
	}
	if mc := p.MaxContainers(); mc != 4 {
		t.Errorf("MaxContainers=%d want 4（131/30）", mc)
	}
}

func TestBudget_Proxyはコンテナ数で頭打ちにならない(t *testing.T) {
	// 直結なら 50 コンテナ×20 = 1000 本で即枯渇。Proxy backend 100 本なら収まる。
	direct := poolbudget.Plan{DBMaxConnections: 500, Reserved: 50, Containers: 50, PerContainer: 20}
	if direct.Fits() {
		t.Errorf("直結 50×20=1000 は予算 450 を超えるはず")
	}
	proxied := direct
	proxied.ProxyBackend = 100
	if !proxied.Fits() {
		t.Errorf("Proxy backend 100 は予算 450 に収まるはず")
	}
	if proxied.Demand() != 100 {
		t.Errorf("Proxy Demand=%d want 100（コンテナ数に依らない）", proxied.Demand())
	}
	if mc := proxied.MaxContainers(); mc != -1 {
		t.Errorf("Proxy はコンテナ数で頭打ちにならない（-1）: %d", mc)
	}
	t.Logf("\n直結:\n%s\nProxy:\n%s", direct.Report(), proxied.Report())
}

func TestRecommendPerContainer(t *testing.T) {
	// 予算 131、目標 10 コンテナ、20% 余白 → usable 104、per = 10 本
	per := poolbudget.RecommendPerContainer(131, 10, 0.2)
	if per != 10 {
		t.Errorf("RecommendPerContainer=%d want 10", per)
	}
	// 目標 200 コンテナは、余白を確保すると 1 本も張れない → 0（Proxy が要る合図）
	if per := poolbudget.RecommendPerContainer(131, 200, 0.2); per != 0 {
		t.Errorf("200 コンテナは直結不可（0 を返すべき）: %d", per)
	}
	t.Logf("10 コンテナに収める per-container = %d 本", per)
}
