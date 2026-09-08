package costlab_test

// EXP-61: 月予算のもとで「タスクスペック↑ / タスク数↑ / Aurora↑」の優先順位（仮想実験）。
//
//	go test ./internal/costlab/ -run TestEXP61 -v   （DB 不要・純 Go の判断モデル）
//
// AWS は触らず、実測した単位容量（EXP-31/59/60）と入力した公式料金から「今の律速に合う lever」を選ぶ。
// 核心: 接続律速のとき（Aurora の max_connections でタスクが頭打ち）はタスクを足しても無駄（EXP-60）→
// Aurora（or Proxy）が第一優先。接続に余裕がある CPU 律速のときは、タスク数/スペックが第一優先で
// Aurora は効かない。料金は入力で、判断ロジックはその向きに頑健。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/costlab"
	"github.com/aq35/sample_manual/internal/expkit"
)

func TestEXP61_コストと優先順位(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-61", "cost-scaling-priority",
		"律速に合う lever を選ぶ: 接続律速→Aurora、CPU律速→タスク（AWS 未使用の判断モデル）")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) 接続律速（タスク×pool が Aurora の max_connections を超える）では、タスクを足しても使える " +
			"rps は増えない（EXP-60）→ Aurora↑ が第一優先。 " +
			"2) 接続に余裕がある CPU 律速では、タスク数/スペックが第一優先で、Aurora↑ は web rps を増やさない。 " +
			"3) Fargate は線形料金なので『タスク数↑』と『スペック↑』は 1rps あたり同程度→分離に有利な横（タスク数）を既定に。")

	// 実測 baseline（入力・実機で置き換える）:
	cap := costlab.Cap{RPSPerVCPU: 200, PoolPerTask: 10} // 1vCPU=200rps(EXP-31 モデル)、1タスク10接続
	// 代表的な料金（★AWS 公式料金から現在値を入れる。ここは説明用）
	prices := costlab.Prices{FargateVCPUHour: 0.04048, FargateGiBHour: 0.004445}
	small := costlab.Aurora{Name: "aurora-small", VCPU: 2, MaxConn: 90, USDPerHour: 0.10}
	large := costlab.Aurora{Name: "aurora-large", VCPU: 4, MaxConn: 200, USDPerHour: 0.20}
	spec := costlab.Spec{VCPU: 1, GiB: 2}

	// ---- シナリオ①: 接続律速（小Aurora max_connections=90 / pool10 → 使えるタスク上限 9）----
	// 12台立てたいが、9台ぶんしか接続が無い＝タスクを足しても無駄。
	desiredA := 12
	bnA := costlab.Bottleneck(desiredA, spec, cap, small, 3000)
	levA := costlab.Evaluate(desiredA, spec, cap, small, prices, 2 /*bigger vCPU*/, &large)
	recA := costlab.Recommend(levA)

	rec.Add(expkit.Variant{
		Name:     "接続律速（12台希望・小Aurora=90接続/pool10→9台で頭打ち）: 第一優先=Aurora↑",
		Accident: true, // 「タスクを足す」の素朴判断が無駄になる事故を示す
		Counters: map[string]int64{
			"usable_tasks": int64(costlab.UsableTasks(desiredA, small, cap.PoolPerTask)),
			"more_tasks_applicable": b2i(findLever(levA, "more-tasks").Applicable),
			"top_is_aurora":         b2i(recA[0].Name == "bigger-aurora"),
		},
		Notes: []string{"律速=" + bnA + " / 第一優先=" + recA[0].Name + "（タスク増は " + applic(findLever(levA, "more-tasks")) + "）"},
	})

	// ---- シナリオ②: CPU律速（大Aurora=200接続 / 8台=80接続で余裕。rps が足りない）----
	desiredB := 8
	bnB := costlab.Bottleneck(desiredB, spec, cap, large, 3000) // 8×200=1600rps < 3000 → app-cpu
	levB := costlab.Evaluate(desiredB, spec, cap, large, prices, 2, nil /*これ以上のクラスは対象外*/)
	recB := costlab.Recommend(levB)

	rec.Add(expkit.Variant{
		Name:     "CPU律速（8台・大Aurora接続余裕・rps不足）: 第一優先=タスク（Aurora↑は効かない）",
		Counters: map[string]int64{
			"web_rps_now":            int64(costlab.WebRPS(desiredB, spec, cap, large)),
			"more_tasks_applicable":  b2i(findLever(levB, "more-tasks").Applicable),
			"top_is_task":            b2i(recB[0].Name == "more-tasks" || recB[0].Name == "bigger-task"),
		},
		Notes: []string{"律速=" + bnB + " / 現状 " + itoa(int(costlab.WebRPS(desiredB, spec, cap, large))) + "rps / 第一優先=" + recB[0].Name},
	})

	// ---- 料金の向き（Fargate 線形なので横と縦は 1rps あたり同程度）----
	more := findLever(levB, "more-tasks")
	bigger := findLever(levB, "bigger-task")
	rec.Add(expkit.Variant{
		Name: "Fargate 線形: 『タスク数↑』と『スペック↑』は 1kRPS あたり同程度 → 横を既定に",
		Metrics: map[string]float64{
			"more_tasks_usd_per_1kRPS":  more.CostPer1kRPS,
			"bigger_task_usd_per_1kRPS": bigger.CostPer1kRPS,
		},
		Notes: []string{"横 $" + f2(more.CostPer1kRPS) + " / 縦 $" + f2(bigger.CostPer1kRPS) + " per 1kRPS（近い→GC/障害影響で横）"},
	})
	t.Logf("A: bottleneck=%s top=%s moreApplicable=%v / B: bottleneck=%s top=%s / linear more=%.2f big=%.2f",
		bnA, recA[0].Name, more.Applicable, bnB, recB[0].Name, more.CostPer1kRPS, bigger.CostPer1kRPS)

	// ---- 検証 ----
	if bnA != "connections" {
		t.Errorf("①が接続律速でない: %s", bnA)
	}
	if findLever(levA, "more-tasks").Applicable {
		t.Errorf("①接続律速でタスク増が『効く』判定になっている（無駄なはず）")
	}
	if recA[0].Name != "bigger-aurora" {
		t.Errorf("①第一優先が Aurora でない: %s", recA[0].Name)
	}
	if bnB != "app-cpu" {
		t.Errorf("②が CPU 律速でない: %s", bnB)
	}
	if !(recB[0].Name == "more-tasks" || recB[0].Name == "bigger-task") {
		t.Errorf("②第一優先がタスク系でない: %s", recB[0].Name)
	}
	// Fargate 線形: 横と縦の 1kRPS 単価が近い（2倍以内）
	if more.CostPer1kRPS <= 0 || bigger.CostPer1kRPS <= 0 || ratio(more.CostPer1kRPS, bigger.CostPer1kRPS) > 2 {
		t.Errorf("横と縦の単価が線形近似から外れる: more=%.3f big=%.3f", more.CostPer1kRPS, bigger.CostPer1kRPS)
	}

	rec.Scope(
		"純 Go の判断モデル / 単位容量 RPSPerVCPU=200・pool10（実機で置換）/ 料金は入力（AWS 公式から）",
		"接続律速の判定は UsableTasks=max_connections/pool（EXP-60）。web rps=使えるタスク×vCPU×RPSPerVCPU",
		"検証しているのは『律速→lever』の向き（料金の絶対値に依らない）",
	)
	rec.Uncertain(
		"RPSPerVCPU・pool・SSE/GiB は実機で測って入れる（EXP-31/59）。1リクエストの重さで大きく動く",
		"料金は変動する。AWS の Fargate/Aurora 公式料金ページから現在値を入れる。リージョン差・Savings Plans も",
		"RDS Proxy を挟むと接続律速が緩む（DB 接続がタスク数に比例しない・rds-proxy.md）→ 第4の lever",
		"DB スループット律速（クエリが遅い/DB CPU 飽和）は別軸。まずクエリ修正($0)→レプリカ→Aurora↑",
	)
	rec.Artifact(
		"internal/costlab: 律速→lever の優先順位を出すコストモデル",
		"docs/cost-scaling-priority.md: 月予算での縦/横/Aurora の優先順位",
	)
	rec.Next("（アーキテクチャ×コスト）")

	files, err := rec.Save(
		"月予算のもとでの優先順位は『律速に合う lever に使う』が原則。接続律速（タスク×pool > Aurora の " +
			"max_connections）ではタスクを足しても無駄（EXP-60）→ Aurora↑（or RDS Proxy）が第一。接続に余裕が " +
			"ある CPU 律速ではタスク数/スペックが第一で Aurora は効かない。Fargate は線形料金なので横と縦は " +
			"1rps あたり同程度→GC 停止・障害影響で有利な横（タスク数）を既定に。最優先は $0 のクエリ修正/pool 適正化。" +
			"料金は AWS 公式から入れ、単位容量は実機で測って置き換える。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func findLever(ls []costlab.Lever, name string) costlab.Lever {
	for _, l := range ls {
		if l.Name == name {
			return l
		}
	}
	return costlab.Lever{Name: name}
}
func applic(l costlab.Lever) string {
	if l.Applicable {
		return "効く"
	}
	return "無駄"
}
func ratio(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	if b <= 0 {
		return 1e18
	}
	return a / b
}
func b2i(b bool) int64 {
	if b {
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
func f2(v float64) string {
	n := int64(v*100 + 0.5)
	return itoa(int(n/100)) + "." + pad2(int(n%100))
}
func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
