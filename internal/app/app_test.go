package app_test

// §2 の主張を、外部サービスの代役（fakesvc）と実際の MySQL を相手に確かめる。
// ここが「資料のとおりに作ると、資料のとおりに壊れない」ことの確認になる。

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/app"
	"github.com/aq35/sample_manual/internal/fakesvc"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

const tenant = model.TenantID("t-app")

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setup(t *testing.T, cfg app.Config) (*app.App, *fakesvc.Service, *store.Store) {
	t.Helper()
	s := mysqltest.Store(t, store.DefaultPool())
	mysqltest.Truncate(t, s.DB(), string(tenant))

	svc := fakesvc.New()
	t.Cleanup(svc.Close)

	cfg.Tenant = tenant
	cfg.APIURL = svc.URL() + "/robots"
	cfg.WSURL = svc.WSURL()
	cfg.Logger = quietLogger()
	return app.New(cfg, s), svc, s
}

// §2.3「準備完了の宣言が、全件取得の後になっているか」
func TestReady_全件取得が終わるまで準備完了にしない(t *testing.T) {
	a, svc, s := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		BatchEvery:     50 * time.Millisecond,
	})
	for i := 0; i < 20; i++ {
		svc.Add(id(i), "running", true, 80)
	}

	if a.Ready() {
		t.Fatal("起動前から準備完了になっている")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return a.Ready() })

	// 準備完了を宣言した時点で、DB には全件入っている（画面に「不明」が出ない）
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM robot_state WHERE tenant_id = ?", string(tenant)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("準備完了なのに DB に %d 件しか入っていない", n)
	}
	cancel()
	<-done
}

// §2.2「A は省略できない」
// B（鮮度チェック）だけでは、名簿の変化と間違った値を拾えないことを実際に見る。
func TestFullSync_Bだけでは拾えないものがある(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		StaleAfter:     50 * time.Millisecond, // すぐ鮮度切れにする
		BatchEvery:     50 * time.Millisecond,
	})
	ctx := context.Background()
	svc.Add("r001", "running", true, 80)
	if err := a.FullSync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// ① 名簿に新しい対象が増えた。WebSocket には流れてこない。
	svc.Add("r002", "running", true, 60)
	// ② 既存の対象の値が、通知なしに変わった（＝こちらが持っている値は間違い）
	svc.SetQuietly("r001", "error", true, 10)

	// B を回す: 鮮度が切れているものだけ取り直す
	time.Sleep(60 * time.Millisecond)
	if err := a.FreshnessCheck(ctx); err != nil {
		t.Fatal(err)
	}

	if lv, _ := a.Tracker().Liveness("r002"); lv != worker.LivenessUnobserved {
		t.Fatal("B が新規追加を拾えてしまった（テストの前提が崩れている）")
	}
	t.Log("① 新規追加された r002 は、B の対象リストにそもそも載らないので拾えない")

	// A を回す: 名簿ごと取り直す
	if err := a.FullSync(ctx); err != nil {
		t.Fatal(err)
	}
	// 名簿に載ったので、記憶に現れる（鮮度は StaleAfter を 50ms にしてあるため
	// すぐ unknown になりうる。ここで見たいのは「知っているかどうか」）。
	if lv, _ := a.Tracker().Liveness("r002"); lv == worker.LivenessUnobserved {
		t.Fatal("A でも新規追加を拾えていない")
	}
	if _, st := a.Tracker().Liveness("r001"); st.Status != model.StatusError {
		t.Fatalf("A で値のズレが直っていない: %v", st.Status)
	}
	t.Log("② A（全件同期）を回すと、名簿の追加も、間違った値の訂正も入る")
}

// §2.5「オフライン判定を、沈黙から行っていないか」
// §2.4「API も答えない → 不明のまま（ここで嘘をつかない）」
func TestUnknown_APIが答えないときは不明のままにする(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		StaleAfter:     50 * time.Millisecond,
		BatchEvery:     50 * time.Millisecond,
	})
	ctx := context.Background()
	svc.Add("r001", "running", true, 80)
	if err := a.FullSync(ctx); err != nil {
		t.Fatal(err)
	}

	svc.SetAPIDown(true)
	time.Sleep(60 * time.Millisecond)

	if err := a.FreshnessCheck(ctx); err == nil {
		t.Fatal("API が落ちているのに成功した")
	}
	lv, st := a.Tracker().Liveness("r001")
	if lv != worker.LivenessUnknown {
		t.Fatalf("鮮度切れなのに unknown になっていない: %v", lv)
	}
	if st.Status == model.StatusStopped || !st.Online {
		t.Fatal("API が答えないのに「停止・オフライン」と断定している（§2.4 違反）")
	}
	t.Log("API が答えない → 状態は「不明」。オフラインとは言わない")

	// API が復旧して「停止中」と答えたら、そこで初めてオフラインが確定する
	svc.SetAPIDown(false)
	svc.SetQuietly("r001", "stopped", false, 0)
	if err := a.FreshnessCheck(ctx); err != nil {
		t.Fatal(err)
	}
	lv, st = a.Tracker().Liveness("r001")
	if lv != worker.LivenessObserved || st.Online {
		t.Fatalf("API の回答が反映されていない: %v %+v", lv, st)
	}
	t.Log("API が「停止中」と答えた → オフライン確定")
}

