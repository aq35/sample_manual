// Package memlab は EXP-50（Go の変数・型ごとのメモリ単価）の実験本体。
//
// 「2GB のタスクに何本つなげるか / 何件をメモリに載せられるか」を見積もるには、まず
// 「1つあたり何バイト食うか」を実測して正規化する。runtime.MemStats のヒープ増分を
// 確保数で割って単価を出し、goroutine スタックは StackInuse の増分で測る。
package memlab

import "runtime"

// sink は測定中に確保物を GC から守るための保持先（グローバルに置いて生存させる）。
var sink []any

// HeapPerItem は alloc() が n 個を sink に積む前後のヒープ増分を n で割った「1個あたりバイト」。
// alloc は確保したものを必ず sink（や別のグローバル）へ入れて生存させること。
func HeapPerItem(n int, alloc func()) float64 {
	sink = nil
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	alloc()

	runtime.GC() // 生きているものだけ残す
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	delta := int64(m2.HeapAlloc) - int64(m1.HeapAlloc)
	sink = nil // 後片付け
	runtime.GC()
	if delta < 0 {
		delta = 0
	}
	return float64(delta) / float64(n)
}

// Keep は確保物を生存させる（sink に積む）。
func Keep(v any) { sink = append(sink, v) }

// SmallStruct は「小さめの行 struct」の代表（3 フィールド）。
type SmallStruct struct {
	ID     int64
	Tenant int32
	Flag   bool
}

// Row は「一覧行っぽい struct」（文字列2本＋数値）。文字列本体は別途ヒープに載る。
type Row struct {
	ID     int64
	Name   string
	Status string
	At     int64
}

// GoroutineStackPerItem は n 個の goroutine を park させ、StackInuse の増分を n で割る。
// 返り値は「1 goroutine あたりのスタック実バイト」。測定後に goroutine は解放する。
func GoroutineStackPerItem(n int) float64 {
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	before := m1.StackInuse

	release := make(chan struct{})
	started := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			started <- struct{}{}
			<-release // park（スタックを保持したまま待つ）
		}()
	}
	for i := 0; i < n; i++ {
		<-started // 全 goroutine が動き出すまで待つ
	}

	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	after := m2.StackInuse

	close(release) // 解放
	delta := int64(after) - int64(before)
	if delta < 0 {
		delta = 0
	}
	return float64(delta) / float64(n)
}

// FitInBudget は予算バイトを 1個あたりバイトで割った「載る個数」。
func FitInBudget(budgetBytes, perItem float64) int64 {
	if perItem <= 0 {
		return 0
	}
	return int64(budgetBytes / perItem)
}
