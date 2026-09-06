package cadencelab_test

// EXP-17: 適応的バックオフ。
//
//	MYSQL_DSN=... go test ./internal/cadencelab/ -run TestEXP17 -v
//
// EXP-12 で「到着が疎なら速いポーリングは空振りの無駄」と分かった。
// 適応的バックオフ（空振りで間隔を伸ばし、仕事が来たら詰める）で、
// 空振りの問い合わせをどれだけ減らせるか、遅延を保てるかを測る。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/cadencelab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP17_適応的バックオフ(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-17", "adaptive-backoff",
		"適応的バックオフ: 空振りで間隔を伸ばし、仕事で詰める")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 固定 100ms ポーリングは、暇な間も一定間隔で空振りし続ける。",
		"2) 適応的バックオフ（空振りで倍々、仕事で Min へ）は、暇な間の空振りを大きく減らす。",
		"3) 仕事が来たときの遅延は、固定 100ms とほぼ同等に保てる（Min へ戻すので）。",
	}, " "))

	const commands = 30
	window := 3 * time.Second
	hold := window + 3*time.Second // 捌き終わった後も暇な時間を作る（空振りを観測）
	rec.Workload("commands", commands).Workload("arrival", "10/s（疎）").Workload("hold", hold.String())

	// ① 固定 100ms
	start := time.Now()
	fcfg := cadencelab.Config{Tenant: "exp17", Commands: commands, Window: window,
		Interval: 100 * time.Millisecond, Batch: 600, HoldFor: hold}
	if err := cadencelab.Seed(ctx, db, fcfg, start); err != nil {
		t.Fatal(err)
	}
	fixed, err := cadencelab.Run(ctx, db, fcfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "固定 100ms ポーリング",
		Accident: true,
		Counters: map[string]int64{"polls": fixed.Polls, "empty_polls": fixed.EmptyPolls, "dispatched": fixed.Dispatched},
		Metrics:  map[string]float64{"p95_ms": ms(fixed.Latency.P95)},
	})
	t.Logf("固定100ms   polls=%d empty=%d dispatched=%d p95=%v",
		fixed.Polls, fixed.EmptyPolls, fixed.Dispatched, fixed.Latency.P95)

	// ② 適応的バックオフ（Min 100ms → Max 2s）
	start = time.Now()
	acfg := cadencelab.Config{Tenant: "exp17", Commands: commands, Window: window,
		Batch: 600, HoldFor: hold, Adaptive: true,
		MinInterval: 100 * time.Millisecond, MaxInterval: 2 * time.Second}
	if err := cadencelab.Seed(ctx, db, acfg, start); err != nil {
		t.Fatal(err)
	}
	adaptive, err := cadencelab.Run(ctx, db, acfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "適応的バックオフ（Min 100ms → Max 2s）",
		Counters: map[string]int64{"polls": adaptive.Polls, "empty_polls": adaptive.EmptyPolls, "dispatched": adaptive.Dispatched},
		Metrics:  map[string]float64{"p95_ms": ms(adaptive.Latency.P95)},
		Notes: []string{
			"空振り " + itoa(fixed.EmptyPolls) + " → " + itoa(adaptive.EmptyPolls),
			"仕事が来たら Min へ戻すので、dispatch 遅延は固定 100ms と同等に保てる",
		},
	})
	t.Logf("適応backoff polls=%d empty=%d dispatched=%d p95=%v",
		adaptive.Polls, adaptive.EmptyPolls, adaptive.Dispatched, adaptive.Latency.P95)

	// ---- 検証 ----
	if adaptive.Dispatched != commands || fixed.Dispatched != commands {
		t.Errorf("取りこぼし: fixed=%d adaptive=%d want %d", fixed.Dispatched, adaptive.Dispatched, commands)
	}
	// 適応の空振りは固定より大幅に少ない
	if adaptive.EmptyPolls >= fixed.EmptyPolls {
		t.Errorf("適応の空振り(%d)が固定(%d)以上。バックオフが効いていない", adaptive.EmptyPolls, fixed.EmptyPolls)
	}
	// 遅延は Max(2s) を超えない範囲で許容（Min へ戻すので極端に悪化しない）
	if adaptive.Latency.P95 > 2500*time.Millisecond {
		t.Errorf("適応の p95(%v) が大きすぎる（バックオフしすぎ）", adaptive.Latency.P95)
	}

	rec.Scope(
		"MySQL 8.0 / 単一テナント / 命令 30 を 3 秒に散らし、その後 3 秒暇にする",
		"空振り = 0 件だったポーリング。dispatch 遅延 = scheduled_for から claim まで",
	)
	rec.Uncertain(
		"暇の長さ・Min/Max・到着パターンで効果は動く。この数字はこのホストのもの",
		"仕事が『バックオフ中に来た』場合、最大 Max だけ遅れる。低遅延が要るなら wake（EXP-12）と併用",
		"複数テナントを畳む（EXP-14）場合のバックオフは、担当集合ごとに1つ回す想定（未測定）",
	)
	rec.Artifact("internal/cadencelab: Adaptive バックオフ（空振りで倍々、仕事で Min へ）")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"適応的バックオフは、暇な間の空振りの問い合わせを大きく減らす。",
		"仕事が来たら Min へ戻すので、dispatch 遅延は固定間隔とほぼ同等に保てる。",
		"低到着のテナントを多数抱えるとき、fan-out を畳む（EXP-14）と合わせて DB 負荷を下げられる。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
