package fencelab_test

// EXP-2: lease / fencing / clock skew。
//
//	MYSQL_DSN=... go test ./internal/fencelab/ -run TestEXP2 -v -timeout 20m
//
// 守りたい不変条件:
//   1. lease を失った worker は、それ以降 commit できない
//   2. 新しい担当の fence より古い write は拒否される
//   3. プロセスの自己申告時刻だけで lease を奪えない
//   4. 同時に claim したとき、勝者は1つ

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/fencelab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

const ttl = 1500 * time.Millisecond

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := fencelab.Open(mysqltest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	if err := fencelab.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEXP2_lease_fencing_clockskew(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/fencelab")

	rec := expkit.NewRecorder("EXP-2", "lease-fencing-clock-skew",
		"止まった worker・ずれた時計・同時 claim のもとで、古い担当の書き込みを止められるか")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) lease だけでは、停止していた worker が再開したときの書き込みを止められない。",
		"2) fence（担当が変わるたびに増える番号）を書き込み条件に入れると止まる。",
		"3) 期限の判定を各プロセスの時計で行うと、時計が進んだプロセスが生きた lease を奪える。",
		"   DB の時計で判定すれば奪えない。",
		"4) 同時に claim したとき、勝者は1つになる（lease 方式のいずれでも）。",
	}, " "))
	rec.Workload("ttl", ttl.String()).
		Workload("modes", []string{
			string(fencelab.ModeNoLease), string(fencelab.ModeLocalClock),
			string(fencelab.ModeDBClock), string(fencelab.ModeFencing)}).
		Injection("pause", "SIGUSR1 待ちで worker を書き込み直前に停止（GC の長い停止・OS による停止の相当）").
		Injection("clock_skew", "worker の自己申告時刻を +60s ずらす").
		Injection("concurrent_claim", "8 プロセス同時起動")

	// ---- 実験A: 止まっていた worker が再開したときの書き込み ----
	for _, mode := range []fencelab.Mode{fencelab.ModeDBClock, fencelab.ModeFencing} {
		res := staleWriterCase(t, ctx, db, bin, mode)
		rec.Add(res)
	}

	// ---- 実験B: 時計のずれで lease を奪えるか ----
	for _, mode := range []fencelab.Mode{fencelab.ModeLocalClock, fencelab.ModeDBClock} {
		rec.Add(clockSkewCase(t, ctx, db, bin, mode))
	}

	// ---- 実験C: 同時 claim の勝者 ----
	for _, mode := range []fencelab.Mode{fencelab.ModeNoLease, fencelab.ModeDBClock, fencelab.ModeFencing} {
		rec.Add(concurrentClaimCase(t, ctx, db, bin, mode))
	}

	rec.Scope(
		"MySQL 8.0 / 単一インスタンス / 同一ホストの複数プロセス",
		"停止は SIGUSR1 待ちで再現（SIGSTOP と同じく、その間プロセスは何もしない）",
		"fence は DB の1行で単調増加させている（外部の採番サービスは使っていない）",
	)
	rec.Uncertain(
		"ネットワーク分断（DB へは届くが worker 間では見えない）は未再現",
		"DB のフェイルオーバーで fence 行が巻き戻る場合は未検証",
		"NTP による時刻の飛び（時計が後ろ向きに跳ぶ）は未再現",
		"GET_LOCK 方式は担当の表現としては測っていない（接続占有と期限なしの問題は docs/locking.md）",
	)
	rec.Artifact(
		"internal/fencelab: 4方式（lease 無し / 自己時計 / DB 時計 / DB 時計+fence）の実装",
		"cmd/fencelab: 書き込み直前で止まり、SIGUSR1 で再開する worker",
		"fence_audit 表: 受理・拒否の両方を残すので、事後に「誰の書き込みが通ったか」を数えられる",
	)
	rec.Next("EXP-3 graceful shutdown: 担当を持ったまま終了するときの手順")

	files, err := rec.Save(strings.Join([]string{
		"lease の期限判定を DB の時計に寄せると、時計がずれたプロセスによる乗っ取りは止まる。",
		"しかし『停止していた worker が再開して書く』のは lease だけでは止まらない。",
		"書き込み条件に fence を入れて初めて、古い担当の書き込みが DB 側で拒否された。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

// 実験A: worker A を書き込み直前で止め、その間に B が担当を取る。
// A を再開させたときの書き込みが通るかどうか。
func staleWriterCase(t *testing.T, ctx context.Context, db *sql.DB, bin string, mode fencelab.Mode) expkit.Variant {
	t.Helper()
	tenant := "t-stale-" + short(string(mode))
	if err := fencelab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	// A: 1回目の書き込み直前で止まる
	a, err := expkit.StartChild(ctx, bin, args(tenant, "A", mode,
		"-writes", "2", "-pause-before-write", "1", "-check-renew", "true", "-stop-on-lost", "false"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WaitPoint("paused_before_write", 15*time.Second); err != nil {
		t.Fatalf("[%s] A が停止地点に達しない: %v", mode, err)
	}
	ownerA, fenceA, err := fencelab.CurrentOwner(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}

	// lease が切れるまで待つ（A は止まっているので延長できない）
	time.Sleep(ttl + 400*time.Millisecond)

	// B: 担当を引き継いで書く
	b, err := expkit.StartChild(ctx, bin, args(tenant, "B", mode, "-writes", "2"))
	if err != nil {
		t.Fatal(err)
	}
	binfo, err := b.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if binfo.Code != 0 {
		t.Fatalf("[%s] B が担当を取れなかった: %s\n%s", mode, binfo, binfo.Stdout)
	}
	ownerB, fenceB, err := fencelab.CurrentOwner(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}

	// A を再開させる。A は「まだ自分が担当」と思っている
	if err := a.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	ainfo, err := a.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	acceptedA, err := fencelab.AcceptedWritesBy(ctx, db, tenant, "A")
	if err != nil {
		t.Fatal(err)
	}
	writer, fenceState, value, err := fencelab.StateWriter(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}

	v := expkit.Variant{
		Name: fmt.Sprintf("停止していた worker の再開 / %s", mode),
		Desc: "A を書き込み直前で止め、lease 切れ後に B が担当。そのあと A を再開させる",
		Counters: map[string]int64{
			"fence_before":             int64(fenceA),
			"fence_after_takeover":     int64(fenceB),
			"accepted_writes_by_stale": int64(acceptedA),
			"final_state_fence":        int64(fenceState),
			"final_state_value":        value,
		},
		Accident: acceptedA > 0,
		Notes: []string{
			fmt.Sprintf("担当: %s(fence=%d) → %s(fence=%d)", ownerA, fenceA, ownerB, fenceB),
			fmt.Sprintf("最後に書いたのは %s（fence=%d）", writer, fenceState),
			fmt.Sprintf("A の終了: %s / B の終了: %s", ainfo, binfo),
		},
	}
	if mode == fencelab.ModeFencing {
		if acceptedA > 0 {
			t.Errorf("[%s] fence があるのに古い担当の書き込みが %d 件通った", mode, acceptedA)
		}
		v.Notes = append(v.Notes, "古い fence の書き込みは DB 側で拒否された（WHERE fence <= ?）")
	} else {
		if acceptedA == 0 {
			t.Errorf("[%s] 事故を再現できていない（fence 無しでも古い書き込みが止まった）", mode)
		}
		v.Notes = append(v.Notes, "★lease を失っていても書けてしまう。停止と再開の間に担当が変わったことを、書き込み側は知らない")
	}
	t.Logf("[%s] 停止→担当交代→再開: 古い担当の書き込みで受理されたもの %d 件", mode, acceptedA)
	return v
}

// 実験B: 時計が進んだプロセスが、生きている lease を奪えるか。
func clockSkewCase(t *testing.T, ctx context.Context, db *sql.DB, bin string, mode fencelab.Mode) expkit.Variant {
	t.Helper()
	tenant := "t-skew-" + short(string(mode))
	if err := fencelab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	// A: 正しい時計で担当を取り、長めに保持する
	a, err := expkit.StartChild(ctx, bin, args(tenant, "A", mode,
		"-writes", "6", "-interval", "300ms"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WaitPoint("acquired", 15*time.Second); err != nil {
		t.Fatalf("[%s] A が担当を取れない: %v", mode, err)
	}

	// B: 時計が 60 秒進んでいる。自分の時計だけを見れば「切れている」ように見える
	b, err := expkit.StartChild(ctx, bin, args(tenant, "B", mode,
		"-writes", "1", "-skew", "60s", "-acquire-timeout", "1s"))
	if err != nil {
		t.Fatal(err)
	}
	binfo, err := b.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stolen := binfo.Code == 0 // 取れてしまったか
	t.Logf("[%s] B(skew) の出力: %q / stderr: %q", mode, binfo.Stdout, binfo.Stderr)

	ainfo, err := a.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	acceptedB, err := fencelab.AcceptedWritesBy(ctx, db, tenant, "B")
	if err != nil {
		t.Fatal(err)
	}

	v := expkit.Variant{
		Name: fmt.Sprintf("時計が 60 秒進んだプロセスの割り込み / %s", mode),
		Desc: "A が正常に担当を保持している間に、時計がずれた B が担当を取りに来る",
		Counters: map[string]int64{
			"stolen":                    boolToInt(stolen),
			"accepted_writes_by_skewed": int64(acceptedB),
		},
		Accident: stolen,
		Notes: []string{
			fmt.Sprintf("A の終了: %s / B の終了: %s", ainfo, binfo),
		},
	}
	if mode == fencelab.ModeLocalClock {
		if !stolen {
			t.Errorf("[%s] 事故を再現できていない（自己時計方式なのに奪えなかった）", mode)
		}
		v.Notes = append(v.Notes, "★期限を自分の時計で判定すると、時計がずれたプロセスが生きた lease を奪える")
	} else {
		if stolen {
			t.Errorf("[%s] DB の時計で判定しているのに lease を奪われた", mode)
		}
		v.Notes = append(v.Notes, "期限の判定を DB の時計に寄せると、プロセスの時計がずれても奪えない")
	}
	t.Logf("[%s] 時計 +60s のプロセスが担当を奪えたか: %v（その書き込み %d 件）", mode, stolen, acceptedB)
	return v
}

// 実験C: 8 プロセスが同時に claim したときの勝者の数。
func concurrentClaimCase(t *testing.T, ctx context.Context, db *sql.DB, bin string, mode fencelab.Mode) expkit.Variant {
	t.Helper()
	tenant := "t-claim-" + short(string(mode))
	if err := fencelab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := 0; i < n; i++ {
		owner := fmt.Sprintf("W%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// ★勝者を「同じ瞬間」で数えるため、担当を持ったまま待たせる。
			// 取ってすぐ手放すと、順番に取り直しただけの回数を「勝者」と数えてしまう
			// （最初の測定はこれで勝者 3 と出た。実装ではなく測り方が間違っていた）
			c, err := expkit.StartChild(ctx, bin, args(tenant, owner, mode,
				"-writes", "1", "-acquire-timeout", "400ms",
				"-hold", "1200ms", "-release=false"))
			if err != nil {
				t.Error(err)
				return
			}
			info, err := c.Wait(30 * time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			if info.Code == 0 {
				mu.Lock()
				winners = append(winners, owner)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	v := expkit.Variant{
		Name:     fmt.Sprintf("8 プロセス同時 claim / %s", mode),
		Counters: map[string]int64{"winners": int64(len(winners))},
		Accident: len(winners) != 1,
		Notes:    []string{fmt.Sprintf("担当を取れたのは %v", winners)},
	}
	if mode == fencelab.ModeNoLease {
		if len(winners) <= 1 {
			t.Errorf("[%s] 事故を再現できていない: 勝者 %d", mode, len(winners))
		}
		v.Notes = append(v.Notes, "★担当を決めなければ全員が書ける（二重稼働）")
	} else if len(winners) != 1 {
		t.Errorf("[%s] 同時 claim の勝者が %d（1 であるべき）: %v", mode, len(winners), winners)
	}
	t.Logf("[%s] 8プロセス同時 claim の勝者: %d", mode, len(winners))
	return v
}

func args(tenant, owner string, mode fencelab.Mode, extra ...string) []string {
	base := []string{
		"-dsn", mustDSN(), "-tenant", tenant, "-owner", owner,
		"-mode", string(mode), "-ttl", ttl.String(),
	}
	return append(base, extra...)
}

func mustDSN() string { return dsnValue }

var dsnValue string

func TestMain(m *testing.M) {
	dsnValue = os.Getenv("MYSQL_DSN")
	os.Exit(m.Run())
}

func short(s string) string {
	s = strings.ReplaceAll(s, "lease_", "")
	s = strings.ReplaceAll(s, "_", "")
	if len(s) > 10 {
		s = s[:10]
	}
	return s
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
