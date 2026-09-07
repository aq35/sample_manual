// Package metrics は、可観測性の最小限の道具（ラベル付きカウンタと遅延ヒストグラム）。
//
// 方針（EXP-37）:
//   - RED（Rate/Errors/Duration）を境界で取る。ラベルは「有界」なものだけ（tenant・status）。
//   - robot_id や command_id のような高カーディナリティをラベルにしない（時系列が爆発する）。
//     安全のため、ラベル値の種類数に上限を設け、超えたら "__over__" にまとめる。
//   - 記録コストは無視できるほど軽い（DB 往復に比べて桁違いに安い）ことを EXP-37 で確かめる。
package metrics

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Labeled はラベル付きカウンタ。ラベル値の組を "|" で連結してキーにする。
// 種類数が cap を超えると、新しい組は over（"__over__"）に折り畳む（カーディナリティ上限）。
type Labeled struct {
	cap    int
	mu     sync.RWMutex
	series map[string]*int64
	over   int64
}

// NewLabeled は時系列数の上限 cap を持つカウンタを作る（cap<=0 なら無制限＝非推奨）。
func NewLabeled(cap int) *Labeled {
	return &Labeled{cap: cap, series: make(map[string]*int64)}
}

func joinLabels(labels []string) string { return strings.Join(labels, "|") }

// Inc はラベルの組の系列を1増やす。上限超過分は over にまとまる。
func (l *Labeled) Inc(labels ...string) {
	key := joinLabels(labels)
	l.mu.RLock()
	if p, ok := l.series[key]; ok {
		l.mu.RUnlock()
		atomic.AddInt64(p, 1)
		return
	}
	l.mu.RUnlock()

	l.mu.Lock()
	if p, ok := l.series[key]; ok { // 二重確認
		l.mu.Unlock()
		atomic.AddInt64(p, 1)
		return
	}
	if l.cap > 0 && len(l.series) >= l.cap {
		l.over++
		l.mu.Unlock()
		return
	}
	var v int64 = 1
	l.series[key] = &v
	l.mu.Unlock()
}

// SeriesCount は今保持している時系列の数（over は含めない実系列の数）。
func (l *Labeled) SeriesCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.series)
}

// Over は上限超過で折り畳まれた回数。
func (l *Labeled) Over() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.over
}

// Hist は固定バケットの遅延ヒストグラム（境界は昇順の秒/ミリ秒など任意単位）。
type Hist struct {
	bounds []float64
	counts []int64 // len(bounds)+1（最後は +Inf）
	total  int64
}

// NewHist は境界（昇順）でヒストグラムを作る。
func NewHist(bounds ...float64) *Hist {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	return &Hist{bounds: b, counts: make([]int64, len(b)+1)}
}

// Observe は値 v を該当バケットに1つ入れる（bounds[i-1] < v ≤ bounds[i] を i 番に、超過は末尾）。
func (h *Hist) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	atomic.AddInt64(&h.counts[i], 1)
	atomic.AddInt64(&h.total, 1)
}

// Total は観測回数。
func (h *Hist) Total() int64 { return atomic.LoadInt64(&h.total) }

// Buckets は各バケットの件数（最後は +Inf 超え）。
func (h *Hist) Buckets() []int64 {
	out := make([]int64, len(h.counts))
	for i := range h.counts {
		out[i] = atomic.LoadInt64(&h.counts[i])
	}
	return out
}
