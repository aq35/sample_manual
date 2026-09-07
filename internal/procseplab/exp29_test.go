package procseplab_test

// EXP-29: Web と Worker を同じ接続プールで動かすと、Worker が待たされるのか。分けると守れるのか。
//
//	MYSQL_DSN=... go test ./internal/procseplab/ -run TestEXP29 -v
//
// Web=外から来るバースト・重め、Worker=定期ポーリング・軽い・遅延に敏感。総接続数は同じにして、
// 「1つのプールを共有」vs「役割ごとに取り分を分ける」で、Worker のレイテンシ p95 を比べる。

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/procseplab"
)

func TestEXP29_WebとWorkerの分離(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)

	rec := expkit.NewRecorder("EXP-29", "web-worker-pool-separation",
		"Web と Worker で接続プールを共有 vs 分離したときの Worker レイテンシ")
	// env 取得用に1本開ける
	envDB, err := procseplab.Open(dsn, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = envDB.Close() }()
	if err := envDB.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	rec.Env(expkit.CaptureEnv(ctx, envDB))
	rec.Freeze(
		"1) 総接続数を同じにしても、1プールを Web と Worker で共有すると、Web のバーストが接続を" +
			"占有し、Worker の軽い問い合わせが acquire 待ちで p95 が跳ねる。 " +
			"2) プールを役割ごとに分ける（Worker に専用の取り分）と、Worker は Web の影響を受けず p95 が低い。 " +
			"3) 総接続数は変えていないので、効いているのは『分けたこと』そのもの。")

	const (
		webConns    = 8  // Web の取り分
		workerConns = 2  // Worker の取り分
		webGoros    = 16 // Web の同時リクエスト（プールより多い＝飽和）
		iters       = 100
	)
	webSleep := 50 * time.Millisecond
	rec.Workload("web_goroutines", webGoros).Workload("web_sleep_ms", 50).
		Workload("total_conns", webConns+workerConns)

	// ---- ① 共有: 1プール（総接続 = webConns+workerConns）を Web と Worker で共有 ----
	shared, err := procseplab.Open(dsn, webConns+workerConns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shared.Close() }()
	stop := procseplab.WebBurst(ctx, shared, webGoros, webSleep)
	time.Sleep(200 * time.Millisecond) // バーストを立ち上げる
	sharedLat := procseplab.WorkerLatency(ctx, shared, iters)
	stop()

	// ---- ② 分離: Web 用プールと Worker 専用プール（同じ総接続数）----
	webDB, err := procseplab.Open(dsn, webConns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = webDB.Close() }()
	workerDB, err := procseplab.Open(dsn, workerConns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = workerDB.Close() }()
	stop2 := procseplab.WebBurst(ctx, webDB, webGoros, webSleep)
	time.Sleep(200 * time.Millisecond)
	sepLat := procseplab.WorkerLatency(ctx, workerDB, iters)
	stop2()

	rec.Add(expkit.Variant{
		Name:     "共有プール: Web と Worker が同じプール（Worker が待たされる）",
		Accident: true,
		Metrics:  map[string]float64{"worker_p50_ms": ms(sharedLat.P50), "worker_p95_ms": ms(sharedLat.P95)},
		Notes:    []string{"Web バーストが接続を占有し、Worker の SELECT 1 が acquire 待ちになる"},
	})
	rec.Add(expkit.Variant{
		Name:    "分離プール: Worker 専用の取り分（Web の影響を受けない）",
		Metrics: map[string]float64{"worker_p50_ms": ms(sepLat.P50), "worker_p95_ms": ms(sepLat.P95)},
		Notes: []string{"共有 p95 " + sharedLat.P95.String() + " → 分離 p95 " + sepLat.P95.String() +
			"（総接続数は同じ。効いているのは分けたこと）"},
	})
	t.Logf("Worker p95: 共有=%v / 分離=%v（p50 共有=%v 分離=%v）",
		sharedLat.P95, sepLat.P95, sharedLat.P50, sepLat.P50)

	// ---- 検証 ----
	if sharedLat.P95 <= sepLat.P95 {
		t.Errorf("共有プールで Worker p95 が悪化していない: 共有=%v 分離=%v", sharedLat.P95, sepLat.P95)
	}
	if sharedLat.P95 < sepLat.P95*3 {
		t.Errorf("共有と分離の差が小さい（3倍未満）: 共有=%v 分離=%v", sharedLat.P95, sepLat.P95)
	}

	rec.Scope(
		"MySQL 8.0 / 総接続 10（Web 8・Worker 2）/ Web=SLEEP(50ms)×16並行 / Worker=SELECT 1×100",
		"レイテンシは Worker 側の『接続 acquire＋実行』。ここに待ちが乗る",
		"生の *sql.DB を直接使う（プールの振る舞いを見るため。repo 層は経由しない）",
	)
	rec.Uncertain(
		"絶対値はローカルのもの。Web の重さ・並行度・プール比で差は動く",
		"別プロセス/別コンテナに分ければ CPU・メモリ・障害も隔離できる（ここはプールの隔離のみ）",
		"読み取りをレプリカへ回す（EXP-20）とさらに primary の負担を分けられる",
	)
	rec.Artifact(
		"internal/procseplab: 共有/分離プールでの Worker レイテンシ測定",
		"docs/web-worker-split.md: Web と Worker の分離（接続予算は poolbudget）",
	)
	rec.Next("EXP-30 冗長化（複数レプリカ）でアプリはどうあるべきか")

	files, err := rec.Save(
		"Web と Worker は接続プールを分ける（総接続数が同じでも、共有すると Web バーストで Worker の" +
			"dispatch 遅延が跳ねる）。役割ごとに取り分を持てば Worker は Web の影響を受けない。" +
			"プロセス/コンテナを分ければ CPU・障害も隔離できる。接続予算の配分は poolbudget で。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
