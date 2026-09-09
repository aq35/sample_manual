package doorbelllab_test

// EXP-62: worker のイベント駆動 — doorbell は起こす信号、正しさは DB CAS。
//
//	MYSQL_DSN=... go test ./internal/doorbelllab/ -run TestEXP62 -v
//
// ① doorbell＋adaptive backoff は 1秒タイトポーリングよりDB接触が激減（仕事があれば即起床）。
//    ただし floor（poll/reconcile）が要る：ドアベルを1つ落とすと、event だけでは永久に拾えない。
// ② 完了は DB CAS で exactly-once：重複/並行ドアベルで何度起こされても complete_count=1。
//    素朴な read-then-write は二重完了する（事故）。
// ③ crash 回収は event でなく lease 失効の reconcile：担当が死ぬと「イベントが来ない」ので、
//    DB 時計の lease 期限＋sweep でしか拾えない。

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/doorbelllab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

// --- Part ① 純ロジックの poll コスト比較（決定的）---
type simResult struct {
	touches int // DB を見に行った回数
	stuck   int // 拾えなかった仕事の数
	maxLat  int // 仕事到着から拾うまでの最大遅延（論理秒）
}

func simulate(mode string, T int, workAt, doorbellDelivered map[int]bool, cap int) simResult {
	picked := map[int]bool{}
	touches, maxLat := 0, 0
	backoff, nextFloor := 1, 0
	for t := 0; t <= T; t++ {
		wake := false
		floorWake := false
		switch mode {
		case "tight":
			wake = true // 毎秒
		case "doorbell_only":
			wake = doorbellDelivered[t]
		case "doorbell_floor":
			if doorbellDelivered[t] {
				wake = true
				backoff, nextFloor = 1, t+1
			} else if t >= nextFloor {
				wake, floorWake = true, true
			}
		}
		if !wake {
			continue
		}
		touches++
		found := false
		for w := range workAt { // poll は現在の全 pending を見る
			if w <= t && !picked[w] {
				picked[w] = true
				found = true
				if t-w > maxLat {
					maxLat = t - w
				}
			}
		}
		if mode == "doorbell_floor" && floorWake {
			if found {
				backoff = 1
			} else if backoff*2 <= cap {
				backoff *= 2
			} else {
				backoff = cap
			}
			nextFloor = t + backoff
		}
	}
	stuck := 0
	for w := range workAt {
		if !picked[w] {
			stuck++
		}
	}
	return simResult{touches, stuck, maxLat}
}

