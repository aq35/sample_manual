// Package queuesvc は「外部キュー」の代役。
//
// graceful shutdown の実験では、**ack 済みなのに処理されていない**（消失）と、
// **未 ack のまま抱え込んだ**（滞留）を区別する必要がある。
// そのため、取り出し（pull）・確定（ack）・差し戻し（nack）を別々に数える。
package queuesvc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Item はキューの1件。
type Item struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

type state struct {
	item       Item
	inflight   bool
	acked      bool
	deliveries int
	deadline   time.Time
}

// Service はキューの代役。
type Service struct {
	mu        sync.Mutex
	items     []*state
	visible   time.Duration
	failPulls int // 最初の n 回の pull を 500 で返す（再試行の経路を通すため）

	srv *httptest.Server
}

// FailNextPulls は次の n 回の取り出しを失敗させる。
// 「再試行の待ち時間中に終了信号を受ける」状況を作るために使う。
func (s *Service) FailNextPulls(n int) {
	s.mu.Lock()
	s.failPulls = n
	s.mu.Unlock()
}

// New は n 件を積んだキューを起動する。visible は取り出し後の可視性タイムアウト
// （この時間 ack されなければ、他のプロセスに再配達される）。
func New(n int, visible time.Duration) *Service {
	s := &Service{visible: visible}
	for i := 1; i <= n; i++ {
		s.items = append(s.items, &state{item: Item{ID: i, Body: "payload"}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/pull", s.handlePull)
	mux.HandleFunc("/ack", s.handleAck)
	mux.HandleFunc("/nack", s.handleNack)
	mux.HandleFunc("/stats", s.handleStats)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *Service) Close()      { s.srv.Close() }
func (s *Service) URL() string { return s.srv.URL }

// Stats は現在の状態。
type Stats struct {
	Pending  int `json:"pending"`  // まだ配っていない
	Inflight int `json:"inflight"` // 配ったが ack されていない
	Acked    int `json:"acked"`    // 確定した
	Redeliv  int `json:"redelivered"`
}

func (s *Service) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked()
}

func (s *Service) statsLocked() Stats {
	var st Stats
	now := time.Now()
	for _, it := range s.items {
		switch {
		case it.acked:
			st.Acked++
		case it.inflight && it.deadline.After(now):
			st.Inflight++
		default:
			st.Pending++
		}
		if it.deliveries > 1 {
			st.Redeliv++
		}
	}
	return st
}

// AckedIDs は ack 済みの ID。
func (s *Service) AckedIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, it := range s.items {
		if it.acked {
			out = append(out, it.item.ID)
		}
	}
	return out
}

func (s *Service) handlePull(w http.ResponseWriter, r *http.Request) {
	n := 1
	if v := r.URL.Query().Get("n"); v != "" {
		_, _ = fmtSscan(v, &n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failPulls > 0 {
		s.failPulls--
		http.Error(w, "temporary failure", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	var out []Item
	for _, it := range s.items {
		if len(out) >= n {
			break
		}
		if it.acked {
			continue
		}
		if it.inflight && it.deadline.After(now) {
			continue // まだ他のプロセスが持っている
		}
		it.inflight = true
		it.deliveries++
		it.deadline = now.Add(s.visible)
		out = append(out, it.item)
	}
	writeJSON(w, map[string]any{"items": out})
}

func (s *Service) handleAck(w http.ResponseWriter, r *http.Request) {
	ids := readIDs(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		for _, id := range ids {
			if it.item.ID == id {
				it.acked = true
				it.inflight = false
			}
		}
	}
	writeJSON(w, s.statsLocked())
}

func (s *Service) handleNack(w http.ResponseWriter, r *http.Request) {
	ids := readIDs(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		for _, id := range ids {
			if it.item.ID == id && !it.acked {
				// すぐに他のプロセスへ配れるよう、可視性を戻す
				it.inflight = false
				it.deadline = time.Time{}
			}
		}
	}
	writeJSON(w, s.statsLocked())
}

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Stats())
}

func readIDs(r *http.Request) []int {
	var body struct {
		IDs []int `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.IDs
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}
