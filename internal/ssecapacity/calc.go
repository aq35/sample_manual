// Package ssecapacity は EXP-40（SSE は何人まで／hub あり・なし）の容量計算機。
//
// 実測係数（EXP-31: 1接続 ~34KB・EXP-38: fan-out は激安・DB 往復）を式に入れ、
// 入力（同時接続・テナント数・ポーリング間隔・タスクのメモリ・DB 予算）から、
//   - hub あり: 1タスクの接続上限（メモリ/fd の小さい方）・必要タスク数・DB 読み/秒（接続数に無関係）
//   - hub なし: DB 読み/秒（接続数に比例）・DB 予算で許される最大接続・目標が収まるか
// を出す。実値を差し替えれば即再計算できる。
package ssecapacity

import "math"

// Inputs は容量計算の入力。既定係数は EXP-31/38 の実測に合わせる。
type Inputs struct {
	Subscribers        int     // 想定同時接続（合計）
	Tenants            int     // アクティブなテナント数
	PollSeconds        float64 // ポーリング間隔（秒）
	TaskMemMB          int     // 1タスクのメモリ（MB）
	ReserveMB          int     // ランタイム/GC/アプリ予約（MB）
	PerConnKB          float64 // 1接続メモリ（KB。EXP-31 実測 ~34）
	SafetyFactor       float64 // 実用係数（GC 余白・突発。例 0.4）
	FDCapPerTask       int     // fd 上限（ulimit -n 由来）
	DBReadBudgetPerSec float64 // hub なしで SSE に割ける DB 読み/秒
}

// Result は計算結果。
type Result struct {
	MemConns           int     // メモリ由来の1タスク接続上限（安全率込み）
	PerTaskConnCap     int     // 1タスクの接続上限（mem と fd の小さい方）
	Binding            string  // 上限を決めているもの（"memory" or "fd"）
	TasksNeeded        int     // hub あり: 目標接続を捌くのに要るタスク数
	HubDBReadsPerSec   float64 // hub あり: DB 読み/秒（= テナント数 / 間隔。接続数に無関係）
	NoHubDBReadsPerSec float64 // hub なし: DB 読み/秒（= 接続数 / 間隔）
	NoHubMaxConns      int     // hub なし: DB 予算で許される最大接続（= 予算 × 間隔）
	NoHubFits          bool    // hub なし: 目標が DB 予算に収まるか
}

// Compute は入力から容量を計算する。
func Compute(in Inputs) Result {
	usableBytes := float64(in.TaskMemMB-in.ReserveMB) * 1024 * 1024
	perConnBytes := in.PerConnKB * 1024
	memConns := 0
	if usableBytes > 0 && perConnBytes > 0 {
		memConns = int(usableBytes / perConnBytes * in.SafetyFactor)
	}
	cap := memConns
	binding := "memory"
	if in.FDCapPerTask > 0 && in.FDCapPerTask < cap {
		cap = in.FDCapPerTask
		binding = "fd"
	}
	tasks := 0
	if cap > 0 {
		tasks = int(math.Ceil(float64(in.Subscribers) / float64(cap)))
	}
	var hubReads, noHubReads float64
	if in.PollSeconds > 0 {
		hubReads = float64(in.Tenants) / in.PollSeconds
		noHubReads = float64(in.Subscribers) / in.PollSeconds
	}
	noHubMax := int(in.DBReadBudgetPerSec * in.PollSeconds)
	return Result{
		MemConns:           memConns,
		PerTaskConnCap:     cap,
		Binding:            binding,
		TasksNeeded:        tasks,
		HubDBReadsPerSec:   hubReads,
		NoHubDBReadsPerSec: noHubReads,
		NoHubMaxConns:      noHubMax,
		NoHubFits:          noHubReads <= in.DBReadBudgetPerSec,
	}
}

// Defaults は EXP-31/38 実測ベースの既定係数（1 vCPU/2GB 想定）を埋めた Inputs を返す。
func Defaults() Inputs {
	return Inputs{
		PollSeconds:        1,
		TaskMemMB:          2048,
		ReserveMB:          800,
		PerConnKB:          34,    // EXP-31 実測
		SafetyFactor:       0.4,   // GC 余白・安全率
		FDCapPerTask:       60000, // ulimit を上げた前提
		DBReadBudgetPerSec: 2000,  // SSE に割ける DB 読み/秒（保守的）
	}
}
