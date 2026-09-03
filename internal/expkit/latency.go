package expkit

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Latency は所要時間の記録。p50/p95/p99 を出すためだけの最小構成。
//
// ヒストグラムではなく全件を保持する。実験の規模（数万件）では問題にならず、
// 後から任意の分位点を出せるほうが都合がよい。
type Latency struct {
	mu      sync.Mutex
	samples []time.Duration
}

func NewLatency() *Latency { return &Latency{} }

func (l *Latency) Record(d time.Duration) {
	l.mu.Lock()
	l.samples = append(l.samples, d)
	l.mu.Unlock()
}

// Observe は fn の実行時間を記録する。
func (l *Latency) Observe(fn func()) {
	start := time.Now()
	fn()
	l.Record(time.Since(start))
}

// LatencyStats は結果ファイルに載る形。
type LatencyStats struct {
	Count int           `json:"count"`
	Min   time.Duration `json:"min_ns"`
	P50   time.Duration `json:"p50_ns"`
	P95   time.Duration `json:"p95_ns"`
	P99   time.Duration `json:"p99_ns"`
	Max   time.Duration `json:"max_ns"`
	Mean  time.Duration `json:"mean_ns"`
}

func (s LatencyStats) String() string {
	if s.Count == 0 {
		return "（記録なし）"
	}
	return fmt.Sprintf("n=%d p50=%v p95=%v p99=%v max=%v",
		s.Count, s.P50.Round(time.Microsecond), s.P95.Round(time.Microsecond),
		s.P99.Round(time.Microsecond), s.Max.Round(time.Microsecond))
}

func (l *Latency) Stats() LatencyStats {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.samples) == 0 {
		return LatencyStats{}
	}
	s := make([]time.Duration, len(l.samples))
	copy(s, l.samples)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })

	var total time.Duration
	for _, d := range s {
		total += d
	}
	return LatencyStats{
		Count: len(s),
		Min:   s[0],
		P50:   quantile(s, 0.50),
		P95:   quantile(s, 0.95),
		P99:   quantile(s, 0.99),
		Max:   s[len(s)-1],
		Mean:  total / time.Duration(len(s)),
	}
}

func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1)*q + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
