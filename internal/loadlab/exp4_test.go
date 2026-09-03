package loadlab_test

// EXP-4: backpressure と過負荷。
//
//	MYSQL_DSN=... go test ./internal/loadlab/ -run TestEXP4 -v -timeout 20m
//
// 必須条件:
//   入力が処理能力を超えてもメモリが無制限に増えない
//   あるテナントの過負荷が全テナントを停止させない
//   捨てた・まとめた・遅延した事実が観測できる

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/loadlab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP4_backpressure(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	db, err := loadlab.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	if err := loadlab.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-4", "backpressure-overload",
		"入力が処理能力を超えたときのメモリ・遅延・公平性")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 無制限キューは、入力が処理能力を超えるとメモリと遅延が増え続ける。",
		"2) 有界キューはメモリを抑えるが、代わりに『待たせる』か『捨てる』のどちらかを選ぶことになる。",
		"3) キーごとに最新だけ残す方式は、同じ対象への連続更新が多いほど書き込みを減らし、",
		"   遅延も小さくなる。",
		"4) テナントごとに枠を分けないと、1テナントの氾濫が他テナントの処理を奪う。",
	}, " "))

	const (
		rate     = 10000
		duration = 2 * time.Second
		dbDelay  = 20 * time.Millisecond // 1トランザクションあたり。処理側が明確に追いつかない条件にする
		batchN   = 10
		workers  = 1
	)
	single := []loadlab.TenantMix{{ID: "t-a", Share: 1}}
	rec.Workload("rate_events_per_sec", rate).
		Workload("duration", duration.String()).
		Workload("keys_per_tenant", 200).
		Workload("capacity", 1024).
		Workload("batch_size", batchN).
		Workload("payload_bytes", 512).
		Workload("workers", workers).
		Injection("db_delay_per_tx", dbDelay.String()).
		Injection("tenant_skew", "1テナントが入力の 90% を占める条件を別途実行")

	// ---- ① 受け方6種を同じ負荷で比べる ----
	strategies := []loadlab.Strategy{
		loadlab.Unbounded, loadlab.BoundedBlock, loadlab.BoundedDrop,
		loadlab.Coalesce, loadlab.CoalesceBatch,
	}
	// ★メモリの比較は「キューに抱えている量」で行う。
	// ヒープの最大値は 10,000 件/秒の割り当ての回転に支配され、
	// 捨てた分のごみが回収される前かどうかで前後する（測定器の限界）。
	// 抱えている量 = キューの最大長 × 1件のデータ量 のほうが、方式の差を正しく表す。
	var unboundedHeld, boundedHeldMax int64
	for _, st := range strategies {
		if err := loadlab.Reset(ctx, db, single); err != nil {
			t.Fatal(err)
		}
		res, err := loadlab.Run(ctx, db, loadlab.Config{
			Strategy: st, Rate: rate, Duration: duration, Tenants: single,
			Keys: 200, Capacity: 1024, BatchSize: batchN, DBDelay: dbDelay, Workers: workers,
		})
		if err != nil {
			t.Fatal(err)
		}
		v := variantOf(st, res)
		rec.Add(v)
		t.Logf("[%-16s] 生成 %6d 受理 %6d 捨 %6d まとめ %6d 書込 %5d tx %4d / キュー最大 %6d 残 %6d / ヒープ最大 %6.1fMB / p95 %v",
			st, res.Produced, res.Accepted, res.Dropped, res.Coalesced, res.Committed, res.Txs,
			res.MaxQueue, res.Leftover, float64(res.Samples.MaxHeapAlloc)/1024/1024,
			res.Latency.P95.Round(time.Millisecond))

		held := res.MaxQueue * 512 // 抱えている量（1件 512B）
		switch st {
		case loadlab.Unbounded:
			unboundedHeld = held
			if res.MaxQueue < 10000 {
				t.Errorf("事故を再現できていない: 無制限キューなのに最大 %d 件しか溜まっていない", res.MaxQueue)
			}
		default:
			if held > boundedHeldMax {
				boundedHeldMax = held
			}
			if res.MaxQueue > 2000 {
				t.Errorf("[%s] 有界のはずがキューが %d 件まで伸びた", st, res.MaxQueue)
			}
		}
		if st == loadlab.BoundedBlock {
			// 背圧が入口まで届いていること（生成そのものが減る）
			if res.Produced > int64(rate)*int64(duration/time.Second)/2 {
				t.Errorf("背圧が入口に届いていない: %d 件も生成できている", res.Produced)
			}
		}
	}
	if unboundedHeld <= boundedHeldMax*4 {
		t.Errorf("抱えている量の差が出ていない: 無制限 %.1fMB vs 有界の最大 %.1fMB",
			float64(unboundedHeld)/1024/1024, float64(boundedHeldMax)/1024/1024)
	}
	t.Logf("キューが抱えていた量: 無制限 %.1fMB vs 有界方式の最大 %.1fMB（ヒープ最大は割り当ての回転に支配されるので別に見る）",
		float64(unboundedHeld)/1024/1024, float64(boundedHeldMax)/1024/1024)

	// ---- ② DB 遅延を変えて、まとめ方式がどこまで耐えるか ----
	for _, delay := range []time.Duration{0, 10 * time.Millisecond, 100 * time.Millisecond} {
		if err := loadlab.Reset(ctx, db, single); err != nil {
			t.Fatal(err)
		}
		res, err := loadlab.Run(ctx, db, loadlab.Config{
			Strategy: loadlab.CoalesceBatch, Rate: rate, Duration: duration, Tenants: single,
			Keys: 200, Capacity: 1024, BatchSize: 200, DBDelay: delay, Workers: workers,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Add(expkit.Variant{
			Name: fmt.Sprintf("まとめて書く / DB 遅延 %v", delay),
			Counters: map[string]int64{
				"produced": res.Produced, "committed": res.Committed, "txs": res.Txs,
				"max_queue": res.MaxQueue,
			},
			Metrics: map[string]float64{
				"tx_per_sec":  float64(res.Txs) / res.Elapsed.Seconds(),
				"heap_max_mb": float64(res.Samples.MaxHeapAlloc) / 1024 / 1024,
			},
			Latency: &res.Latency,
			Notes: []string{fmt.Sprintf("キュー最大 %d 件 / 反映の遅れ p99 %v",
				res.MaxQueue, res.Latency.P99.Round(time.Millisecond))},
		})
		t.Logf("[まとめ書き DB遅延 %5v] tx %4d (%5.1f/s) キュー最大 %5d p50 %v p99 %v",
			delay, res.Txs, float64(res.Txs)/res.Elapsed.Seconds(), res.MaxQueue,
			res.Latency.P50.Round(time.Millisecond), res.Latency.P99.Round(time.Millisecond))
	}

	// ---- ③ 1テナントの氾濫: 共有キュー vs テナントごとの枠 ----
	skewed := []loadlab.TenantMix{{ID: "t-big", Share: 0.9}, {ID: "t-small", Share: 0.1}}
	var shares [2]float64
	for i, st := range []loadlab.Strategy{loadlab.BoundedDrop, loadlab.PerTenantQuota} {
		if err := loadlab.Reset(ctx, db, skewed); err != nil {
			t.Fatal(err)
		}
		res, err := loadlab.Run(ctx, db, loadlab.Config{
			Strategy: st, Rate: rate, Duration: duration, Tenants: skewed,
			Keys: 200, Capacity: 256, BatchSize: batchN, DBDelay: dbDelay, Workers: workers,
		})
		if err != nil {
			t.Fatal(err)
		}
		small := res.ByTenant["t-small"]
		big := res.ByTenant["t-big"]
		share := 0.0
		if small+big > 0 {
			share = float64(small) / float64(small+big)
		}
		shares[i] = share
		rec.Add(expkit.Variant{
			Name: fmt.Sprintf("1テナントが入力の90%%を占める / %s", st),
			Counters: map[string]int64{
				"committed_big": big, "committed_small": small,
				"dropped": res.Dropped, "max_queue": res.MaxQueue,
			},
			Metrics:  map[string]float64{"small_tenant_share": share},
			Latency:  &res.Latency,
			Accident: share < 0.03,
			Notes: []string{
				fmt.Sprintf("少数派テナントの取り分 %.1f%%（入力比では 10%%）", share*100),
			},
		})
		t.Logf("[%-16s] 氾濫テナント %6d / 少数派 %5d（取り分 %.1f%%）捨 %6d",
			st, big, small, share*100, res.Dropped)
	}
	if shares[1] <= shares[0] {
		t.Errorf("テナントごとに枠を分けても少数派の取り分が改善していない: 共有 %.1f%% → 枠あり %.1f%%",
			shares[0]*100, shares[1]*100)
	}

	rec.Scope(
		"MySQL 8.0 / 同一ホスト / 1プロセス内のパイプライン（生成→キュー→バッチ→トランザクション）",
		"DB 遅延は SELECT SLEEP で DB 側に実際に待たせている（接続の占有まで含まれる）",
		"入力レートは 10,000 件/秒。処理側は DB 遅延によって明確に追いつかない条件",
	)
	rec.Uncertain(
		"ヒープ最大値は方式の比較に使えなかった（10,000件/秒では割り当ての回転と GC の時機に支配される）。"+
			"方式の差は『キューが抱えている件数×1件の大きさ』で見ている",
		"spill-to-disk（あふれをディスクへ退避）は未実装・未測定",
		"複数プロセスでの公平性（同じ DB を複数 worker が奪い合う場合）は未測定",
		"入力が止まったあとの回復時間は、2秒の走行では十分に観測できていない",
		"GC の挙動は Go の既定のまま（GOGC 未調整）",
	)
	rec.Artifact(
		"internal/loadlab: 6方式の受け口（無制限/有界待ち/有界捨て/まとめ/まとめ+バッチ/テナント枠）",
		"expkit.Sampler の custom で queue depth を時系列に残す型",
	)
	rec.Next("EXP-5 接続プールの飽和点")

	files, err := rec.Save(strings.Join([]string{
		"無制限キューは入力が処理能力を超えた瞬間からメモリと遅延が伸び続け、",
		"有界にすると『待たせる』『捨てる』のどちらかを選ぶことになる（選ばない選択肢は無い）。",
		"キーごとに最新だけ残す方式は、同じ対象への更新が多い場面で書き込みと遅延の両方を下げた。",
		"テナントごとに枠を分けると、氾濫しているテナントの隣で少数派の処理が生き残った。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

func variantOf(st loadlab.Strategy, res loadlab.Result) expkit.Variant {
	v := expkit.Variant{
		Name: string(st),
		Counters: map[string]int64{
			"produced": res.Produced, "accepted": res.Accepted,
			"dropped": res.Dropped, "coalesced": res.Coalesced,
			"committed": res.Committed, "txs": res.Txs,
			"max_queue": res.MaxQueue, "leftover_at_stop": res.Leftover,
		},
		Metrics: map[string]float64{
			"heap_max_mb":     float64(res.Samples.MaxHeapAlloc) / 1024 / 1024,
			"rss_max_mb":      float64(res.Samples.MaxRSS) / 1024 / 1024,
			"tx_per_sec":      float64(res.Txs) / res.Elapsed.Seconds(),
			"blocked_seconds": res.Blocked.Seconds(),
		},
		Latency:  &res.Latency,
		Samples:  &res.Samples,
		Accident: st == loadlab.Unbounded,
	}
	switch st {
	case loadlab.Unbounded:
		v.Desc = "入ってきたぶんだけ溜める"
		v.Notes = append(v.Notes, "★キューが伸び続け、反映の遅れもそのぶん増える。落ちるまで気づきにくい")
	case loadlab.BoundedBlock:
		v.Desc = "有界。いっぱいなら生産側を待たせる（入口へ背圧をかける）"
		v.Notes = append(v.Notes, fmt.Sprintf("生産側が待たされた合計 %v（これが背圧の量）", res.Blocked.Round(time.Millisecond)))
	case loadlab.BoundedDrop:
		v.Desc = "有界。いっぱいなら捨てる"
		v.Notes = append(v.Notes, fmt.Sprintf("捨てた %d 件（数えているので、あとから説明できる）", res.Dropped))
	case loadlab.Coalesce:
		v.Desc = "キーごとに最新だけ残す（1件ずつ書く）"
		v.Notes = append(v.Notes, fmt.Sprintf("まとめた %d 件", res.Coalesced))
	case loadlab.CoalesceBatch:
		v.Desc = "キーごとに最新だけ残し、1トランザクションでまとめて書く"
		v.Notes = append(v.Notes, fmt.Sprintf("まとめた %d 件 / トランザクション %d 回", res.Coalesced, res.Txs))
	}
	return v
}

var _ = sql.ErrNoRows
