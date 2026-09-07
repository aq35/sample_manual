package ssehub_test

// EXP-38: 同一テナントで 300人が SSE 購読するとき、アプリ側でできる対応。
//
//	MYSQL_DSN=... go test ./internal/ssehub/ -run TestEXP38 -v
//
// 300 接続が各自 DB をポーリングすると、同じテナントのデータに 300 倍の読み（ファンイン）。
// (1) テナントに1つの poller＋hub で全員に配る（300→1）。(2) 一斉接続の初期取得は singleflight で
// 1回に畳む。(3) 遅い購読者は他をブロックしない（有界バッファで落とす）。

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/sync/singleflight"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/ssehub"
)

func TestEXP38_SSEファンイン(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	const tenant = "sse-main"
	if err := ssehub.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := ssehub.Seed(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-38", "sse-fan-in",
		"同一テナントで多数が SSE 購読するときの DB ファンインとアプリ側の対応")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 300 接続が各自ポーリングすると、更新1回につき DB 読みが 300 回（ファンイン）。 " +
			"2) テナントに1つの poller が1回引き hub で配ると、更新1回につき DB 読みは 1 回。 " +
			"3) 一斉接続の初期スナップショットは singleflight で畳める（同時 in-flight を1つに。300 同時→ごく少数回）。 " +
			"4) 遅い購読者は有界バッファで落とし、他 299 人の配信をブロックしない。")

	const subscribers, updates = 300, 10

	// ---- ① 素朴: 各接続が毎更新ごとに DB をポーリング ----
	var naiveHits int64
	for u := 0; u < updates; u++ {
		for s := 0; s < subscribers; s++ {
			if _, err := ssehub.Load(ctx, db, tenant, &naiveHits); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec.Add(expkit.Variant{
		Name:     "素朴: 300 接続が各自ポーリング（更新10回）",
		Accident: true,
		Counters: map[string]int64{"db_reads": naiveHits},
		Notes:    []string{"更新10回 × 300接続 = " + itoa64(naiveHits) + " 回の DB 読み（同じテナントに集中）"},
	})
	t.Logf("素朴: db_reads=%d", naiveHits)

	// ---- ② hub: テナントに1つの poller が引き、全員へ配る ----
	hub := ssehub.NewHub(16)
	chans := make([]<-chan int64, subscribers)
	for s := 0; s < subscribers; s++ {
		_, ch := hub.Subscribe()
		chans[s] = ch
	}
	var hubHits int64
	for u := 0; u < updates; u++ {
		if err := ssehub.Bump(ctx, db, tenant); err != nil {
			t.Fatal(err)
		}
		v, err := ssehub.Load(ctx, db, tenant, &hubHits) // 1回だけ引く
		if err != nil {
			t.Fatal(err)
		}
		hub.Broadcast(v)
	}
	// 全購読者が更新を受け取れている（バッファ 16 ≥ updates 10 なので全部届く）
	minReceived := updates
	for s := 0; s < subscribers; s++ {
		got := drain(chans[s])
		if got < minReceived {
			minReceived = got
		}
	}
	rec.Add(expkit.Variant{
		Name:     "hub: テナントに1つの poller＋fan-out（更新10回）",
		Counters: map[string]int64{"db_reads": hubHits, "min_received": int64(minReceived)},
		Notes:    []string{"素朴 " + itoa64(naiveHits) + " 回 → hub " + itoa64(hubHits) + " 回（更新回数と同じ）。全員が受信"},
	})
	t.Logf("hub: db_reads=%d min_received=%d", hubHits, minReceived)

	// ---- ③ stampede: 300 が一斉に初期スナップショットを要求 → singleflight で1回 ----
	var sfHits int64
	sf := &singleflight.Group{}
	var wg sync.WaitGroup
	for s := 0; s < subscribers; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ssehub.Snapshot(ctx, sf, db, tenant, &sfHits)
		}()
	}
	wg.Wait()
	rec.Add(expkit.Variant{
		Name:     "stampede: 300 が一斉に初期取得 → singleflight でごく少数の DB 読みに畳む",
		Counters: map[string]int64{"db_reads": atomic.LoadInt64(&sfHits)},
		Notes:    []string{"300 同時要求が " + itoa64(atomic.LoadInt64(&sfHits)) + " 回の DB 読みに畳まれた（in-flight が重なったぶんを1つに）"},
	})
	t.Logf("stampede: db_reads=%d", sfHits)

	// ---- ④ 遅い購読者: 有界バッファで落とし、他をブロックしない ----
	// 健全な購読者は各配信の直後に読む（追いつく）。遅い購読者は読まない。
	hub2 := ssehub.NewHub(4) // 小さいバッファ
	_, fast := hub2.Subscribe()
	hub2.Subscribe() // 遅い購読者（読まない）
	const many = 100
	fastGot := 0
	for i := 0; i < many; i++ {
		hub2.Broadcast(int64(i)) // 遅い方が詰まっても Broadcast は即返る（非ブロッキング）
		select { // 健全な購読者はすぐ引き取る → 取りこぼさない
		case <-fast:
			fastGot++
		default:
		}
	}
	rec.Add(expkit.Variant{
		Name:     "遅い購読者: 有界バッファで落とし、速い購読者は全部受け取る",
		Counters: map[string]int64{"fast_received": int64(fastGot), "dropped_total": hub2.Dropped()},
		Notes:    []string{"速い購読者は " + itoa64(int64(fastGot)) + "/100 受信、遅い方は落とされた（" + itoa64(hub2.Dropped()) + " 件）。全体は止まらない"},
	})
	t.Logf("slow-consumer: fast=%d dropped=%d", fastGot, hub2.Dropped())

	// ---- ⑤ hub の性能上限: fan-out(チャネル配信)のコストを購読者数別に ----
	// これは in-memory の配信コスト（実 SSE の「各ソケットへの書き込み」は別＝より重い。ここは下限）。
	for _, n := range []int{300, 1000, 5000} {
		h := ssehub.NewHub(1)
		for i := 0; i < n; i++ {
			h.Subscribe() // 誰も読まない（配信コストだけ測る）
		}
		h.Broadcast(0) // warm
		const B = 3000
		t0 := time.Now()
		for b := 0; b < B; b++ {
			h.Broadcast(int64(b))
		}
		perBroadcastNs := float64(time.Since(t0).Nanoseconds()) / float64(B)
		broadcastsPerSec := 1e9 / perBroadcastNs
		rec.Add(expkit.Variant{
			Name: "hub 上限: fan-out コスト（購読者 " + itoa64(int64(n)) + "・in-memory）",
			Metrics: map[string]float64{
				"per_broadcast_us":   perBroadcastNs / 1000,
				"per_sub_ns":         perBroadcastNs / float64(n),
				"broadcasts_per_sec": broadcastsPerSec,
			},
			Notes: []string{"1配信 " + f1(perBroadcastNs/1000) + "µs（1購読者あたり " + f1(perBroadcastNs/float64(n)) + "ns）"},
		})
		t.Logf("hub上限 n=%d: per_broadcast=%.1fµs per_sub=%.1fns broadcasts/s=%.0f",
			n, perBroadcastNs/1000, perBroadcastNs/float64(n), broadcastsPerSec)
	}

	// ---- 検証 ----
	if naiveHits != subscribers*updates {
		t.Errorf("素朴の DB 読みが想定と違う: %d（%d のはず）", naiveHits, subscribers*updates)
	}
	if hubHits != updates {
		t.Errorf("hub の DB 読みが更新回数と違う: %d（%d のはず）", hubHits, updates)
	}
	if minReceived != updates {
		t.Errorf("hub で受信漏れがある: min=%d（%d のはず）", minReceived, updates)
	}
	if h := atomic.LoadInt64(&sfHits); h < 1 || h >= int64(subscribers)/10 {
		t.Errorf("singleflight の畳み込みが弱い: %d（300同時が桁違いに減るはず）", h)
	}
	if fastGot != many {
		t.Errorf("遅い購読者に引きずられ速い購読者が取りこぼした: %d", fastGot)
	}
	if hub2.Dropped() == 0 {
		t.Errorf("遅い購読者ぶんが落とされていない（有界バッファのはず）")
	}

	rec.Scope(
		"MySQL 8.0 / 同一テナント 300 購読者・更新10回 / hub は in-memory fan-out・有界バッファ",
		"DB 読み回数を数える（Load を呼ぶたび +1）。hub は poller 1本ぶんだけ引く",
		"singleflight は golang.org/x/sync。stampede は 300 goroutine 同時要求で再現",
	)
	rec.Uncertain(
		"実運用の SSE は接続ごとに goroutine とバッファを持つ（メモリは EXP-31）。ここは DB ファンインに集中",
		"更新の押し出し方（毎回 push か coalesce か）は頻度次第。高頻度なら間引き/最新版のみ配る",
		"複数プロセスに購読者が分かれると、各プロセスに poller が要る（プロセス数ぶんの DB 読み）。" +
			"それでも接続数ではなくプロセス数で頭打ち。プロセス跨ぎの通知は pub/sub が要る（MySQL に LISTEN/NOTIFY は無い）",
		"落とした更新は『最新版がまた来る』前提の設計（差分でなく版/スナップショット配信）",
	)
	rec.Artifact(
		"internal/ssehub: テナント単位 poller＋fan-out hub・singleflight・有界バッファ",
		"docs/sse-fan-in.md: 多数購読時の DB ファンイン対策",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"同一テナントで多数が購読するとき、接続ごとに DB を引かせない。テナントに1つの poller が1回引き、" +
			"in-memory hub で全員へ配る（300→1）。一斉接続の初期取得は singleflight で1回に畳む。" +
			"遅い購読者は有界バッファで落として全体を止めない（最新版はまた来るので、差分でなく版/" +
			"スナップショットを配る設計にする）。SSE 接続に DB 接続を1:1で持たせない（EXP-31）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func drain(ch <-chan int64) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

func itoa64(n int64) string {
	if n < 0 {
		return "-" + itoa64(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa64(n/10) + string(rune('0'+n%10))
}

func f1(v float64) string {
	i := int64(v * 10)
	return itoa64(i/10) + "." + itoa64(i%10)
}
