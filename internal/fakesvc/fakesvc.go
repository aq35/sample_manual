// Package fakesvc は「外部サービス」の代役。
//
// §2 の主張を実際に再現するために、次を意図的に起こせるようにしてある。
//   - 名簿にだけ現れる新しい対象（WebSocket には流れてこない）
//   - DB とズレた値（WebSocket は「変化なし」なので直せない）
//   - 沈黙（対象が止まったのか、送るのをやめただけなのか区別できない）
//   - 接続は生きているが何も流れてこない状態（§2.6）
//   - API が答えない状態（§2.4 の「ここで嘘をつかない」）
package fakesvc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Robot はサービス側が持っている状態。
type Robot struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	Online     bool    `json:"online"`
	Battery    float64 `json:"battery"`
	ObservedAt string  `json:"observed_at"`
	// 以下は「API は返すが、こちらは要らない」項目（§6.1）
	Name     string `json:"name"`
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
}

// Service は外部サービスの代役。
type Service struct {
	mu      sync.Mutex
	robots  map[string]*Robot
	silent  bool // WebSocket に何も流さない（接続は生かしたまま）
	apiDown bool // API が答えない
	noPong  bool // Ping に応答しない（§2.6）

	upgrader websocket.Upgrader
	conns    map[*websocket.Conn]string // 接続 → 購読しているテナントの接頭辞

	srv *httptest.Server
}

func New() *Service {
	s := &Service{
		robots: map[string]*Robot{},
		conns:  map[*websocket.Conn]string{},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots", s.handleList)
	mux.HandleFunc("/ws", s.handleWS)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *Service) Close() {
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.srv.Close()
}

func (s *Service) URL() string   { return s.srv.URL }
func (s *Service) WSURL() string { return "ws" + s.srv.URL[len("http"):] + "/ws" }

// ---- サービス側の操作（テストから状況を作る） ----

// Add は名簿に対象を足す。★WebSocket には流さない。
// 「名簿は API が持つ」（§2.6）ことを再現するため。
func (s *Service) Add(id, status string, online bool, batt float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.robots[id] = &Robot{
		ID: id, Status: status, Online: online, Battery: batt,
		ObservedAt: nowRFC3339Milli(),
		Name:       "robot-" + id, Model: "AGV-3000", Firmware: "2.14.3",
	}
}

// Remove は名簿から消す（§3.3 の掃除の相手）。
func (s *Service) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.robots, id)
}

// Publish は状態を変えて、WebSocket にも流す（正常な差分配信）。
func (s *Service) Publish(id, status string, online bool, batt float64) {
	s.mu.Lock()
	r, ok := s.robots[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	r.Status, r.Online, r.Battery = status, online, batt
	r.ObservedAt = nowRFC3339Milli()
	silent := s.silent
	payload, _ := json.Marshal(r)
	conns := make([]*websocket.Conn, 0, len(s.conns))
	for c, prefix := range s.conns {
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue // 別テナントの購読者には流さない
		}
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if silent {
		return // 接続は生きているが、何も流れてこない（§2.6）
	}
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_ = c.WriteMessage(websocket.TextMessage, payload)
	}
}

// SetQuietly は「サービス側だけ状態が変わり、こちらには通知が来ない」状況を作る。
// これが A（全件同期）でしか直せないズレ（§2.2）。
func (s *Service) SetQuietly(id, status string, online bool, batt float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.robots[id]; ok {
		r.Status, r.Online, r.Battery = status, online, batt
		r.ObservedAt = nowRFC3339Milli()
	}
}

// SetSilent は WebSocket への配信を止める（接続は維持したまま）。
func (s *Service) SetSilent(v bool) { s.mu.Lock(); s.silent = v; s.mu.Unlock() }

// SetAPIDown は API を落とす（§2.4「API も答えない → 不明のまま」）。
func (s *Service) SetAPIDown(v bool) { s.mu.Lock(); s.apiDown = v; s.mu.Unlock() }

// SetNoPong は Ping に応答しない状態にする（§2.6）。
func (s *Service) SetNoPong(v bool) { s.mu.Lock(); s.noPong = v; s.mu.Unlock() }

// Conns は現在の WebSocket 接続数。
func (s *Service) Conns() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.conns) }

// ---- HTTP ----

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	down := s.apiDown
	list := make([]*Robot, 0, len(s.robots))
	for _, rb := range s.robots {
		cp := *rb
		list = append(list, &cp)
	}
	s.mu.Unlock()

	if down {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	// prefix= が指定されていれば、そのテナントの対象だけ返す
	if prefix := r.URL.Query().Get("prefix"); prefix != "" {
		filtered := make([]*Robot, 0, len(list))
		for _, rb := range list {
			if strings.HasPrefix(rb.ID, prefix) {
				filtered = append(filtered, rb)
			}
		}
		list = filtered
	}

	// ids= が指定されていれば、その対象だけ返す（B の鮮度チェック用）
	if ids := r.URL.Query()["ids"]; len(ids) > 0 {
		want := map[string]struct{}{}
		for _, id := range ids {
			want[id] = struct{}{}
		}
		filtered := list[:0]
		for _, rb := range list {
			if _, ok := want[rb.ID]; ok {
				filtered = append(filtered, rb)
			}
		}
		list = filtered
	}

	w.Header().Set("Content-Type", "application/json")
	// API は将来 項目を増やすことがある。受け側は DisallowUnknownFields を
	// 使ってはいけない（§6.3）。ここでも既知でない項目を1つ混ぜてある。
	_, _ = fmt.Fprint(w, `{"schema_version":2,"robots":[`)
	for i, rb := range list {
		if i > 0 {
			_, _ = fmt.Fprint(w, ",")
		}
		b, _ := json.Marshal(rb)
		// 未知の項目を混ぜる
		mixed := string(b[:len(b)-1]) + `,"experimental_field":"ignore me"}`
		_, _ = fmt.Fprint(w, mixed)
	}
	_, _ = fmt.Fprint(w, `]}`)
}

func (s *Service) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	s.mu.Lock()
	noPong := s.noPong
	s.conns[c] = prefix
	s.mu.Unlock()

	// Ping に応答しない設定なら、Pong を返さない（§2.6 の検出対象）
	if noPong {
		c.SetPingHandler(func(string) error { return nil })
	}

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
		for {
			// 読み続けないと Ping/Pong の制御メッセージが処理されない
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// nowRFC3339Milli はミリ秒まで含む RFC3339。
//
// ★秒単位の時刻しか返さないサービスだと、同じ秒の中で起きた変化が
// 「observed_at が進んでいない」と見なされて捨てられる（§2.7 の比較が > のため）。
// その場合は observed_at に加えて連番（シーケンス）を持たせる設計が要る。
func nowRFC3339Milli() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
