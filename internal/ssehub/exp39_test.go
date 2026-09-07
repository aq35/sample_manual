package ssehub_test

// EXP-39: 動く SSE エンドポイント（poller＋fan-out hub＋レジストリ）を1コンテナ内で。
//
//	MYSQL_DSN=... go test ./internal/ssehub/ -run TestEXP39 -v
//
// httptest で実際に N 本の SSE 接続を張り、テナントに1つの poller が DB を引いて全接続へ配ること、
// 接続数が増えても DB 読みは poller の分だけであること、切断で後始末されることを確かめる。

import (
	"bufio"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/ssehub"
)

func TestEXP39_SSEエンドポイント(t *testing.T) {
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
	const tenant = "sse-ep"
	if err := ssehub.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := ssehub.Seed(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-39", "sse-endpoint",
		"動く SSE エンドポイント: 1つの poller が引いて全接続へ配る（1コンテナ内）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) N 本の SSE 接続でも、テナントの poller は interval ごとに1回だけ DB を引く（接続数に依らない）。 " +
			"2) 版が変わると全接続が受け取る。 " +
			"3) 全接続が切れると poller は止まり、レジストリは空になる（goroutine/DB を漏らさない）。 " +
			"4) これは1コンテナ（1プロセス）内で成立する。")

	// レジストリ: load は subq を1回読むだけ（DB 読み回数は Registry が数える）
	var loadCalls int64
	load := func(ctx context.Context, tn string) (int64, error) {
		atomic.AddInt64(&loadCalls, 1)
		var v int64
		err := db.QueryRowContext(ctx, "SELECT version FROM subq WHERE tenant_id=?", tn).Scan(&v)
		return v, err
	}
	const interval = 30 * time.Millisecond
	reg := ssehub.NewRegistry(load, interval, 64)
	srv := httptest.NewServer(reg.Handler(func(r *http.Request) (string, bool) {
		return r.Header.Get("X-Tenant"), r.Header.Get("X-Tenant") != ""
	}))
	defer srv.Close()

	// N 本の SSE 接続を張り、受け取った data イベント数を数える
	const N = 12
	clients := make([]*sseClient, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		c := newSSEClient(t, srv.URL, tenant)
		clients[i] = c
		wg.Add(1)
		go func() { defer wg.Done(); c.run() }()
	}
	time.Sleep(150 * time.Millisecond) // poller 起動＋初期配信を待つ

	// 版を5回変える（各 poller tick より広い間隔で）→ 全接続が受け取るはず
	const bumps = 5
	for b := 0; b < bumps; b++ {
		if err := ssehub.Bump(ctx, db, tenant); err != nil {
			t.Fatal(err)
		}
		time.Sleep(70 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	readsWhileConnected := reg.Reads()
	activeWhileConnected := reg.ActiveTenants()

	// 全クライアント切断
	for _, c := range clients {
		c.stop()
	}
	wg.Wait()
	time.Sleep(150 * time.Millisecond) // release＋poller 停止を待つ
	activeAfter := reg.ActiveTenants()
	readsAfter := reg.Reads()
	time.Sleep(120 * time.Millisecond) // 停止後、poller が回っていないこと（reads が増えない）を確認
	readsStopped := reg.Reads()

	minEvents := 1 << 30
	for _, c := range clients {
		if g := c.count(); g < minEvents {
			minEvents = g
		}
	}

	rec.Add(expkit.Variant{
		Name:     "N=12 接続・版を5回変更: 全接続が受信、DB 読みは poller の分だけ",
		Counters: map[string]int64{"clients": N, "db_reads_connected": readsWhileConnected, "min_events_per_client": int64(minEvents), "active_tenants": int64(activeWhileConnected)},
		Notes:    []string{"素朴なら 接続数×tick で数百回。poller は1本ぶん " + itoa64(readsWhileConnected) + " 回。各接続 最低 " + itoa64(int64(minEvents)) + " 受信"},
	})
	rec.Add(expkit.Variant{
		Name:     "全切断後: poller 停止・レジストリ空・DB 読みが止まる",
		Counters: map[string]int64{"active_after": int64(activeAfter), "reads_at_disconnect": readsAfter, "reads_120ms_later": readsStopped},
		Notes:    []string{"active_tenants " + itoa64(int64(activeWhileConnected)) + " → " + itoa64(int64(activeAfter)) + " / reads は " + itoa64(readsAfter) + " から増えない(" + itoa64(readsStopped) + ")"},
	})
	t.Logf("connected: reads=%d active=%d min_events=%d / after: active=%d reads=%d→%d",
		readsWhileConnected, activeWhileConnected, minEvents, activeAfter, readsAfter, readsStopped)

	// ---- 検証 ----
	if activeWhileConnected != 1 {
		t.Errorf("接続中のアクティブテナントが1でない: %d", activeWhileConnected)
	}
	if minEvents < bumps { // 各接続が5回の変更を（初期含め）受け取れている
		t.Errorf("受信漏れ: 最小 %d（>=%d のはず）", minEvents, bumps)
	}
	if int64(N)*bumps <= readsWhileConnected {
		t.Errorf("DB 読みが接続数に比例している（fan-in を畳めていない）: reads=%d", readsWhileConnected)
	}
	if activeAfter != 0 {
		t.Errorf("全切断後もテナントが残っている（poller/リソース漏れ）: %d", activeAfter)
	}
	if readsStopped != readsAfter {
		t.Errorf("切断後も poller が DB を読み続けている: %d → %d", readsAfter, readsStopped)
	}

	rec.Scope(
		"MySQL 8.0 / httptest の実 SSE 接続 12 本 / poller interval 30ms / バッファ 64",
		"DB 読みは Registry が数える（poller の load 呼び出し）。1コンテナ・1プロセス内",
		"版変更は subq を5回 Bump。各 tick で load し変化時だけ配る（coalesce）",
	)
	rec.Uncertain(
		"タイミング依存（interval・sleep）。桁（poller 1本 vs 接続数×）が要点で絶対値は環境で動く",
		"実運用の接続数上限はメモリ/fd（EXP-31: ~1万〜1.5万/タスク）。ここは仕組みの検証",
		"複数プロセスに接続が分かれると各プロセスに poller が要る → プロセス跨ぎは pub/sub（EXP-38）",
		"初期スナップショットは poller キャッシュから配る（接続ごとの DB 読みは無い）",
	)
	rec.Artifact(
		"internal/ssehub: Registry（テナント単位 poller・参照カウント）と SSE HTTP ハンドラ",
		"docs/sse-fan-in.md: 多数購読時の対策と hub の作り方",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"SSE は『テナントに1つの poller が DB を引き、hub で全接続へ配る』を1コンテナ内で実現できる。" +
			"接続がいくら増えても DB 読みは poller の分だけ。切断は req.Context().Done() で検知し参照カウントで" +
			"poller を止める（漏らさない）。接続数がメモリ/fd 上限を超えるか隔離したいときだけ、SSE 層を分けて" +
			"pub/sub で繋ぐ（EXP-38）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

// --- 最小の SSE クライアント（data 行を数える）---

type sseClient struct {
	t      *testing.T
	url    string
	tenant string
	cancel context.CancelFunc
	events int64
}

func newSSEClient(t *testing.T, url, tenant string) *sseClient {
	return &sseClient{t: t, url: url, tenant: tenant}
}

func (c *sseClient) run() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	req.Header.Set("X-Tenant", c.tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			atomic.AddInt64(&c.events, 1)
		}
	}
}

func (c *sseClient) stop() {
	if c.cancel != nil {
		c.cancel()
	}
}
func (c *sseClient) count() int { return int(atomic.LoadInt64(&c.events)) }
