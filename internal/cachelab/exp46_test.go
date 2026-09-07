package cachelab_test

// EXP-46: キャッシュの無効化戦略（TTL / イベント失効 / stampede）。
//
//	go test ./internal/cachelab/ -run TestEXP46 -v   （DB 不要）
//
// 読みをキャッシュすると DB 負荷は減るが、古い値(stale)を返す危険が出る。無キャッシュ・TTL・
// イベント失効で「DB 読み回数」と「stale を返した回数」を論理時計で測り、trade-off を見る。
// さらに、期限切れ直後の一斉読み(stampede)を singleflight で1回に畳めることを確かめる。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"golang.org/x/sync/singleflight"
)

func TestEXP46_キャッシュ無効化(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-46", "cache-invalidation",
		"無キャッシュ / TTL / イベント失効の DB 負荷と stale の trade-off")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) 無キャッシュ: 毎回 DB を読む（DB 読み=読み回数）が stale は 0。 " +
			"2) TTL: DB 読みは 読み回数/ttl に減るが、書き込み後 ttl まで stale を返しうる。 " +
			"3) イベント失効: 書き込みで無効化 → 次の読みで1回引き直す。DB 読みは少なく stale は 0。 " +
			"4) 期限切れ直後の一斉読みは singleflight で1回の DB 読みに畳める（stampede 防止）。")

	// 論理時計（step）で進める。truth は書き込みで増える版。
	const steps = 100
	const ttl = 10
	writeAt := map[int]bool{20: true, 55: true, 80: true} // この step で書き込み（truth++）

	// --- ① 無キャッシュ ---
	truth := 0
	noCacheReads, noCacheStale := 0, 0
	for s := 0; s < steps; s++ {
		if writeAt[s] {
			truth++
		}
		got := truth // 毎回 DB を読む
		noCacheReads++
		if got != truth {
			noCacheStale++
		}
	}

	// --- ② TTL ---
	truth = 0
	ttlReads, ttlStale := 0, 0
	cachedVal, loadedAt := 0, -ttl-1
	for s := 0; s < steps; s++ {
		if writeAt[s] {
			truth++
		}
		if s-loadedAt >= ttl { // 期限切れ → 引き直す
			cachedVal = truth
			loadedAt = s
			ttlReads++
		}
		if cachedVal != truth { // 返した値が今の truth と違う＝stale
			ttlStale++
		}
	}

	// --- ③ イベント失効 ---
	truth = 0
	evReads, evStale := 0, 0
	valid := false
	evVal := 0
	for s := 0; s < steps; s++ {
		if writeAt[s] {
			truth++
			valid = false // 書き込みで無効化
		}
		if !valid { // 無効なら引き直す
			evVal = truth
			valid = true
			evReads++
		}
		if evVal != truth {
			evStale++
		}
	}

	rec.Add(expkit.Variant{
		Name:     "無キャッシュ: DB 読み=読み回数・stale=0",
		Accident: true,
		Counters: map[string]int64{"db_reads": int64(noCacheReads), "stale_reads": int64(noCacheStale)},
	})
	rec.Add(expkit.Variant{
		Name:     "TTL(10): DB 読み激減・ただし stale を返しうる",
		Counters: map[string]int64{"db_reads": int64(ttlReads), "stale_reads": int64(ttlStale)},
		Notes:    []string{"DB 読み " + itoa(noCacheReads) + "→" + itoa(ttlReads) + " / stale " + itoa(ttlStale) + " 回（書込後 ttl まで古い）"},
	})
	rec.Add(expkit.Variant{
		Name:     "イベント失効: DB 読み少なく stale=0",
		Counters: map[string]int64{"db_reads": int64(evReads), "stale_reads": int64(evStale)},
		Notes:    []string{"書込で無効化 → 次読みで1回引く。DB 読み " + itoa(evReads) + "・stale " + itoa(evStale)},
	})
	t.Logf("reads: nocache=%d ttl=%d event=%d / stale: ttl=%d event=%d",
		noCacheReads, ttlReads, evReads, ttlStale, evStale)

	// --- ④ stampede: 期限切れ直後に300が一斉に読む → singleflight で1回 ---
	// ★計測の落とし穴: load が即返ると、次の goroutine が呼ぶ頃には前の呼び出しが終わっていて
	//   「in-flight」に重ならず畳めない（素朴に書くと 300→299 のような結果になり嘘になる）。
	//   本物の一斉読みを再現する: 全 goroutine が到着してから一斉に解き放ち、load は少し待つ。
	var dbHits int64
	sf := &singleflight.Group{}
	load := func() (any, error) {
		atomic.AddInt64(&dbHits, 1)
		time.Sleep(30 * time.Millisecond) // DB 往復ぶん。この間に後続が in-flight に合流する
		return 42, nil
	}
	const stampeders = 300
	var arrived, wg sync.WaitGroup
	arrived.Add(stampeders)
	gate := make(chan struct{})
	for i := 0; i < stampeders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrived.Done() // 到着を通知
			<-gate         // 全員そろうまで待つ
			_, _, _ = sf.Do("key", load)
		}()
	}
	arrived.Wait() // 300 個そろった
	close(gate)    // 一斉に解き放つ
	wg.Wait()
	stampedeHits := atomic.LoadInt64(&dbHits)
	rec.Add(expkit.Variant{
		Name:     "stampede: 期限切れ一斉読み300 → singleflight で DB 読みごく少数",
		Counters: map[string]int64{"db_reads": stampedeHits, "stampeders": stampeders},
		Notes:    []string{"300 が一斉に読んでも DB 読みは " + itoa(int(stampedeHits)) + " 回（in-flight に合流）"},
	})
	t.Logf("stampede: db_hits=%d", dbHits)

	// ---- 検証 ----
	if noCacheReads != steps || noCacheStale != 0 {
		t.Errorf("無キャッシュ: reads=%d stale=%d", noCacheReads, noCacheStale)
	}
	if ttlReads >= noCacheReads {
		t.Errorf("TTL で DB 読みが減っていない: %d", ttlReads)
	}
	if ttlStale == 0 {
		t.Errorf("TTL で stale が出ていない（trade-off のはず）: %d", ttlStale)
	}
	if evStale != 0 {
		t.Errorf("イベント失効で stale が出た（0 のはず）: %d", evStale)
	}
	if evReads >= noCacheReads {
		t.Errorf("イベント失効で DB 読みが減っていない: %d", evReads)
	}
	if stampedeHits > stampeders/10 { // 300 → せいぜい数回に畳めているはず
		t.Errorf("stampede が畳めていない: %d（<=%d のはず）", stampedeHits, stampeders/10)
	}

	rec.Scope(
		"純 Go（DB 不要）/ 論理時計 100 step・TTL=10・書込 3 回 / stampede は 300 goroutine",
		"stale = 返した値が『その時点の truth』と違う回数。DB 読み = 引き直した回数",
		"singleflight は golang.org/x/sync（EXP-38 と同じ）",
	)
	rec.Uncertain(
		"実際の stale 許容はデータ次第（残高は不可・表示名は数秒可 等）。ここは回数の trade-off を示す",
		"イベント失効は『書込を確実に無効化に届ける』のが前提（同プロセスなら簡単、跨ぐなら pub/sub・EXP-43）",
		"TTL とイベント失効は併用できる（短めTTL＋失効で上限保証）。ここは各単体",
	)
	rec.Artifact("docs/cache.md: キャッシュ無効化（TTL / イベント失効 / stampede）")
	rec.Next("EXP-47 順序・冪等消費")

	files, err := rec.Save(
		"キャッシュは DB 負荷を下げるが stale を生む。無キャッシュ=常に新鮮だが毎回 DB、TTL=DB 激減だが" +
			"書込後 ttl まで古い、イベント失効=書込で無効化して新鮮かつ DB 少。データの stale 許容で選ぶ" +
			"（許さないならイベント失効＋短めTTL）。期限切れ一斉読みは singleflight で1回に畳む。")
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
