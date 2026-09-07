// Package poolbudget は「コンテナ数 × プール ≤ DB の接続予算」を計算する。
//
// EXP-5 で測ったのは「1つの DB がどこで頭打ちになるか（飽和点）」。
// このパッケージはその手前、**接続数がそもそも予算に収まるか**を見積もる。
// 接続は唯一の希少資源で、オートスケールで コンテナ数×プール が膨らんで
// max_connections を食い潰すのが典型事故（store.go §5.1）。
//
// ★注意: ここが「収まる」でも遅延が良いとは限らない。
// スループットの膝（throughput knee）は別で、EXP-5 で実測する。
package poolbudget

import (
	"fmt"
	"math"
	"strings"
)

// Plan は1つの構成。
type Plan struct {
	DBMaxConnections int // MySQL の max_connections
	Reserved         int // 管理・マイグレーション・監視のために空けておく接続
	Containers       int // アプリ/ワーカーのコンテナ数
	PerContainer     int // 1コンテナの MaxOpenConns（プライマリ）
	ReplicaPer       int // 1コンテナのレプリカ読み取り用プール（無ければ 0）

	// ProxyBackend > 0 なら、ProxySQL / RDS Proxy が前段を多重化し、
	// DB に張る接続はこの本数で頭打ちになる（コンテナ数に比例しなくなる）。
	ProxyBackend int
}

// Budget は使ってよい接続数（予約を引いた残り）。
func (p Plan) Budget() int { return p.DBMaxConnections - p.Reserved }

// Demand は DB に実際に張られる接続数の見積もり。
func (p Plan) Demand() int {
	if p.ProxyBackend > 0 {
		// Proxy 経由: バックエンドは多重化されるので、コンテナ数に依らずこの本数。
		return p.ProxyBackend
	}
	return p.Containers * (p.PerContainer + p.ReplicaPer)
}

// Fits は予算に収まるか。
func (p Plan) Fits() bool { return p.Demand() <= p.Budget() }

// Headroom は余り（負なら不足）。
func (p Plan) Headroom() int { return p.Budget() - p.Demand() }

// Utilization は予算に対する使用率。
func (p Plan) Utilization() float64 {
	if p.Budget() <= 0 {
		return math.Inf(1)
	}
	return float64(p.Demand()) / float64(p.Budget())
}

// MaxContainers は、この per-container プールのまま増やせるコンテナ数の上限。
// Proxy 構成では「コンテナ数で頭打ちにならない」ので -1 を返す。
func (p Plan) MaxContainers() int {
	if p.ProxyBackend > 0 {
		return -1
	}
	per := p.PerContainer + p.ReplicaPer
	if per <= 0 {
		return -1
	}
	return p.Budget() / per
}

// RecommendPerContainer は、目標コンテナ数に収めるための per-container プール上限。
// 余白率 headroomRatio（例 0.2 で 20% 空ける）を確保する。
func RecommendPerContainer(budget, targetContainers int, headroomRatio float64) int {
	if targetContainers <= 0 {
		return 0
	}
	usable := int(float64(budget) * (1 - headroomRatio))
	per := usable / targetContainers
	if per < 1 {
		return 0 // このコンテナ数では、余白を確保すると 1 本も張れない
	}
	return per
}

// Report は人が読める要約。
func (p Plan) Report() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("DB max_connections : %d", p.DBMaxConnections)
	w("予約（管理/移行/監視）: %d", p.Reserved)
	w("使える予算          : %d", p.Budget())
	if p.ProxyBackend > 0 {
		w("構成               : Proxy 多重化（backend %d 本で頭打ち）", p.ProxyBackend)
	} else {
		w("構成               : 直結 %d コンテナ × (%d + replica %d) 本",
			p.Containers, p.PerContainer, p.ReplicaPer)
	}
	w("DB への接続見積もり : %d", p.Demand())
	w("余白               : %d（使用率 %.0f%%）", p.Headroom(), p.Utilization()*100)
	if p.Fits() {
		w("判定               : 収まる")
	} else {
		w("判定               : ★収まらない（%d 本オーバー）", -p.Headroom())
	}
	if mc := p.MaxContainers(); mc >= 0 {
		w("このプールで増やせるコンテナ上限: %d", mc)
	}
	w("備考               : 収まる = 遅延が良い ではない。スループットの膝は EXP-5 で実測する")
	return b.String()
}

// Role は接続予算の中での役割ごとの取り分。
type Role struct {
	Name         string
	Containers   int
	PerContainer int
}

// Guard は「全役割の接続要求が予算に収まるか」を起動時に確かめる。
// 収まらなければ error（fail-fast）。web と worker を別プロセスにしても、
// 合計が DB の予算を超えないことをここで担保する。
func Guard(dbMax, reserved int, roles ...Role) error {
	budget := dbMax - reserved
	total := 0
	var parts []string
	for _, r := range roles {
		d := r.Containers * r.PerContainer
		total += d
		parts = append(parts, fmt.Sprintf("%s=%d×%d=%d", r.Name, r.Containers, r.PerContainer, d))
	}
	if total > budget {
		return fmt.Errorf("接続予算オーバー: 要求 %d > 予算 %d（max %d - 予約 %d）[%s]",
			total, budget, dbMax, reserved, strings.Join(parts, " "))
	}
	return nil
}
