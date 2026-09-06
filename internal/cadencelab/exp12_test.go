package cadencelab_test

// EXP-12: ワーカーのポーリング頻度。
//
//	MYSQL_DSN=... go test ./internal/cadencelab/ -run TestEXP12 -v
//
// スケジュール／命令／実績を DB で表したとき、ワーカーはどれくらいの頻度で
// DB を引き命令を出すのが良いか。ポーリング間隔を振って、
// 「dispatch 遅延」と「DB への問い合わせ負荷」のトレードオフを実測する。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/cadencelab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP12_ポーリング頻度(t *testing.T) {
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
	if err := cadencelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-12", "worker-cadence",
		"ワーカーのポーリング頻度: dispatch 遅延と DB 負荷のトレードオフ")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 十分な batch で毎回 due を捌けるなら、dispatch 遅延 p95 はほぼポーリング間隔になる。",
		"2) 間隔を短くすると遅延は下がるが、問い合わせ回数は増える。",
		"3) 到着レートが低いのに速く引くと、空振りの問い合わせ回数が増える（無駄）。遅延は間隔で決まる。",
		"4) 疎な到着では、signal 型の wake が空振りを無くし、ポーリングより低遅延で捌ける。",
		"5) 別の制約: batch/interval が到着レート未満だと、間隔と無関係に backlog が溜まり遅延が発散する。",
	}, " "))

	const window = 3 * time.Second
	intervals := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond,
		500 * time.Millisecond, 1 * time.Second}

	// ---- 高到着（200/s）: batch を十分にして遅延≈間隔を見る ----
	high := runSweep(t, ctx, rec, db, "高到着 200/s", 600, window, intervals, 600)
	// ---- 低到着（10/s）: 速く引くと空振りが増えることを見る ----
	low := runSweepHold(t, ctx, rec, db, "低到着 10/s", 30, window, intervals, 600, window+2*time.Second)

	// ---- signal 型 wake（疎な到着で比較）----
	start := time.Now()
	wcfg := cadencelab.Config{Tenant: "exp12", Commands: 30, Window: window, Batch: 600, Wake: true}
	if err := cadencelab.Seed(ctx, db, wcfg, start); err != nil {
		t.Fatal(err)
	}
	wake, err := cadencelab.RunWake(ctx, db, wcfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "wake（低到着10/s・signal 型。due 到来で起こす）",
		Desc:     "ポーリングと違い空振りしない。疎な到着で低遅延・空振りゼロを狙う",
		Counters: map[string]int64{"dispatched": wake.Dispatched, "polls": wake.Polls, "empty_polls": wake.EmptyPolls},
		Metrics:  metricsOf(wake),
		Latency:  &wake.Latency,
	})
	t.Logf("wake(signal) dispatched=%d polls=%d empty=%d p95=%v",
		wake.Dispatched, wake.Polls, wake.EmptyPolls, wake.Latency.P95)

	// ---- 制約: batch/interval < 到着レート で backlog 発散（事故として記録）----
	under := runOne(t, ctx, db, cadencelab.Config{
		Tenant: "exp12", Commands: 600, Window: window, Interval: 500 * time.Millisecond, Batch: 50})
	rec.Add(expkit.Variant{
		Name:     "★制約: batch 50 / interval 500ms（処理能力 100/s < 到着 200/s）",
		Desc:     "batch/interval が到着レート未満だと、間隔と無関係に backlog が溜まる",
		Accident: true,
		Counters: map[string]int64{"dispatched": under.Dispatched, "polls": under.Polls},
		Metrics:  metricsOf(under),
		Latency:  &under.Latency,
		Notes: []string{
			fmt.Sprintf("処理能力 = batch/interval = 100/s。到着 200/s に追いつかず p95=%v まで発散", under.Latency.P95),
			"→ ポーリング間隔の前に、batch/interval ≥ 到着レート を満たすこと",
		},
	})
	t.Logf("under-prov   dispatched=%d/600 p95=%v", under.Dispatched, under.Latency.P95)

	// ---- 検証 ----
	// 高到着（batch 十分）では全件捌け、遅延がほぼ間隔になる（100ms < 1s）
	for _, p := range high {
		if p.res.Dispatched != 600 {
			t.Errorf("高到着 interval=%v で取りこぼし: %d", p.interval, p.res.Dispatched)
		}
	}
	if high[0].res.Latency.P95 >= high[len(high)-1].res.Latency.P95 {
		t.Errorf("遅延が間隔に単調でない: 100ms=%v 1s=%v", high[0].res.Latency.P95, high[len(high)-1].res.Latency.P95)
	}
	// 低到着では、速い間隔ほど空振りの問い合わせ回数が多い（無駄）
	if low[0].res.EmptyPolls <= low[len(low)-1].res.EmptyPolls {
		t.Errorf("低到着で空振り回数が間隔に単調でない: 100ms=%d 1s=%d",
			low[0].res.EmptyPolls, low[len(low)-1].res.EmptyPolls)
	}
	// wake（signal型）は、低到着で空振りをほぼ無くし、100ms ポーリングより低遅延
	lowFast := low[0].res // 低到着 100ms
	if wake.EmptyPolls > lowFast.EmptyPolls {
		t.Errorf("wake の空振り(%d) が 100ms ポーリング(%d) より多い（無くすはず）", wake.EmptyPolls, lowFast.EmptyPolls)
	}
	if wake.Latency.P95 >= lowFast.Latency.P95 {
		t.Errorf("wake の p95(%v) が低到着100msポーリング(%v) 以上（低遅延のはず）", wake.Latency.P95, lowFast.Latency.P95)
	}

	rec.Scope(
		"MySQL 8.0 / 単一テナント・単一ワーカー（lease 前提）/ 命令を 3 秒に散らす",
		"dispatch 遅延 = scheduled_for から claim までの時間。ロボット側の実行時間は含まない",
		"claim は state 遷移（pending→dispatched）。複数ワーカーは対象外（1テナント1ワーカー）",
	)
	rec.Uncertain(
		"最適間隔は到着レート・batch・DB の忙しさで動く。この数字はこのホストのもの",
		"複数テナント同時のポーリングは未測定（jitter で山をずらす実装だけ）",
		"wake は in-process の producer を仮定。別プロセス跨ぎの push は別（MySQL に LISTEN/NOTIFY は無い）",
		"ロボット側の実行遅延・失敗・OUTCOME_UNKNOWN は cmd_result で観測する設計だが本実験の対象外",
	)
	rec.Artifact(
		"internal/cadencelab: スケジュール/命令/実績のスキーマ（schema.sql）とポーリングワーカー",
		"docs/scheduling.md: 設計・テナントワーカーの制約・最適間隔の決め方",
	)
	rec.Next("なし（EXP-1..12）")

	files, err := rec.Save(strings.Join([]string{
		"ポーリング間隔は「許容する dispatch 遅延」で決める（batch を十分にすれば p95 ≈ 間隔）。",
		"ただし先に batch/interval ≥ 到着レート を満たすこと（さもないと間隔と無関係に発散）。",
		"到着が低いなら速く引くのは空振りの無駄。jitter で山をずらし、wake で低遅延と低負荷を両立する。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

type point struct {
	interval time.Duration
	res      cadencelab.Result
}

func runSweep(t *testing.T, ctx context.Context, rec *expkit.Recorder, db *sql.DB,
	label string, commands int, window time.Duration, intervals []time.Duration, batch int) []point {
	t.Helper()
	var pts []point
	for _, iv := range intervals {
		res := runOne(t, ctx, db, cadencelab.Config{
			Tenant: "exp12", Commands: commands, Window: window, Interval: iv, Batch: batch})
		pts = append(pts, point{iv, res})
		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("%s / interval=%v", label, iv),
			Counters: map[string]int64{"dispatched": res.Dispatched, "polls": res.Polls, "empty_polls": res.EmptyPolls},
			Metrics:  metricsOf(res),
			Latency:  &res.Latency,
		})
		t.Logf("%-14s interval=%-6v dispatched=%d polls=%d empty=%.0f%% p95=%v",
			label, iv, res.Dispatched, res.Polls, res.EmptyRatio*100, res.Latency.P95)
	}
	return pts
}

func runSweepHold(t *testing.T, ctx context.Context, rec *expkit.Recorder, db *sql.DB,
	label string, commands int, window time.Duration, intervals []time.Duration, batch int, hold time.Duration) []point {
	t.Helper()
	var pts []point
	for _, iv := range intervals {
		res := runOne(t, ctx, db, cadencelab.Config{
			Tenant: "exp12", Commands: commands, Window: window, Interval: iv, Batch: batch, HoldFor: hold})
		pts = append(pts, point{iv, res})
		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("%s / interval=%v（空振りの無駄）", label, iv),
			Counters: map[string]int64{"dispatched": res.Dispatched, "polls": res.Polls, "empty_polls": res.EmptyPolls},
			Metrics:  metricsOf(res),
			Latency:  &res.Latency,
		})
		t.Logf("%-14s interval=%-6v dispatched=%d polls=%d empty=%d p95=%v",
			label, iv, res.Dispatched, res.Polls, res.EmptyPolls, res.Latency.P95)
	}
	return pts
}

func runOne(t *testing.T, ctx context.Context, db *sql.DB, cfg cadencelab.Config) cadencelab.Result {
	t.Helper()
	start := time.Now()
	if err := cadencelab.Seed(ctx, db, cfg, start); err != nil {
		t.Fatal(err)
	}
	res, err := cadencelab.Run(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func metricsOf(r cadencelab.Result) map[string]float64 {
	return map[string]float64{
		"latency_p50_ms": ms(r.Latency.P50), "latency_p95_ms": ms(r.Latency.P95),
		"latency_p99_ms": ms(r.Latency.P99), "queries_per_sec": r.QueriesPerSec,
		"empty_ratio": r.EmptyRatio,
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
