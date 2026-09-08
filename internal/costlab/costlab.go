// Package costlab は EXP-61（仮想コスト・容量モデル）の本体。
//
// 月予算の上限のもとで「タスクスペック↑ / タスク数↑ / Aurora スペック↑」のどれを優先すべきかを、
// AWS を触らずに決めるための計算モデル。核心は EXP-60 の結論——**接続律速のときにタスクを増やしても
// 使える容量は増えない**（Aurora の max_connections でタスクが頭打ち）。だから lever は律速に合わせる。
//
// ★料金(USDPerHour 等)は「入力」で、AWS の公式料金ページから現在値を入れて使う（変動する）。
// このモデルが検証するのは「律速 → どの lever を優先すべきか」という**判断ロジック**（料金に頑健）。
package costlab

// Spec は1タスク(Fargate 等)のスペック。
type Spec struct {
	VCPU float64
	GiB  float64
}

// Aurora は DB のクラス（スペックが上がると max_connections と処理能力が増える）。
type Aurora struct {
	Name       string
	VCPU       float64
	MaxConn    int     // このクラスの max_connections
	USDPerHour float64 // 公式料金から入れる（入力）
}

// Cap は実測に基づく単位容量。
type Cap struct {
	RPSPerVCPU  float64 // Web は CPU 律速: 1 vCPU あたり捌ける rps（実機で測る・EXP-31）
	PoolPerTask int     // 1タスクが DB に張る接続本数
}

// Prices は Fargate の線形料金（入力・公式料金から）。
type Prices struct {
	FargateVCPUHour float64
	FargateGiBHour  float64
}

const hoursPerMonth = 730.0

// UsableTasks は「接続予算で頭打ちにした」実際に使えるタスク数（EXP-60）。
// desired 台を立てても、Aurora の max_connections ÷ pool を超える分は 1040 で拒否され使えない。
func UsableTasks(desired int, a Aurora, poolPerTask int) int {
	if poolPerTask <= 0 {
		return desired
	}
	capByConn := a.MaxConn / poolPerTask
	if desired < capByConn {
		return desired
	}
	return capByConn
}

// WebRPS はこの構成で実際に捌ける web rps（接続で頭打ちにしたタスク数 × 1タスクの rps）。
func WebRPS(desired int, spec Spec, c Cap, a Aurora) float64 {
	ut := UsableTasks(desired, a, c.PoolPerTask)
	return float64(ut) * c.RPSPerVCPU * spec.VCPU
}

// Bottleneck は今の律速を返す（"connections" / "app-cpu" / "ok"）。
func Bottleneck(desired int, spec Spec, c Cap, a Aurora, targetRPS float64) string {
	if UsableTasks(desired, a, c.PoolPerTask) < desired {
		return "connections" // Aurora の接続でタスクが頭打ち（タスクを足しても無駄・EXP-60）
	}
	if WebRPS(desired, spec, c, a) < targetRPS {
		return "app-cpu" // 接続は足りているが計算力(CPU)が足りない
	}
	return "ok"
}

// TaskCostPerMonth は1タスクの月額。
func TaskCostPerMonth(spec Spec, p Prices) float64 {
	return (spec.VCPU*p.FargateVCPUHour + spec.GiB*p.FargateGiBHour) * hoursPerMonth
}

// AuroraCostPerMonth は Aurora の月額。
func AuroraCostPerMonth(a Aurora) float64 { return a.USDPerHour * hoursPerMonth }

// Lever は1つの打ち手の評価結果。
type Lever struct {
	Name          string  // "more-tasks" / "bigger-task" / "bigger-aurora"
	DeltaRPS      float64 // この打ち手で増える使える rps
	DeltaCostMo   float64 // 増える月額
	CostPer1kRPS  float64 // 1000rps あたり月額（小さいほど良い。DeltaRPS<=0 なら +Inf）
	Applicable    bool    // 効くか（接続律速でタスク増は Applicable=false）
	Note          string
}

func costPer1k(dRPS, dCost float64) float64 {
	if dRPS <= 0 {
		return 1e18 // 効かない
	}
	return dCost / (dRPS / 1000.0)
}

// Evaluate は3つの lever を評価する（次のクラス nextAurora が nil ならその lever は対象外）。
func Evaluate(desired int, spec Spec, c Cap, cur Aurora, p Prices, biggerTaskVCPU float64, nextAurora *Aurora) []Lever {
	cur0 := WebRPS(desired, spec, c, cur)

	// ① more-tasks: 1台足す（接続律速なら使えるタスクが増えない → DeltaRPS=0）
	moreRPS := WebRPS(desired+1, spec, c, cur) - cur0
	moreCost := TaskCostPerMonth(spec, p)
	more := Lever{
		Name: "more-tasks", DeltaRPS: moreRPS, DeltaCostMo: moreCost,
		CostPer1kRPS: costPer1k(moreRPS, moreCost), Applicable: moreRPS > 0,
	}
	if moreRPS <= 0 {
		more.Note = "接続律速でタスクを足しても使える容量が増えない（EXP-60）"
	}

	// ② bigger-task: 全タスクの vCPU を biggerTaskVCPU に上げる（接続は不変）
	bigSpec := Spec{VCPU: biggerTaskVCPU, GiB: spec.GiB * (biggerTaskVCPU / spec.VCPU)}
	bigRPS := WebRPS(desired, bigSpec, c, cur) - cur0
	ut := UsableTasks(desired, cur, c.PoolPerTask)
	bigCost := float64(ut) * (TaskCostPerMonth(bigSpec, p) - TaskCostPerMonth(spec, p))
	bigger := Lever{
		Name: "bigger-task", DeltaRPS: bigRPS, DeltaCostMo: bigCost,
		CostPer1kRPS: costPer1k(bigRPS, bigCost), Applicable: bigRPS > 0,
	}

	// ③ bigger-aurora: 次のクラスへ（max_connections が増え、接続律速が解ける）
	aur := Lever{Name: "bigger-aurora", Applicable: false, CostPer1kRPS: 1e18}
	if nextAurora != nil {
		aRPS := WebRPS(desired, spec, c, *nextAurora) - cur0
		aCost := AuroraCostPerMonth(*nextAurora) - AuroraCostPerMonth(cur)
		aur = Lever{
			Name: "bigger-aurora", DeltaRPS: aRPS, DeltaCostMo: aCost,
			CostPer1kRPS: costPer1k(aRPS, aCost), Applicable: aRPS > 0,
		}
		if aRPS <= 0 {
			aur.Note = "接続は足りているので Aurora を上げても web rps は増えない（律速でない）"
		}
	}
	return []Lever{more, bigger, aur}
}

// Recommend は Evaluate 結果を「効く & 1kRPS あたり安い」順に並べる（先頭が第一優先）。
func Recommend(levers []Lever) []Lever {
	out := make([]Lever, len(levers))
	copy(out, levers)
	// 単純な安定ソート（applicable 優先、その中で CostPer1kRPS 昇順）
	for i := 0; i < len(out); i++ {
		best := i
		for j := i + 1; j < len(out); j++ {
			if less(out[j], out[best]) {
				best = j
			}
		}
		out[i], out[best] = out[best], out[i]
	}
	return out
}

func less(a, b Lever) bool {
	if a.Applicable != b.Applicable {
		return a.Applicable // applicable を前へ
	}
	return a.CostPer1kRPS < b.CostPer1kRPS
}
