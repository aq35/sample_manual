package lease_test

// §2.8「マルチコンテナで、1テナント1担当が保証されているか」を実際に確かめる。
// これが無いと同じワーカーが2重に動き、接続も書き込みも倍になる（普段は動いてしまう）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/store"
)

const tenant = model.TenantID("t-lease")

func TestLease_同時に取りに行っても担当は1つ(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	mysqltest.Truncate(t, s.DB(), string(tenant))
	m := lease.NewManager(s.DB(), 3*time.Second)
	ctx := context.Background()

	const contenders = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		owner := "container-" + string(rune('A'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, ok, err := m.Acquire(ctx, tenant, owner)
			if err != nil {
				t.Errorf("%s: %v", owner, err)
				return
			}
			if ok {
				mu.Lock()
				winners = append(winners, l.Owner)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("担当が %d 個できた（1つでなければ二重稼働する）: %v", len(winners), winners)
	}
	t.Logf("%d コンテナが同時に取りに行って、担当は %q の1つだけ", contenders, winners[0])
}

func TestLease_期限が切れたら別のコンテナが拾う(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	mysqltest.Truncate(t, s.DB(), string(tenant))
	ttl := 1500 * time.Millisecond
	m := lease.NewManager(s.DB(), ttl)
	ctx := context.Background()

	l1, ok, err := m.Acquire(ctx, tenant, "container-A")
	if err != nil || !ok {
		t.Fatalf("最初の取得に失敗: %v %v", ok, err)
	}

	// 期限内は他のコンテナは取れない
	if _, ok, err := m.Acquire(ctx, tenant, "container-B"); err != nil || ok {
		t.Fatalf("期限内なのに横取りできてしまった: %v %v", ok, err)
	}

	// 持ち主は延長できる
	if renewed, err := m.Renew(ctx, tenant, "container-A"); err != nil || !renewed {
		t.Fatalf("延長できない: %v %v", renewed, err)
	}
	// 持っていないコンテナは延長できない
	if renewed, err := m.Renew(ctx, tenant, "container-B"); err != nil || renewed {
		t.Fatalf("持ち主でないのに延長できた: %v %v", renewed, err)
	}

	// A が落ちた（延長が止まった）→ 期限切れ後に B が拾う
	time.Sleep(ttl + 300*time.Millisecond)
	l2, ok, err := m.Acquire(ctx, tenant, "container-B")
	if err != nil || !ok {
		t.Fatalf("期限切れ後に拾えない: %v %v", ok, err)
	}
	if l2.Fence <= l1.Fence {
		t.Fatalf("担当が変わったのに fence が増えていない: %d → %d", l1.Fence, l2.Fence)
	}
	t.Logf("A(fence=%d) が落ちた → %v 後に B(fence=%d) が担当を引き継いだ", l1.Fence, ttl, l2.Fence)
}

func TestLease_明け渡すと即座に次が取れる(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	mysqltest.Truncate(t, s.DB(), string(tenant))
	m := lease.NewManager(s.DB(), 30*time.Second)
	ctx := context.Background()

	if _, ok, err := m.Acquire(ctx, tenant, "container-A"); err != nil || !ok {
		t.Fatalf("取得できない: %v %v", ok, err)
	}
	if err := m.Release(ctx, tenant, "container-A"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.Acquire(ctx, tenant, "container-B"); err != nil || !ok {
		t.Fatalf("明け渡し後に取れない: %v %v", ok, err)
	}
}
