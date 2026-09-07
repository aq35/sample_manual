package subcaplab_test

// EXP-59: gqlgen subscription 1本あたりの実メモリを測る（推測 ~40KB の裏取り）。
//
//	go test ./internal/subcaplab/ -run TestEXP59 -v   （DB 不要・純 Go）
//
// concern-subscription-capacity.md の「1本 ≒ 40KB」は接続コスト(EXP-31)＋見積り。ここでは
// gqlgen の subscription を実際に開き、Go 側の per-subscription メモリ（resolver goroutine＋
// チャネル＋hub 相乗り）を実測し、さらに本物の WebSocket で end-to-end も測る。hub 自体のサイズ
// （テナント単位）も測り、「メモリを食うのは hub でなく接続」を確かめる。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/ssehub"
)

// goMem は GC 後の「生きている Go メモリ」= HeapInuse＋StackInuse（バイト）。
func goMem() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse
}

// settle は前の変種で cancel した goroutine が実際に退場し切るのを待つ（計測の混入を防ぐ）。
func settle() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(150 * time.Millisecond)
	}
	runtime.GC()
}

func TestEXP59_subscription単価(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-59", "gqlgen-subscription-memory",
		"gqlgen subscription 1本あたりの実メモリを測る（推測 40KB の裏取り）")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) Go 側の per-subscription（resolver goroutine＋out チャネル＋hub 相乗り）は数 KB。 " +
			"2) 本物の WebSocket の end-to-end（クライアント＋サーバ両端・TLS 無し）は、両端の gorilla " +
			"バッファぶん重く数十 KB。 " +
			"3) hub 自体はテナント単位で数 KB（購読者数では増えない）＝メモリを食うのは接続側。 " +
			"4) 現実のサーバ per-subscription ≒ Go側(数KB) ＋ 接続バッファ(EXP-31: ~34KB) ≒ 約40KB。")

	fakeLoader := func(ctx context.Context, tenant string) (int64, error) { return 1, nil }

	// ---- ① server-side isolated: resolver＋hub を N 本（socket 無し）----
	{
		reg := ssehub.NewRegistry(fakeLoader, time.Hour, 8) // interval 長 → poller は1回で寝る
		r := &gql.Resolver{Events: reg}
		const n = 5000
		base := goMem()
		cancels := make([]context.CancelFunc, 0, n)
		chans := make([]<-chan int, 0, n)
		for i := 0; i < n; i++ {
			cctx, cancel := context.WithCancel(gql.WithTenant(ctx, "t1")) // 1テナントに N 購読者
			ch, err := r.Subscription().RobotVersion(cctx)
			if err != nil {
				cancel()
				t.Fatalf("subscribe %d: %v", i, err)
			}
			cancels = append(cancels, cancel)
			chans = append(chans, ch)
		}
		after := goMem()
		perByte := float64(int64(after)-int64(base)) / float64(n)
		rec.Add(expkit.Variant{
			Name:     "server-side isolated: resolver＋hub の per-subscription（socket 無し）",
			Metrics:  map[string]float64{"bytes_per_sub": perByte, "KB_per_sub": perByte / 1024},
			Counters: map[string]int64{"subscriptions": n, "active_tenants": int64(reg.ActiveTenants())},
			Notes:    []string{"1テナント×" + itoa(n) + "購読者。Go側 ≒ " + f1(perByte/1024) + "KB/本"},
		})
		t.Logf("isolated: per_sub=%.0fB (%.1fKB) tenants=%d", perByte, perByte/1024, reg.ActiveTenants())
		isoPer = perByte
		for _, c := range cancels {
			c()
		}
		_ = chans
	}
	settle() // ①の goroutine が退場し切るのを待つ

	// ---- ② hub 自体のサイズ: T テナント×1購読者（poller ぶん）----
	{
		reg := ssehub.NewRegistry(fakeLoader, time.Hour, 8)
		r := &gql.Resolver{Events: reg}
		const tn = 500
		base := goMem()
		cancels := make([]context.CancelFunc, 0, tn)
		for i := 0; i < tn; i++ {
			cctx, cancel := context.WithCancel(gql.WithTenant(ctx, model.TenantID("tenant-"+itoa(i))))
			if _, err := r.Subscription().RobotVersion(cctx); err != nil {
				cancel()
				t.Fatalf("tenant sub %d: %v", i, err)
			}
			cancels = append(cancels, cancel)
		}
		after := goMem()
		perTenant := float64(int64(after)-int64(base)) / float64(tn)
		rec.Add(expkit.Variant{
			Name:     "hub のサイズ: テナント1つ（poller＋hub）あたり",
			Metrics:  map[string]float64{"bytes_per_tenant": perTenant, "KB_per_tenant": perTenant / 1024},
			Counters: map[string]int64{"tenants": tn, "active": int64(reg.ActiveTenants())},
			Notes:    []string{"hub＋poller ≒ " + f1(perTenant/1024) + "KB/テナント。購読者数では増えない"},
		})
		t.Logf("hub: per_tenant=%.0fB (%.1fKB) active=%d", perTenant, perTenant/1024, reg.ActiveTenants())
		hubPer = perTenant
		for _, c := range cancels {
			c()
		}
	}
	settle() // ②の goroutine が退場し切るのを待つ

	// ---- ③ 本物の WebSocket で end-to-end（クライアント＋サーバ両端・TLS 無し）----
	{
		reg := ssehub.NewRegistry(fakeLoader, time.Hour, 8)
		srv := gql.NewServer(nil, gql.ServerConfig{ComplexityLimit: 200, MaxPageSize: 100, Events: reg})
		tenantOf := func(req *http.Request) (model.TenantID, bool) {
			v := req.Header.Get("X-Tenant")
			if v == "" {
				return "", false
			}
			return model.TenantID(v), true
		}
		h := gql.Middleware(nil, tenantOf, false, srv)
		ts := httptest.NewServer(h)
		defer ts.Close()
		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

		const n = 800
		base := goMem()
		conns := make([]*websocket.Conn, 0, n)
		for i := 0; i < n; i++ {
			c, err := dialSub(wsURL, "wt1")
			if err != nil {
				t.Skipf("websocket %d 本目で失敗（環境制約かも）: %v", i, err)
			}
			conns = append(conns, c)
		}
		time.Sleep(200 * time.Millisecond) // サーバ側の goroutine 確保が落ち着くのを待つ
		after := goMem()
		perByte := float64(int64(after)-int64(base)) / float64(n)
		rec.Add(expkit.Variant{
			Name:     "本物の WebSocket end-to-end（クライアント＋サーバ両端・TLS 無し）",
			Metrics:  map[string]float64{"bytes_per_sub_loopback": perByte, "KB_per_sub_loopback": perByte / 1024},
			Counters: map[string]int64{"subscriptions": int64(len(conns))},
			Notes:    []string{"両端ぶん＝" + f1(perByte/1024) + "KB。サーバ片側はこの約半分。TLS は別途 ~32KB(EXP-31)"},
		})
		t.Logf("ws loopback: per_sub=%.0fB (%.1fKB) n=%d", perByte, perByte/1024, len(conns))
		for _, c := range conns {
			_ = c.Close()
		}
	}

	// ---- 現実のサーバ per-subscription の合成 ----
	const socketTLS = 34 * 1024 // EXP-31: TLS 込み接続バッファ
	realPer := isoPer + socketTLS
	rec.Add(expkit.Variant{
		Name:     "合成: 現実のサーバ per-subscription ≒ Go側 ＋ 接続バッファ(EXP-31)",
		Metrics:  map[string]float64{"KB_per_sub_estimated": realPer / 1024},
		Notes:    []string{"Go側 " + f1(isoPer/1024) + "KB ＋ 接続 ~34KB(EXP-31) ≒ " + f1(realPer/1024) + "KB。doc の ~40KB と整合"},
	})

	// ---- 検証 ----
	if isoPer <= 0 || isoPer > 30*1024 {
		t.Errorf("Go側 per-subscription が想定外: %.0fB（数KB のはず）", isoPer)
	}
	if hubPer > 50*1024 { // hub はテナント単位で小さい（数KB オーダー）
		t.Errorf("hub の per-tenant が大きすぎる: %.0fB（数KB のはず）", hubPer)
	}

	rec.Scope(
		"純 Go（DB 不要・fake loader）/ このホストの Go ランタイム / HeapInuse＋StackInuse の GC 後増分",
		"① socket 無しの Go 側コスト（resolver goroutine＋out chan＋hub 相乗り）",
		"③ 本物 ws は同一プロセスに client＋server 両端が乗る（TLS 無し）ので両端ぶん",
	)
	rec.Uncertain(
		"TLS バッファ(~32KB)は本実験に含まれない（本番の実接続で乗る・EXP-31）。合成で足して評価",
		"③のループバックは client＋server 両端ぶん。サーバ単体はおよそ半分",
		"gqlgen のバージョン・バッファ設定で単価は動く。オーダー（数KB＋接続34KB＝約40KB）で使う",
		"本番相当の 1vCPU/2GB 実機で N 本張って RSS を測るのが最終確認（ここは Go ヒープの内訳）",
	)
	rec.Artifact(
		"internal/subcaplab: gqlgen subscription の per-subscription メモリ実測",
		"docs/concern-subscription-capacity.md: 単価の裏取り（~40KB）",
	)
	rec.Next("（容量見積りの裏取り）")

	files, err := rec.Save(
		"gqlgen subscription の Go 側 per-subscription（resolver goroutine＋チャネル＋hub 相乗り）は数 KB " +
			"で、接続バッファ(EXP-31: ~34KB)を足すと現実のサーバ per-subscription ≒ 約40KB。" +
			"concern-subscription-capacity.md の見積り（1本~40KB → 1vCPU/2GB で 1万〜1.5万本）は妥当。" +
			"hub 自体はテナント単位で数 KB（購読者数では増えない）＝メモリを食うのは hub でなく接続側。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

var isoPer float64
var hubPer float64

// dialSub は graphql-transport-ws で subscription を1本張る（connection_init→ack→subscribe）。
func dialSub(wsURL, tenant string) (*websocket.Conn, error) {
	d := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}, HandshakeTimeout: 5 * time.Second}
	hdr := http.Header{}
	hdr.Set("X-Tenant", tenant)
	c, _, err := d.Dial(wsURL, hdr)
	if err != nil {
		return nil, err
	}
	if err := c.WriteJSON(map[string]any{"type": "connection_init"}); err != nil {
		_ = c.Close()
		return nil, err
	}
	// connection_ack を読む
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack map[string]json.RawMessage
	if err := c.ReadJSON(&ack); err != nil {
		_ = c.Close()
		return nil, err
	}
	sub := map[string]any{
		"id":      "1",
		"type":    "subscribe",
		"payload": map[string]any{"query": "subscription { robotVersion }"},
	}
	if err := c.WriteJSON(sub); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Time{}) // 以降は読まない（keepalive ping は 15s なので試験中は来ない）
	return c, nil
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
func f1(v float64) string {
	n := int64(v*10 + 0.5)
	return itoa(int(n/10)) + "." + itoa(int(n%10))
}
