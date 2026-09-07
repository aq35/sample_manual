package ssehub_test

// EXP-42: hub はテナントごとの分離をちゃんと実現できているか。
//
//	go test ./internal/ssehub/ -run TestEXP42 -v   （DB 不要）
//
// テナント A と B に購読者を張り、A の版だけ動かす。A の購読者だけが受け取り、B には一切
// 流れないこと（逆も）を確かめる。値の範囲でテナントを区別し、混線を検出する。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/ssehub"
)

func TestEXP42_hubのテナント分離(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-42", "hub-tenant-isolation",
		"hub がテナントごとに分離できているか（A の購読者に B のイベントが漏れない）")
	rec.Env(expkit.CaptureEnv(ctx, nil)) // DB 不要
	rec.Freeze(
		"1) テナントごとに別 hub＋別 poller。A の版が変わっても A の購読者にだけ流れ、B には流れない。 " +
			"2) B の版が変わっても B にだけ。値の範囲(A=100番台/B=100万番台)で混線を検出する。 " +
			"3) 全購読者が抜けたテナントの poller だけが止まる（他テナントに影響しない）。")

	// テナントごとに版を持つ in-memory ローダ。値の範囲でテナントを区別できるようにする。
	verA := int64(100)       // A の版は 100番台
	verB := int64(1_000_000) // B の版は 100万番台
	var mu sync.Mutex
	load := func(ctx context.Context, tenant string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		switch tenant {
		case "A":
			return verA, nil
		case "B":
			return verB, nil
		}
		return 0, nil
	}
	bump := func(tenant string) {
		mu.Lock()
		defer mu.Unlock()
		if tenant == "A" {
			verA++
		} else {
			verB++
		}
	}

	reg := ssehub.NewRegistry(load, 20*time.Millisecond, 64)

	// A に2人・B に2人。受信を集める。
	type collector struct {
		ch      <-chan int64
		release func()
		mu      sync.Mutex
		got     []int64
	}
	newCol := func(tenant string) *collector {
		ch, _, release := reg.Subscribe(tenant)
		return &collector{ch: ch, release: release}
	}
	cols := map[string][]*collector{
		"A": {newCol("A"), newCol("A")},
		"B": {newCol("B"), newCol("B")},
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, list := range cols {
		for _, c := range list {
			wg.Add(1)
			go func(c *collector) {
				defer wg.Done()
				for {
					select {
					case v := <-c.ch:
						c.mu.Lock()
						c.got = append(c.got, v)
						c.mu.Unlock()
					case <-stop:
						return
					}
				}
			}(c)
		}
	}

	time.Sleep(60 * time.Millisecond) // 初期配信を待つ
	activeWhile := reg.ActiveTenants()

	// A の版だけ3回動かす
	for i := 0; i < 3; i++ {
		bump("A")
		time.Sleep(40 * time.Millisecond)
	}
	// B の版を2回動かす
	for i := 0; i < 2; i++ {
		bump("B")
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)

	close(stop)
	wg.Wait()

	// 受信を分類（A範囲=100〜999, B範囲=100万以上）
	inA := func(v int64) bool { return v >= 100 && v < 1000 }
	inB := func(v int64) bool { return v >= 1_000_000 }
	leaks := 0
	aGotUpdates, bGotUpdates := false, false
	for tenant, list := range cols {
		for _, c := range list {
			c.mu.Lock()
			for _, v := range c.got {
				switch tenant {
				case "A":
					if inB(v) {
						leaks++ // A の購読者が B の値を受け取った＝漏れ
					}
					if v > 100 { // 初期100より進んだ＝更新を受けた
						aGotUpdates = true
					}
				case "B":
					if inA(v) {
						leaks++ // B の購読者が A の値を受け取った＝漏れ
					}
					if v > 1_000_000 {
						bGotUpdates = true
					}
				}
			}
			c.mu.Unlock()
		}
	}

	// 全 A 購読者を解放 → A の poller だけ止まる。B は残る。
	for _, c := range cols["A"] {
		c.release()
	}
	time.Sleep(80 * time.Millisecond)
	activeAfterA := reg.ActiveTenants()
	for _, c := range cols["B"] {
		c.release()
	}
	time.Sleep(80 * time.Millisecond)
	activeAfterAll := reg.ActiveTenants()

	rec.Add(expkit.Variant{
		Name:     "テナント分離: A の更新は A だけ・B の更新は B だけ・漏れゼロ",
		Counters: map[string]int64{"cross_leaks": int64(leaks), "a_got_updates": b2iX(aGotUpdates), "b_got_updates": b2iX(bGotUpdates), "active_while": int64(activeWhile)},
		Notes:    []string{"A範囲=100番台/B範囲=100万番台。A購読者にB値・B購読者にA値が来た回数=" + itoa64(int64(leaks))},
	})
	rec.Add(expkit.Variant{
		Name:     "解放: A 全員が抜けても B の poller は残る（テナント独立）",
		Counters: map[string]int64{"active_after_A_released": int64(activeAfterA), "active_after_all": int64(activeAfterAll)},
		Notes:    []string{"アクティブ: 購読中 " + itoa64(int64(activeWhile)) + " → A解放後 " + itoa64(int64(activeAfterA)) + " → 全解放後 " + itoa64(int64(activeAfterAll))},
	})
	t.Logf("isolation: leaks=%d aGot=%v bGot=%v active %d→(Aのみ解放)%d→(全解放)%d",
		leaks, aGotUpdates, bGotUpdates, activeWhile, activeAfterA, activeAfterAll)

	// ---- 検証 ----
	if leaks != 0 {
		t.Errorf("テナント混線が起きた（漏れ %d 件）", leaks)
	}
	if !aGotUpdates || !bGotUpdates {
		t.Errorf("自テナントの更新を受け取れていない: A=%v B=%v", aGotUpdates, bGotUpdates)
	}
	if activeWhile != 2 {
		t.Errorf("購読中のアクティブテナントが2でない: %d", activeWhile)
	}
	if activeAfterA != 1 {
		t.Errorf("A 解放後、B が残っていない（テナント独立でない）: %d", activeAfterA)
	}
	if activeAfterAll != 0 {
		t.Errorf("全解放後もテナントが残っている: %d", activeAfterAll)
	}

	rec.Scope(
		"純 Go（DB 不要）/ A・B 各2購読者 / 値の範囲でテナントを区別（A=100番台, B=100万番台）",
		"分離の構造: テナントごとに別 hub（別 map エントリ）＋別 poller。Broadcast は自 hub の購読者だけ",
		"poller の load はテナント引数で自テナントだけ読む（IN の担当限定と同じ原理・EXP-14）",
	)
	rec.Uncertain(
		"ここは hub 層の配信分離。テナントの出所（認証済み ctx から取る）は resolver/handler の責務（EXP-24/41）",
		"singleflight も tenant キーで畳むので跨がない（EXP-38）",
		"複数プロセスに分けると各プロセスに hub がある。pub/sub のトピックもテナントで分ける必要（EXP-38）",
	)
	rec.Artifact(
		"internal/ssehub: テナントごとに独立した hub＋poller（Registry）",
		"docs/sse-fan-in.md / docs/security-layers.md: hub のテナント分離",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"hub はテナントごとに別 hub＋別 poller で分離する。Broadcast は自テナントの購読者にだけ届き、" +
			"A の購読者に B のイベントは漏れない（実測で混線ゼロ）。poller の load も自テナントだけ読む。" +
			"あるテナントの購読者が全員抜けても、その poller だけが止まり他テナントには影響しない。" +
			"テナントの出所を認証済み ctx から取る（EXP-24/41）ことと合わせて、分離は構造で担保される。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func b2iX(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
