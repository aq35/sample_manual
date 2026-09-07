package gql_test

// EXP-41: gqlgen のサブスクリプションを hub で作る。
//
//	go test ./internal/gql/ -run TestEXP41 -v   （DB 不要）
//
// gqlgen の subscription リゾルバは「チャネルを返す」だけ。接続ごとに DB を引かず、テナント単位の
// hub（ssehub.Registry）に相乗りする。版が変わると全接続へ流れ、ctx が切れると購読解除される。
// ここではリゾルバを直接呼び、hub からの配信が流れること・切断で後始末されることを確かめる
// （WebSocket トランスポートは NewServer 側で AddTransport 済み。ここは配線の意味論に集中）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/ssehub"
)

func TestEXP41_gqlgenサブスクリプション(t *testing.T) {
	ctx0 := context.Background()
	rec := expkit.NewRecorder("EXP-41", "gqlgen-subscription-hub",
		"gqlgen の subscription をテナント単位 hub で作る（接続ごとに DB を引かない）")
	rec.Env(expkit.CaptureEnv(ctx0, nil)) // DB 不要
	rec.Freeze(
		"1) subscription リゾルバはチャネルを返すだけ。DB は引かず hub（Registry）に相乗りする。 " +
			"2) テナントの版が変わると、購読チャネルに流れる。 " +
			"3) ctx が切れるとリゾルバは購読解除し、hub の最後の1人なら poller も止まる（漏れ防止）。 " +
			"4) テナントは ctx から取る（subscription でも引数からは取らない）。")

	// hub の DB 読みを模す in-memory ローダ（版を atomic で持つ）。DB 読み回数も数える。
	var ver, loadCalls int64
	load := func(ctx context.Context, tenant string) (int64, error) {
		atomic.AddInt64(&loadCalls, 1)
		return atomic.LoadInt64(&ver), nil
	}
	reg := ssehub.NewRegistry(load, 20*time.Millisecond, 16)
	r := &gql.Resolver{Events: reg}

	ctx, cancel := context.WithCancel(gql.WithTenant(ctx0, model.TenantID("t1")))
	ch, err := r.Subscription().RobotVersion(ctx)
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}

	first, ok := recv(t, ch, time.Second) // 最初の配信（版 0）
	if !ok {
		t.Fatal("最初の配信が来ない")
	}
	atomic.StoreInt64(&ver, 5)
	got5 := recvUntil(t, ch, 5, time.Second)
	atomic.StoreInt64(&ver, 9)
	got9 := recvUntil(t, ch, 9, time.Second)

	activeWhile := reg.ActiveTenants()

	// 切断 → チャネルが閉じ、購読解除される
	cancel()
	closed := waitClosed(t, ch, time.Second)
	time.Sleep(80 * time.Millisecond) // release＋poller 停止を待つ
	activeAfter := reg.ActiveTenants()

	rec.Add(expkit.Variant{
		Name:     "subscription が hub から配信を受ける（tenant は ctx から）",
		Counters: map[string]int64{"first": int64(first), "got5": b2i(got5), "got9": b2i(got9), "active_while": int64(activeWhile)},
		Notes:    []string{"版 0→5→9 が購読チャネルへ流れた。接続は hub に相乗り（DB 読みは poller の分だけ）"},
	})
	rec.Add(expkit.Variant{
		Name:     "ctx 切断 → チャネルが閉じ、購読解除（poller も止まる）",
		Counters: map[string]int64{"channel_closed": b2i(closed), "active_after": int64(activeAfter)},
		Notes:    []string{"active_tenants " + itoa(activeWhile) + " → " + itoa(activeAfter) + "（最後の1人が抜け poller 停止）"},
	})
	t.Logf("sub: first=%d got5=%v got9=%v active %d→%d closed=%v",
		first, got5, got9, activeWhile, activeAfter, closed)

	// ---- 検証 ----
	if !got5 || !got9 {
		t.Errorf("版変更が購読チャネルに届いていない: got5=%v got9=%v", got5, got9)
	}
	if activeWhile != 1 {
		t.Errorf("購読中のアクティブテナントが1でない: %d", activeWhile)
	}
	if !closed {
		t.Errorf("ctx 切断でチャネルが閉じない（goroutine 漏れ）")
	}
	if activeAfter != 0 {
		t.Errorf("切断後もテナントが残っている（poller 漏れ）: %d", activeAfter)
	}

	// tenant が無ければ subscription は張れない（引数から取らない）
	if _, err := r.Subscription().RobotVersion(ctx0); err == nil {
		t.Errorf("テナント無しでも subscription が張れてしまった")
	}
	// Events 未設定なら使えない
	if _, err := (&gql.Resolver{}).Subscription().RobotVersion(gql.WithTenant(ctx0, "t1")); err == nil {
		t.Errorf("Events 未設定でも subscription が張れてしまった")
	}

	rec.Scope(
		"純 Go（DB 不要）/ in-memory ローダで版を模す / gqlgen subscription リゾルバを直接呼ぶ",
		"WebSocket トランスポートは NewServer 側で AddTransport 済み。ここは hub 配線の意味論",
		"tenant は ctx（WithTenant）。subscription でも引数からは取らない",
	)
	rec.Uncertain(
		"実配信は WebSocket（or graphql-sse）トランスポート越し。ここはリゾルバ→hub の配線を検証",
		"接続数の上限はメモリ/fd（EXP-31/40）。fan-out は激安（EXP-38）",
		"複数プロセスに購読者が分かれると poller はプロセスごと → プロセス跨ぎは pub/sub（EXP-38）",
	)
	rec.Artifact(
		"internal/gql: subscription リゾルバ（ssehub.Registry に相乗り）＋ WebSocket トランスポート配線",
		"docs/graphql.md / docs/sse-fan-in.md: gqlgen サブスクリプションと hub",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"gqlgen のサブスクリプションは『チャネルを返す』だけ。接続ごとに DB を引かず、テナント単位の" +
			"hub（Registry）に相乗りさせる。版が変わると全接続へ流れ、ctx 切断で購読解除＋poller 停止" +
			"（漏らさない）。テナントは ctx から取り、WebSocket は NewServer で AddTransport する。" +
			"何人まで・何タスク要るかは EXP-40 の計算機で（hub あり＝メモリ/fd 律速）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func recv(t *testing.T, ch <-chan int, d time.Duration) (int, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(d):
		return 0, false
	}
}

// recvUntil は want が来るまで受信する（間の値は読み飛ばす）。
func recvUntil(t *testing.T, ch <-chan int, want int, d time.Duration) bool {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return false
			}
			if v == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func waitClosed(t *testing.T, ch <-chan int, d time.Duration) bool {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func b2i(v bool) int64 {
	if v {
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
