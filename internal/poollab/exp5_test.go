package poollab_test

// EXP-5: 接続プールの飽和点。
//
//	MYSQL_DSN=... go test ./internal/poollab/ -run TestEXP5 -v -timeout 20m
//
// 「MaxOpenConns をいくつにするか」を勘で決めないために、
// **どこで頭打ちになり、そのとき何が起きるか**を測る。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/poollab"
)

func TestEXP5_接続プールの飽和点(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	db := mysqltest.Raw(t)
	ctx := context.Background()
	if err := poollab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-5", "connection-pool-saturation",
		"同時実行数と MaxOpenConns を振って、スループットが伸びなくなる点を探す")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが線形に伸びる。",
		"2) その点は MaxOpenConns 付近にあり、超えた分は database/sql の待ち行列（WaitCount）に現れる。",
		"3) MaxOpenConns を増やしても、CPU やディスクが先に頭打ちになれば伸びない。",
		"4) 遅い処理（DB 側で待つトランザクション）は、その時間ぶん接続を占有し、",
		"   同時実行数に関係なく MaxOpenConns / 処理時間 で上限が決まる。",
	}, " "))
	rec.Workload("duration_per_point", "1.5s").
		Workload("table_rows", 64).
		Injection("none", "故障注入なし。負荷と設定だけを振る")

	// ---- ① 同時実行数を振る（MaxOpenConns = 8 固定） ----
	const fixedMaxOpen = 8
	var (
		best      float64
		bestConc  int
		sweepRows []string
	)
	for _, conc := range []int{1, 2, 4, 8, 16, 32, 64} {
		res := run(t, ctx, dsn, poollab.Config{
			Mode: poollab.PointRead, MaxOpen: fixedMaxOpen, MaxIdle: fixedMaxOpen,
			Concurrency: conc, Duration: 1500 * time.Millisecond,
		})
		if res.OpsPerSec > best {
			best, bestConc = res.OpsPerSec, conc
		}
		sweepRows = append(sweepRows, fmt.Sprintf(
			"並列 %3d: %8.0f ops/s p50=%-8v p99=%-9v 待ち %5d 回 / 合計 %v・サーバ接続 最大 %d",
			conc, res.OpsPerSec, res.Latency.P50.Round(time.Microsecond),
			res.Latency.P99.Round(time.Microsecond), res.WaitCount,
			res.WaitDuration.Round(time.Millisecond), res.MaxThreadsConnected))
		t.Logf("[並列 %3d / MaxOpen %d] %8.0f ops/s  p50 %-8v p99 %-9v 待ち %5d 回 (%v)",
			conc, fixedMaxOpen, res.OpsPerSec, res.Latency.P50.Round(time.Microsecond),
			res.Latency.P99.Round(time.Microsecond), res.WaitCount,
			res.WaitDuration.Round(time.Millisecond))
		rec.Add(expkit.Variant{
			Name: fmt.Sprintf("主キー読み / 並列 %d / MaxOpen %d", conc, fixedMaxOpen),
			Counters: map[string]int64{
				"ops": res.Ops, "wait_count": res.WaitCount,
				"server_threads_connected_max": int64(res.MaxThreadsConnected),
				"server_threads_running_max":   int64(res.MaxThreadsRunning),
				"new_connections":              res.NewConnections,
			},
			Metrics: map[string]float64{
				"ops_per_sec":      res.OpsPerSec,
				"wait_duration_ms": float64(res.WaitDuration.Milliseconds()),
			},
			Latency: &res.Latency,
		})
	}
	t.Logf("スループットの頂点: 並列 %d で %.0f ops/s（MaxOpen %d）", bestConc, best, fixedMaxOpen)

	// ---- ② MaxOpenConns を振る（並列 32 固定） ----
	var openRows []string
	prev := 0.0
	kneeAt := 0
	for _, maxOpen := range []int{1, 2, 4, 8, 16, 32} {
		res := run(t, ctx, dsn, poollab.Config{
			Mode: poollab.PointRead, MaxOpen: maxOpen, MaxIdle: maxOpen,
			Concurrency: 32, Duration: 1500 * time.Millisecond,
		})
		gain := 0.0
		if prev > 0 {
			gain = (res.OpsPerSec - prev) / prev * 100
			if gain < 10 && kneeAt == 0 {
				kneeAt = maxOpen
			}
		}
		prev = res.OpsPerSec
		openRows = append(openRows, fmt.Sprintf(
			"MaxOpen %2d: %8.0f ops/s（前段からの伸び %+5.1f%%）p99=%-9v 待ち %5d 回",
			maxOpen, res.OpsPerSec, gain, res.Latency.P99.Round(time.Microsecond), res.WaitCount))
		t.Logf("[MaxOpen %2d / 並列 32] %8.0f ops/s（伸び %+5.1f%%）p99 %-9v 待ち %5d 回",
			maxOpen, res.OpsPerSec, gain, res.Latency.P99.Round(time.Microsecond), res.WaitCount)
	}
	rec.Add(expkit.Variant{
		Name:     "MaxOpenConns を振る（並列 32 固定）",
		Desc:     "接続を増やしても伸びなくなる点を探す",
		Notes:    openRows,
		Counters: map[string]int64{"knee_max_open": int64(kneeAt)},
	})
	rec.Add(expkit.Variant{
		Name:     "同時実行数を振る（MaxOpen 8 固定）",
		Desc:     "頭打ちの位置と、超えた分がどこに現れるか",
		Notes:    sweepRows,
		Metrics:  map[string]float64{"peak_ops_per_sec": best},
		Counters: map[string]int64{"peak_concurrency": int64(bestConc)},
	})

	// ---- ③ MaxIdleConns の既定値（2）の影響 ----
	var idleRows []string
	for _, maxIdle := range []int{2, 16} {
		res := run(t, ctx, dsn, poollab.Config{
			Mode: poollab.PointRead, MaxOpen: 16, MaxIdle: maxIdle,
			Concurrency: 16, Duration: 1500 * time.Millisecond,
		})
		idleRows = append(idleRows, fmt.Sprintf(
			"MaxIdle %2d: %8.0f ops/s / 新規接続 %5d 本 / アイドル超過で閉じた %5d 本 / p99 %v",
			maxIdle, res.OpsPerSec, res.NewConnections, res.MaxIdleClosed,
			res.Latency.P99.Round(time.Microsecond)))
		t.Logf("[MaxIdle %2d / MaxOpen 16 / 並列 16] %8.0f ops/s 新規接続 %5d 本 閉じた %5d 本",
			maxIdle, res.OpsPerSec, res.NewConnections, res.MaxIdleClosed)
	}
	rec.Add(expkit.Variant{
		Name:  "MaxIdleConns の既定値（2）の影響",
		Desc:  "接続の張り直しがスループットに出るか",
		Notes: idleRows,
	})

	// ---- ④ 遅いトランザクションは接続を占有する ----
	slow := run(t, ctx, dsn, poollab.Config{
		Mode: poollab.SlowTx, MaxOpen: 4, MaxIdle: 4, Concurrency: 32,
		Duration: 3 * time.Second, SlowFor: 100 * time.Millisecond,
	})
	theoretical := float64(4) / 0.1 // MaxOpen / 1件あたりの時間
	rec.Add(expkit.Variant{
		Name: "DB 側で 100ms 待つトランザクション / MaxOpen 4 / 並列 32",
		Desc: "遅い処理は、その時間ぶん接続を占有する",
		Counters: map[string]int64{
			"ops": slow.Ops, "wait_count": slow.WaitCount,
			"server_threads_connected_max": int64(slow.MaxThreadsConnected),
		},
		Metrics: map[string]float64{
			"ops_per_sec":      slow.OpsPerSec,
			"theoretical_max":  theoretical,
			"wait_duration_ms": float64(slow.WaitDuration.Milliseconds()),
		},
		Latency: &slow.Latency,
		Notes: []string{
			fmt.Sprintf("実測 %.1f ops/s（理論上限 MaxOpen/処理時間 = %.0f ops/s）", slow.OpsPerSec, theoretical),
			fmt.Sprintf("待ち %d 回・合計 %v。並列を増やしても、この上限は動かない",
				slow.WaitCount, slow.WaitDuration.Round(time.Millisecond)),
			"★上限を決めるのは同時実行数ではなく『接続数 ÷ 1件あたりの占有時間』",
		},
	})
	t.Logf("[遅いtx 100ms / MaxOpen 4 / 並列 32] %.1f ops/s（理論上限 %.0f）待ち %d 回",
		slow.OpsPerSec, theoretical, slow.WaitCount)
	if slow.OpsPerSec > theoretical*1.5 {
		t.Errorf("理論上限を大きく超えている（測定がおかしい）: %.1f > %.0f", slow.OpsPerSec, theoretical)
	}
	if slow.WaitCount == 0 {
		t.Errorf("並列 32 に対して接続 4 本なのに、待ちが発生していない（測定がおかしい）")
	}

	rec.Scope(
		"MySQL 8.0 / 同一ホスト / 4 CPU のコンテナ。DB とアプリが同じ機械にある",
		"表は 64 行・主キー読みが中心。実データの分布や索引の効き方は含まない",
		"1条件 1.5〜3 秒の測定。長時間の安定性は見ていない",
	)
	rec.Uncertain(
		"ネットワークを跨ぐ構成では、往復が支配的になり飽和点が変わる",
		"RDS Proxy などの接続集約を挟んだ場合は docs/rds-proxy.md（LIVE_ENV_REQUIRED）",
		"同一ホストなので、アプリと MySQL が CPU を奪い合っている。専用機での値とは異なる",
		"接続確立のコスト（TLS）は測っていない（TLS 無効の接続）",
	)
	rec.Artifact(
		"internal/poollab: 同時実行数・MaxOpen・MaxIdle・処理時間を振って、" +
			"database/sql と MySQL の両方の数字を同時に取る測定器",
	)
	rec.Next("EXP-7 データ偏り付きの実行計画")

	files, err := rec.Save(strings.Join([]string{
		fmt.Sprintf("この環境（4 CPU・同一ホスト）では、主キー読みの頂点は並列 %d 付近で %.0f ops/s だった。", bestConc, best),
		"それを超えると database/sql の待ち行列に積まれ、スループットは伸びずに遅延だけが伸びる。",
		"遅いトランザクションでは上限が『接続数 ÷ 占有時間』で決まり、並列を増やしても動かない。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

func run(t *testing.T, ctx context.Context, dsn string, cfg poollab.Config) poollab.Result {
	t.Helper()
	res, err := poollab.Run(ctx, dsn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops == 0 {
		t.Fatalf("1件も処理できていない: %+v", cfg)
	}
	return res
}

var _ = sql.ErrNoRows