func TestEXP62_workerイベント駆動(t *testing.T) {
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
	if err := doorbelllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-62", "worker-doorbell-vs-floor",
		"doorbell は起こす信号・正しさは DB CAS。floor(poll/reconcile)の上に event を載せる")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"① doorbell+backoff は tight poll より DB 接触が激減（仕事があれば即起床・遅延0）。 " +
			"ただし event だけ(floor 無し)は、落ちたドアベルの仕事を永久に拾えない（floor が要る）。 " +
			"② 完了は DB CAS で exactly-once：重複/並行ドアベルでも complete_count=1。素朴 read-then-write は二重完了。 " +
			"③ crash 回収は event でなく lease 失効の reconcile：担当が死ぬとイベントは来ない。DB 時計の期限＋sweep で拾う。")

	// ---- ① poll コスト（論理 60 秒・仕事は 2,5,30 に到着・30 のドアベルは欠落）----
	const T = 60
	workAt := map[int]bool{2: true, 5: true, 30: true}
	delivered := map[int]bool{2: true, 5: true} // 30 のドアベルは落ちる
	tight := simulate("tight", T, workAt, delivered, 16)
	dOnly := simulate("doorbell_only", T, workAt, delivered, 16)
	dFloor := simulate("doorbell_floor", T, workAt, delivered, 16)

	rec.Add(expkit.Variant{
		Name:     "tight poll(毎秒): 拾い漏れ0だが DB 接触が多い",
		Counters: map[string]int64{"db_touches": int64(tight.touches), "stuck": int64(tight.stuck), "max_latency_s": int64(tight.maxLat)},
	})
	rec.Add(expkit.Variant{
		Name:     "doorbell のみ(floor なし): ドアベル欠落の仕事を永久に拾えない",
		Accident: true,
		Counters: map[string]int64{"db_touches": int64(dOnly.touches), "stuck": int64(dOnly.stuck), "max_latency_s": int64(dOnly.maxLat)},
		Notes:    []string{"接触は最少だが stuck=" + itoa(dOnly.stuck) + "（落ちたドアベルの仕事が残る）"},
	})
	rec.Add(expkit.Variant{
		Name:     "doorbell + backoff floor: 接触激減・拾い漏れ0",
		Counters: map[string]int64{"db_touches": int64(dFloor.touches), "stuck": int64(dFloor.stuck), "max_latency_s": int64(dFloor.maxLat)},
		Notes:    []string{"tight " + itoa(tight.touches) + "→ " + itoa(dFloor.touches) + " 接触。落ちたドアベルも floor が拾う(最大遅延 " + itoa(dFloor.maxLat) + "s)"},
	})

	// ---- ② 完了 exactly-once（重複/並行ドアベル）----
	const gid = int64(1)
	if err := doorbelllab.Seed(ctx, db, gid, "t1"); err != nil {
		t.Fatal(err)
	}
	const wakes = 50
	var casWon int32
	var wg sync.WaitGroup
	for i := 0; i < wakes; i++ { // 50 個の重複ドアベルが一斉に完了を試みる
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := doorbelllab.CompleteCAS(ctx, db, gid)
			if err == nil && ok {
				atomic.AddInt32(&casWon, 1)
			}
		}()
	}
	wg.Wait()
	casCount, _ := doorbelllab.CompleteCount(ctx, db, gid)

	// 素朴版（事故）: 別 goal で read-then-write を並行
	const gid2 = int64(9)
	if err := doorbelllab.Seed(ctx, db, gid2, "t1"); err != nil {
		t.Fatal(err)
	}
	var wg2 sync.WaitGroup
	for i := 0; i < wakes; i++ {
		wg2.Add(1)
		go func() { defer wg2.Done(); _ = doorbelllab.CompleteNaive(ctx, db, gid2) }()
	}
	wg2.Wait()
	naiveCount, _ := doorbelllab.CompleteCount(ctx, db, gid2)

	rec.Add(expkit.Variant{
		Name:     "完了 DB CAS: 重複/並行ドアベル 50 でも complete_count=1（exactly-once）",
		Counters: map[string]int64{"wakes": wakes, "cas_won": int64(casWon), "complete_count": int64(casCount)},
	})
	rec.Add(expkit.Variant{
		Name:     "素朴 read-then-write: 並行で二重完了（事故）",
		Accident: true,
		Counters: map[string]int64{"wakes": wakes, "complete_count": int64(naiveCount)},
		Notes:    []string{"complete_count=" + itoa(naiveCount) + "（>1＝二重完了。配信でなく CAS が正しさを持つべき理由）"},
	})

	// ---- ③ crash 回収は reconcile（event でなく lease 失効）----
	const gid3 = int64(3)
	if err := doorbelllab.Seed(ctx, db, gid3, "t1"); err != nil {
		t.Fatal(err)
	}
	okA, err := doorbelllab.ClaimLease(ctx, db, gid3, "worker-A", 1*time.Second) // A が担当
	if err != nil {
		t.Fatal(err)
	}
	// A は crash（完了も更新もしない・ドアベルも出ない）
	beforeExpiry, _ := doorbelllab.ReconcileExpired(ctx, db) // まだ失効前 → gid3 は出ないはず
	beforeHas := contains(beforeExpiry, gid3)

	time.Sleep(1300 * time.Millisecond) // DB 時計で lease 失効を待つ
	afterExpiry, _ := doorbelllab.ReconcileExpired(ctx, db)
	afterHas := contains(afterExpiry, gid3)

	reclaimedByB := false
	completedByB := false
	if afterHas { // reconcile が拾ったら B が取り直して完了
		okB, _ := doorbelllab.ClaimLease(ctx, db, gid3, "worker-B", 1*time.Second)
		reclaimedByB = okB
		done, _ := doorbelllab.CompleteCAS(ctx, db, gid3)
		completedByB = done
	}
	cc3, _ := doorbelllab.CompleteCount(ctx, db, gid3)

	rec.Add(expkit.Variant{
		Name:     "crash 回収: lease 失効の reconcile でのみ拾える（event は来ない）",
		Counters: map[string]int64{
			"a_claimed": b2i(okA), "reconcile_before_expiry": b2i(beforeHas),
			"reconcile_after_expiry": b2i(afterHas), "b_reclaimed": b2i(reclaimedByB),
			"b_completed": b2i(completedByB), "complete_count": int64(cc3),
		},
		Notes: []string{"失効前 sweep=拾えない / 失効後 sweep=拾える → B が完了。complete_count=" + itoa(cc3)},
	})
	t.Logf("① touches tight=%d dOnly=%d(stuck%d) dFloor=%d(stuck%d,lat%d) / ② cas cc=%d naive cc=%d / ③ before=%v after=%v cc=%d",
		tight.touches, dOnly.touches, dOnly.stuck, dFloor.touches, dFloor.stuck, dFloor.maxLat, casCount, naiveCount, beforeHas, afterHas, cc3)

	// ---- 検証 ----
	// ①
	if tight.stuck != 0 || tight.touches != T+1 {
		t.Errorf("tight: touches=%d stuck=%d", tight.touches, tight.stuck)
	}
	if dOnly.stuck == 0 {
		t.Errorf("doorbell のみで拾い漏れが出ていない（floor が要る証明にならない）")
	}
	if dFloor.stuck != 0 {
		t.Errorf("doorbell+floor で拾い漏れ: %d", dFloor.stuck)
	}
	if dFloor.touches >= tight.touches {
		t.Errorf("doorbell+floor が tight より接触が減っていない: %d>=%d", dFloor.touches, tight.touches)
	}
	// ②
	if casCount != 1 || casWon != 1 {
		t.Errorf("CAS 完了が exactly-once でない: complete_count=%d won=%d", casCount, casWon)
	}
	if naiveCount <= 1 {
		t.Errorf("素朴版が二重完了していない（するはず）: %d", naiveCount)
	}
	// ③
	if !okA {
		t.Errorf("A が lease を取れていない")
	}
	if beforeHas {
		t.Errorf("失効前に reconcile が拾った（拾ってはいけない）")
	}
	if !afterHas || !reclaimedByB || !completedByB || cc3 != 1 {
		t.Errorf("失効後の回収が成立していない: after=%v reclaim=%v complete=%v cc=%d", afterHas, reclaimedByB, completedByB, cc3)
	}

	rec.Scope(
		"MySQL 8.0 / lease は DB 時計(NOW(3))・ttl=1s / ①は純ロジック 60秒・仕事 2,5,30・30 のドアベル欠落",
		"doorbell=起こす信号（poll のトリガ）、完了/lease は DB CAS。event は floor の上の遅延最適化",
		"②complete_count=完了が走った回数（exactly-once なら 1）。③reconcile=失効 lease の sweep",
	)
	rec.Uncertain(
		"①の論理秒・backoff cap は説明用。実運用は SQS 可視性タイムアウト≒lease、指数＋ジッタ（EXP-17）",
		"跨プロセス通知は MySQL に LISTEN/NOTIFY が無いので SQS/EventBridge で代替（doorbell）。DB は依然 authority",
		"実 SQS/EventBridge での failover 跨ぎ fence 単調性・重複/順序入替配信下の exactly-once は LIVE_ENV_REQUIRED（未実測）",
		"crash 回収の速さは lease ttl と reconcile 周期で決まる。短いほど速いが誤失効の危険（clock skew・EXP-2）",
	)
	rec.Artifact(
		"internal/doorbelllab: doorbell(poll トリガ) vs floor、完了 CAS、lease 失効 reconcile",
		"docs/event-driven-worker.md: イベント駆動の適用範囲（floor は poll/reconcile、event は doorbell）",
	)
	rec.Next("（実 SQS/EventBridge は LIVE_ENV_REQUIRED）")

	files, err := rec.Save(
		"worker のイベント駆動は『doorbell（起こす信号）＋DB が authority』。① doorbell+adaptive backoff は " +
			"tight poll より DB 接触が激減し、仕事があれば即起床（遅延0）。ただし event だけだと落ちたドアベルの " +
			"仕事を永久に拾えず、poll/reconcile の floor が要る。② 完了は DB CAS で exactly-once（重複/並行 " +
			"ドアベル 50 でも complete_count=1）。素朴 read-then-write は二重完了する。③ crash 回収は event で " +
			"なく DB 時計の lease 失効＋reconcile sweep でのみ成立（担当が死ぬとイベントは来ない）。" +
			"＝event は floor の上に載せる遅延最適化であって、正しさ（完了・lease・fence）は DB CAS のまま。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func contains(xs []int64, v int64) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
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
