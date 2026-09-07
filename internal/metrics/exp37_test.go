package metrics_test

// EXP-37: 可観測性。メトリクスのカーディナリティ（時系列の爆発）と記録オーバーヘッド。
//
//	go test ./internal/metrics/ -run TestEXP37 -v   （DB 不要）
//
// (1) tenant×status のような有界ラベルなら時系列は少ない。robot_id を混ぜると爆発する。
//     上限（cap）を設ければ、超過分は "__over__" に畳まれて時系列が有界に保たれる。
// (2) 記録（Inc/Observe）のコストは、DB 往復（EXP-31 で ~0.1ms〜）に比べて桁違いに安い。

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/metrics"
)

func TestEXP37_可観測性(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-37", "observability",
		"メトリクスのカーディナリティ制御と記録オーバーヘッド")
	rec.Env(expkit.CaptureEnv(ctx, nil)) // DB 不要
	rec.Freeze(
		"1) 有界ラベル(tenant 30 × status 6)なら時系列は 180 程度で収まる。 " +
			"2) robot_id を混ぜると時系列がリクエストの種類数まで爆発する。 " +
			"3) cap を設ければ、超過は __over__ に畳まれ、実系列数は cap 以下に保たれる。 " +
			"4) 記録コストは 1回あたりサブマイクロ秒。DB 往復に比べ無視できる。")

	const tenants, statuses = 30, 6

	// ---- ① 有界ラベル: tenant × status ----
	good := metrics.NewLabeled(10000)
	for i := 0; i < 100000; i++ {
		// tenant と status は独立に動かす（i%tenants と i%statuses は 6|30 で相関するため）
		good.Inc(fmt.Sprintf("t%02d", i%tenants), fmt.Sprintf("s%d", (i/tenants)%statuses))
	}
	rec.Add(expkit.Variant{
		Name:     "有界ラベル: tenant×status（robot_id を入れない）",
		Counters: map[string]int64{"series": int64(good.SeriesCount()), "over": good.Over()},
		Notes:    []string{"10万リクエストでも時系列は " + itoa(good.SeriesCount()) + "（=tenant×status）"},
	})
	t.Logf("有界: series=%d over=%d", good.SeriesCount(), good.Over())

	// ---- ② 高カーディナリティ（cap 無し）: robot_id を混ぜる → 爆発 ----
	explode := metrics.NewLabeled(0) // 上限なし（非推奨。爆発を見せる）
	for i := 0; i < 100000; i++ {
		explode.Inc(fmt.Sprintf("t%02d", i%tenants), fmt.Sprintf("r%06d", i)) // robot_id は毎回別
	}
	rec.Add(expkit.Variant{
		Name:     "高カーディナリティ: robot_id をラベルに（cap 無し）→ 時系列が爆発",
		Accident: true,
		Counters: map[string]int64{"series": int64(explode.SeriesCount())},
		Notes:    []string{"時系列が " + itoa(explode.SeriesCount()) + " まで増える（メモリと監視基盤を圧迫）"},
	})
	t.Logf("爆発: series=%d", explode.SeriesCount())

	// ---- ③ cap で有界化: 同じ高カーディナリティ入力を cap=1000 で ----
	capped := metrics.NewLabeled(1000)
	for i := 0; i < 100000; i++ {
		capped.Inc(fmt.Sprintf("t%02d", i%tenants), fmt.Sprintf("r%06d", i))
	}
	rec.Add(expkit.Variant{
		Name:     "cap=1000: 超過は __over__ に畳む → 実系列は上限以下",
		Counters: map[string]int64{"series": int64(capped.SeriesCount()), "over": capped.Over()},
		Notes:    []string{"実系列 " + itoa(capped.SeriesCount()) + "（≤cap）/ 畳んだ回数 " + itoa64(capped.Over())},
	})
	t.Logf("cap: series=%d over=%d", capped.SeriesCount(), capped.Over())

	// ---- ④ 記録オーバーヘッド（Inc / Observe）----
	const N = 2_000_000
	incNsOp := benchNsOp(N, func(i int) { good.Inc("t01", "s1") }) // 既存系列への Inc
	h := metrics.NewHist(1, 5, 10, 50, 100, 500)                   // ms バケット
	obsNsOp := benchNsOp(N, func(i int) { h.Observe(float64(i % 200)) })
	rec.Add(expkit.Variant{
		Name:    "記録コスト: Inc / Observe（1回あたり）",
		Metrics: map[string]float64{"inc_ns": incNsOp, "observe_ns": obsNsOp, "inc_ops_per_sec": 1e9 / incNsOp},
		Notes:   []string{"Inc " + f1(incNsOp) + "ns / Observe " + f1(obsNsOp) + "ns（DB 往復 ~100000ns に比べ桁違いに安い）"},
	})
	t.Logf("コスト: inc=%.1fns observe=%.1fns", incNsOp, obsNsOp)

	// 並行でも安全（データ競合しない）ことを軽く確認
	var wg sync.WaitGroup
	conc := metrics.NewLabeled(100)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10000; i++ {
				conc.Inc("t01", "s1")
			}
		}()
	}
	wg.Wait()

	// ---- 検証 ----
	if good.SeriesCount() != tenants*statuses {
		t.Errorf("有界ラベルの系列数が違う: %d（%d のはず）", good.SeriesCount(), tenants*statuses)
	}
	if explode.SeriesCount() < 50000 {
		t.Errorf("高カーディナリティが爆発していない: %d", explode.SeriesCount())
	}
	if capped.SeriesCount() > 1000 {
		t.Errorf("cap を超えて系列が増えた: %d", capped.SeriesCount())
	}
	if capped.Over() == 0 {
		t.Errorf("cap 超過が __over__ に畳まれていない")
	}
	if incNsOp > 1000 || obsNsOp > 1000 { // 1回あたり <1マイクロ秒のはず
		t.Errorf("記録コストが高すぎる: inc=%.0fns observe=%.0fns", incNsOp, obsNsOp)
	}

	rec.Scope(
		"純 Go（DB 不要）/ 10万回の Inc・200万回の記録ベンチ / 8並行の安全性チェック",
		"時系列数 = ラベル値の直積。ラベルは有界なものだけにする（tenant・status・種別）",
		"cap は最後の安全弁。設計としては高カーディナリティをそもそもラベルにしない",
	)
	rec.Uncertain(
		"ns/op はこのホストのもの。桁（サブマイクロ秒）が要点で、絶対値は環境で動く",
		"本物の監視基盤（Prometheus 等）では系列ごとにメモリ・スクレイプ負荷がかかる。cap はその保険",
		"分散環境では各レプリカの系列が合算される。ラベル設計は全レプリカ横断で効く",
		"トレース（web→worker の連結）は本実験外（ラベル/コストのみ）",
	)
	rec.Artifact(
		"internal/metrics: カーディナリティ上限つきラベルカウンタと遅延ヒストグラム",
		"docs/observability.md: 何を測るか・ラベル設計・記録コスト",
	)
	rec.Next("なし（EXP-32..37 完了）")

	files, err := rec.Save(
		"可観測性は RED を境界で取り、ラベルは有界なもの（tenant・status・種別）だけにする。" +
			"robot_id/command_id のような高カーディナリティをラベルにすると時系列が爆発する。cap を最後の" +
			"安全弁に、超過は __over__ へ畳む。記録コスト自体はサブマイクロ秒で、DB 往復に比べ無視できる。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func benchNsOp(n int, fn func(i int)) float64 {
	t0 := time.Now()
	for i := 0; i < n; i++ {
		fn(i)
	}
	return float64(time.Since(t0).Nanoseconds()) / float64(n)
}

func f1(v float64) string   { return fmt.Sprintf("%.1f", v) }
func itoa(n int) string     { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }
