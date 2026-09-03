package expkit

import (
	"database/sql"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample は1時点の観測値。
type Sample struct {
	At         time.Duration `json:"at_ns"` // 開始からの経過
	Goroutines int           `json:"goroutines"`
	HeapAlloc  uint64        `json:"heap_alloc_bytes"`
	RSS        uint64        `json:"rss_bytes"`
	// DB プール（db が与えられたときだけ）
	OpenConns     int           `json:"db_open_conns"`
	InUse         int           `json:"db_in_use"`
	Idle          int           `json:"db_idle"`
	WaitCount     int64         `json:"db_wait_count"`
	WaitDuration  time.Duration `json:"db_wait_duration_ns"`
	MaxIdleClosed int64         `json:"db_max_idle_closed"`
	MaxLifeClosed int64         `json:"db_max_lifetime_closed"`
	// 呼び出し側が足す値（queue の深さなど）
	Custom map[string]float64 `json:"custom,omitempty"`
}

// Sampler は goroutine 数・メモリ・DB プールを一定間隔で記録する。
//
// 「メモリが無制限に増えないこと」を主張するには、最大値と時系列が要る。
// 最後の1点だけでは、途中で膨らんで GC で戻った形が見えない。
type Sampler struct {
	db       *sql.DB
	interval time.Duration
	custom   func() map[string]float64

	mu      sync.Mutex
	samples []Sample
	stop    chan struct{}
	done    chan struct{}
	start   time.Time
}

func NewSampler(db *sql.DB, interval time.Duration) *Sampler {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	return &Sampler{db: db, interval: interval}
}

// Custom は毎回の観測に足す値（queue 深さなど）を登録する。
func (s *Sampler) Custom(fn func() map[string]float64) *Sampler {
	s.custom = fn
	return s
}

func (s *Sampler) Start() *Sampler {
	s.start = time.Now()
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		s.record()
		for {
			select {
			case <-t.C:
				s.record()
			case <-s.stop:
				s.record()
				return
			}
		}
	}()
	return s
}

func (s *Sampler) Stop() []Sample {
	if s.stop == nil {
		return nil
	}
	close(s.stop)
	<-s.done
	s.stop = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples
}

func (s *Sampler) record() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sample := Sample{
		At:         time.Since(s.start),
		Goroutines: runtime.NumGoroutine(),
		HeapAlloc:  ms.HeapAlloc,
		RSS:        RSS(),
	}
	if s.db != nil {
		st := s.db.Stats()
		sample.OpenConns = st.OpenConnections
		sample.InUse = st.InUse
		sample.Idle = st.Idle
		sample.WaitCount = st.WaitCount
		sample.WaitDuration = st.WaitDuration
		sample.MaxIdleClosed = st.MaxIdleClosed
		sample.MaxLifeClosed = st.MaxLifetimeClosed
	}
	if s.custom != nil {
		sample.Custom = s.custom()
	}
	s.mu.Lock()
	s.samples = append(s.samples, sample)
	s.mu.Unlock()
}

// SampleSummary は時系列の要約（結果ファイルに載せる形）。
type SampleSummary struct {
	Count             int                `json:"count"`
	MaxGoroutines     int                `json:"max_goroutines"`
	FinalGoroutines   int                `json:"final_goroutines"`
	MaxHeapAlloc      uint64             `json:"max_heap_alloc_bytes"`
	FinalHeapAlloc    uint64             `json:"final_heap_alloc_bytes"`
	MaxRSS            uint64             `json:"max_rss_bytes"`
	MaxOpenConns      int                `json:"max_db_open_conns"`
	MaxInUse          int                `json:"max_db_in_use"`
	TotalWaitCount    int64              `json:"db_wait_count"`
	TotalWaitDuration time.Duration      `json:"db_wait_duration_ns"`
	MaxCustom         map[string]float64 `json:"max_custom,omitempty"`
}

func Summarize(samples []Sample) SampleSummary {
	var out SampleSummary
	out.Count = len(samples)
	if len(samples) == 0 {
		return out
	}
	out.MaxCustom = map[string]float64{}
	for _, s := range samples {
		if s.Goroutines > out.MaxGoroutines {
			out.MaxGoroutines = s.Goroutines
		}
		if s.HeapAlloc > out.MaxHeapAlloc {
			out.MaxHeapAlloc = s.HeapAlloc
		}
		if s.RSS > out.MaxRSS {
			out.MaxRSS = s.RSS
		}
		if s.OpenConns > out.MaxOpenConns {
			out.MaxOpenConns = s.OpenConns
		}
		if s.InUse > out.MaxInUse {
			out.MaxInUse = s.InUse
		}
		if s.WaitCount > out.TotalWaitCount {
			out.TotalWaitCount = s.WaitCount
		}
		if s.WaitDuration > out.TotalWaitDuration {
			out.TotalWaitDuration = s.WaitDuration
		}
		for k, v := range s.Custom {
			if v > out.MaxCustom[k] {
				out.MaxCustom[k] = v
			}
		}
	}
	last := samples[len(samples)-1]
	out.FinalGoroutines = last.Goroutines
	out.FinalHeapAlloc = last.HeapAlloc
	if len(out.MaxCustom) == 0 {
		out.MaxCustom = nil
	}
	return out
}

// RSS は自プロセスの常駐メモリ（Linux の /proc/self/status VmRSS）。
// 取れない環境では 0 を返す（Go のヒープだけでは足りないため別途見る）。
func RSS() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
