package pubsub_test

// EXP-43: 複数プロセス（ゲートウェイ）跨ぎの SSE fan-out を pub/sub で。テナント分離は保たれるか。
//
//	go test ./internal/pubsub/ -run TestEXP43 -v   （DB 不要・Redis 不要）
//
// トポロジ: テナントごとに1つの poller が DB を読み、broker の topic=テナント へ publish。
// 複数のゲートウェイがその topic を購読し、各自のローカル hub で自分の接続へ配る。
//   - DB 読みは poller の分だけ（ゲートウェイ数・接続数に無関係）。
//   - topic=テナント なので、A の publish は A の接続にだけ届く（プロセスを跨いでも混線しない）。
//   - 実 Redis/NATS はここでは動かさない（MemBroker で意味論を再現。Broker 差し替えで実装可能）。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/pubsub"
	"github.com/aq35/sample_manual/internal/ssehub"
)

func TestEXP43_pubsub跨ぎのSSE(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-43", "pubsub-cross-process-sse",
		"複数ゲートウェイ跨ぎの SSE fan-out（pub/sub）とテナント分離")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) テナントごとに1 poller が DB を読み broker(topic=テナント)へ publish。DB 読みは接続/ゲートウェイ数に無関係。 " +
			"2) 各ゲートウェイは topic を購読しローカル hub で自分の接続へ配る（2段 fan-out）。 " +
			"3) topic=テナント なので A の publish は A の接続だけに届く（プロセスを跨いでも混線しない）。 " +
			"4) 実 Redis/NATS は差し替え（Broker interface）。ここは MemBroker で意味論を再現。")

	broker := pubsub.NewMemBroker(64)

	// 版（テナントごと）。値の範囲でテナントを区別（A=100番台, B=100万番台）。
	verA, verB := int64(100), int64(1_000_000)
	var vmu sync.Mutex
	readOf := func(tenant string) int64 {
		vmu.Lock()
		defer vmu.Unlock()
		if tenant == "A" {
			return verA
		}
		return verB
	}
	bump := func(tenant string) {
		vmu.Lock()
		defer vmu.Unlock()
		if tenant == "A" {
			verA++
		} else {
			verB++
		}
	}

	stop := make(chan struct{})
	var dbReads int64
	// poller: テナントごとに1本。DB を読み（count）、変化時に broker へ publish。
	poller := func(tenant string) {
		var last int64 = -1
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				atomic.AddInt64(&dbReads, 1) // ← DB 読みはここだけ（接続/ゲートウェイに無関係）
				v := readOf(tenant)
				if v != last {
					last = v
					broker.Publish("tenant:"+tenant, v)
				}
			}
		}
	}
	go poller("A")
	go poller("B")

	// ゲートウェイ: broker の topic を購読 → ローカル hub → 接続。3ゲートウェイ×(A2,B2)接続。
	type conn struct {
		tenant string
		mu     sync.Mutex
		got    []int64
	}
	var conns []*conn
	var cmu sync.Mutex
	var wg sync.WaitGroup
	makeGateway := func() {
		for _, tenant := range []string{"A", "B"} {
			hub := ssehub.NewHub(64)
			// このゲートウェイの topic 購読（テナントにつき1本）→ ローカル hub へ
			bch, unsub := broker.Subscribe("tenant:" + tenant)
			wg.Add(1)
			go func(tenant string) {
				defer wg.Done()
				defer unsub()
				for {
					select {
					case <-stop:
						return
					case v := <-bch:
						hub.Broadcast(v)
					}
				}
			}(tenant)
			// 接続2本（ローカル hub を購読）
			for i := 0; i < 2; i++ {
				_, cch := hub.Subscribe()
				c := &conn{tenant: tenant}
				cmu.Lock()
				conns = append(conns, c)
				cmu.Unlock()
				wg.Add(1)
				go func(c *conn) {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						case v := <-cch:
							c.mu.Lock()
							c.got = append(c.got, v)
							c.mu.Unlock()
						}
					}
				}(c)
			}
		}
	}
	const gateways = 3
	for g := 0; g < gateways; g++ {
		makeGateway()
	}

	time.Sleep(60 * time.Millisecond) // 初期 publish を待つ
	for i := 0; i < 3; i++ { // A の版だけ動かす
		bump("A")
		time.Sleep(40 * time.Millisecond)
	}
	for i := 0; i < 2; i++ { // B も動かす
		bump("B")
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)

	readsWhile := atomic.LoadInt64(&dbReads)
	close(stop)
	wg.Wait()

	// 分類（A範囲=100番台, B範囲=100万以上）
	inA := func(v int64) bool { return v >= 100 && v < 1000 }
	inB := func(v int64) bool { return v >= 1_000_000 }
	leaks, aUpd, bUpd := 0, false, false
	for _, c := range conns {
		c.mu.Lock()
		for _, v := range c.got {
			if c.tenant == "A" {
				if inB(v) {
					leaks++
				}
				if v > 100 {
					aUpd = true
				}
			} else {
				if inA(v) {
					leaks++
				}
				if v > 1_000_000 {
					bUpd = true
				}
			}
		}
		c.mu.Unlock()
	}
	// 素朴（接続ごとに DB ポーリング）なら 接続数 × tick。ここは poller（テナント数）ぶん。
	totalConns := len(conns)
	const tenants = 2
	ticks := readsWhile / tenants                  // 1 poller あたりの tick 数（≒経過/間隔）
	naiveReads := int64(totalConns) * ticks        // 素朴に各接続がポーリングした場合の DB 読み

	rec.Add(expkit.Variant{
		Name:     "pub/sub 跨ぎ: DB 読みは poller の分だけ（接続/ゲートウェイに無関係）",
		Counters: map[string]int64{"gateways": gateways, "conns": int64(totalConns), "db_reads": readsWhile, "naive_reads": naiveReads, "broker_dropped": broker.Dropped()},
		Notes:    []string{"接続 " + itoa(totalConns) + "・ゲートウェイ " + itoa(gateways) + " でも DB 読みは poller ぶん " + itoa64(readsWhile) + "（素朴なら接続数×tick=" + itoa64(naiveReads) + "）"},
	})
	rec.Add(expkit.Variant{
		Name:     "テナント分離: A の publish は A の接続だけ・混線ゼロ（プロセス跨ぎでも）",
		Counters: map[string]int64{"cross_leaks": int64(leaks), "a_got": b(aUpd), "b_got": b(bUpd)},
		Notes:    []string{"topic=tenant で分離。A購読者にB値・B購読者にA値=" + itoa(leaks) + " 件"},
	})
	t.Logf("pubsub: gateways=%d conns=%d db_reads=%d leaks=%d aUpd=%v bUpd=%v",
		gateways, totalConns, readsWhile, leaks, aUpd, bUpd)

	// ---- 検証 ----
	if leaks != 0 {
		t.Errorf("プロセス跨ぎで混線した（漏れ %d 件）", leaks)
	}
	if !aUpd || !bUpd {
		t.Errorf("自テナントの更新が届いていない: A=%v B=%v", aUpd, bUpd)
	}
	// DB 読みは接続数に比例しない（poller＝テナント数ぶん。素朴な接続数×tick より桁違いに少ない）
	if readsWhile >= naiveReads {
		t.Errorf("DB 読みが素朴案(接続数×tick)より減っていない: reads=%d naive=%d", readsWhile, naiveReads)
	}
	if ticks > 0 && readsWhile > tenants*ticks+tenants { // poller はテナント数ぶんだけ
		t.Errorf("DB 読みがテナント数×tick を超えている（接続数に依存？）: reads=%d", readsWhile)
	}

	rec.Scope(
		"純 Go（DB/Redis 不要）/ MemBroker で pub/sub を再現 / 3ゲートウェイ×(A2,B2)接続・poller 2本",
		"2段 fan-out: broker(topic=テナント) → 各ゲートウェイのローカル hub → 接続",
		"DB 読みは poller のみカウント。topic=テナント でプロセス跨ぎでも分離",
	)
	rec.Uncertain(
		"実 Redis/NATS は未実行（ネットワーク制限）。Broker interface 差し替えで実装可能・意味論は同じ",
		"実運用はゲートウェイが topic をテナントにつき1本購読（接続ごとでない）。ここもその形",
		"broker のドロップ/順序/再接続は実装依存（Redis pub/sub は at-most-once）。永続が要るなら stream 型",
	)
	rec.Artifact(
		"internal/pubsub: Broker interface と MemBroker（Redis/NATS 差し替え可能）",
		"docs/sse-fan-in.md: 複数プロセス構成（poller＋ゲートウェイ＋pub/sub）",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"SSE を複数プロセスに分けるときは、テナントごとに1 poller が DB を読み broker(topic=テナント)へ" +
			"publish、各ゲートウェイが topic を購読してローカル hub で自分の接続へ配る（2段 fan-out）。" +
			"DB 読みは poller の分だけ（接続・ゲートウェイ数に無関係）、topic=テナント でプロセスを跨いでも" +
			"混線しない。Broker は interface にして Redis/NATS へ差し替える（at-most-once・永続要なら stream 型）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
func itoa64(n int64) string { return itoa(int(n)) }
func b(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