// §2.6「接続は正常」という監視はほぼ何も見ていない、を実際に見る。
func TestSilent_接続は生きているのに何も来ない(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		StaleAfter:     200 * time.Millisecond,
		BatchEvery:     50 * time.Millisecond,
		PingEvery:      10 * time.Second, // ここでは Ping では検出しない
		PongWait:       10 * time.Second,
	})
	svc.Add("r001", "running", true, 80)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return a.Ready() })
	waitFor(t, 5*time.Second, func() bool { return svc.Conns() > 0 })

	svc.Publish("r001", "running", true, 75)
	waitFor(t, 5*time.Second, func() bool { return !a.LastReceived().IsZero() })

	// サービスが送信をやめた。接続は維持されたまま。
	svc.SetSilent(true)
	svc.Publish("r001", "error", true, 5) // 届かない
	time.Sleep(500 * time.Millisecond)

	if svc.Conns() == 0 {
		t.Fatal("接続が切れてしまった（このテストの前提が崩れている）")
	}
	silence := time.Since(a.LastReceived())
	if silence < 300*time.Millisecond {
		t.Fatalf("沈黙していない: %v", silence)
	}
	t.Logf("接続数=%d（正常に見える）なのに、最後の受信から %v 経っている", svc.Conns(), silence.Round(10*time.Millisecond))
	t.Log("→ 監視するのは「接続の有無」ではなく「最後に受信した時刻」（§2.6）")

	// この状態は WebSocket からは直せない。A/B が API で取りに行って初めて直る。
	if err := a.FreshnessCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, st := a.Tracker().Liveness("r001"); st.Status != model.StatusError {
		t.Fatalf("API で取り直しても直っていない: %v", st.Status)
	}
	t.Log("→ 沈黙の間に起きた変化は、B が API で取り直して初めて反映される")
	cancel()
	<-done
}

// §2.6「Pong が N 回返らなければ、自分から切って繋ぎ直す」
func TestPing_応答が無ければ自分から切って繋ぎ直す(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		BatchEvery:     50 * time.Millisecond,
		PingEvery:      150 * time.Millisecond,
		PongWait:       150 * time.Millisecond,
		MaxNoPong:      2,
	})
	svc.Add("r001", "running", true, 80)
	svc.SetNoPong(true) // Ping に応答しないサービス

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return a.Ready() })

	// 読み取り期限（PingEvery×MaxNoPong + PongWait ≒ 450ms）で切れて張り直す
	waitFor(t, 5*time.Second, func() bool { return a.Reconnects() >= 2 })
	t.Logf("Pong が返らない接続を %d 回張り直した（TCP は生きているので、これが無いと永久に無音のまま）", a.Reconnects())
	cancel()
	<-done
}

// §2.1「イベントは差分、状態は取りに行く」— WebSocket が効いているときは速い
func TestWebSocket_変化が即座に反映される(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		BatchEvery:     50 * time.Millisecond,
	})
	svc.Add("r001", "running", true, 80)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return a.Ready() })
	waitFor(t, 5*time.Second, func() bool { return svc.Conns() > 0 })

	start := time.Now()
	svc.Publish("r001", "error", true, 5)
	waitFor(t, 5*time.Second, func() bool {
		_, st := a.Tracker().Liveness("r001")
		return st.Status == model.StatusError
	})
	t.Logf("WebSocket 経由の反映まで %v（A は %v ごと、B は %v ごと）",
		time.Since(start).Round(time.Millisecond), time.Hour, time.Hour)
	cancel()
	<-done
}

// §6.3「知らない項目の検知は、起動時に1回だけ記録する。処理は止めない」
func TestUnknownField_知らない項目があっても止まらない(t *testing.T) {
	a, svc, _ := setup(t, app.Config{
		FullSyncEvery:  time.Hour,
		FreshnessEvery: time.Hour,
		BatchEvery:     50 * time.Millisecond,
	})
	// fakesvc は応答に experimental_field と schema_version を混ぜている
	svc.Add("r001", "running", true, 80)
	if err := a.FullSync(context.Background()); err != nil {
		t.Fatalf("知らない項目があるだけで全件同期が落ちた（DisallowUnknownFields を使っていないか）: %v", err)
	}
	if lv, _ := a.Tracker().Liveness("r001"); lv != worker.LivenessObserved {
		t.Fatal("取り込めていない")
	}
	t.Log("知らない項目（experimental_field / schema_version）があっても、全件同期は通る")
}

func id(i int) string {
	return string(rune('r')) + string(rune('0'+(i/100)%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("条件が %v 以内に満たされなかった", limit)
}
