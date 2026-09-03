// Package effectsvc は「外部サービス」の代役。
//
// EXP-1 で確かめたいのは **DB の中の状態ではなく、外の世界で何回 effect が起きたか**。
// そのため、effect の回数はアプリの DB ではなく、このサービス側にファイルで永続化する。
// アプリが SIGKILL されても、外の世界に残った事実は消えない、という状況を作るため。
package effectsvc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"
)

// Effect は外の世界で1回起きた出来事（課金・送信など）。
type Effect struct {
	Seq        int64     `json:"seq"`
	Key        string    `json:"idempotency_key"`
	RequestID  string    `json:"request_id"`
	Amount     int       `json:"amount"`
	ReceiptID  string    `json:"receipt_id"`
	AppliedAt  time.Time `json:"applied_at"`
	Duplicated bool      `json:"duplicated"` // 冪等キーで弾かれた再送か
}

// Behavior はサービス側の振る舞い（故障注入）。
type Behavior struct {
	// HonorIdempotencyKey: 冪等キーを見て重複を弾くか。
	// ★false にすると「相手が冪等キーを守らない外部サービス」を再現できる。
	HonorIdempotencyKey bool
	// Delay: effect を起こす前に待つ時間。
	Delay time.Duration
	// HangAfterEffect: effect を起こしたあと応答を返さない（クライアントは timeout する）。
	HangAfterEffect bool
	// FailAfterEffect: effect を起こしたあと 500 を返す。
	//   ★「エラーが返った＝effect が起きていない」ではないことを再現する。
	FailAfterEffect bool
	// OnEffect: effect が成立した直後に呼ばれる。
	//   テストはここでクライアント（子プロセス）を SIGKILL し、
	//   「effect 成立後・応答前」に落ちた状況をぴったり作る。
	OnEffect func(e Effect)
}

// Service は外部サービスの代役。
type Service struct {
	mu       sync.Mutex
	behavior Behavior
	effects  []Effect
	byKey    map[string]Effect
	seq      int64
	logPath  string

	// hang 中のリクエストを終わらせるための合図。
	// これが無いと httptest.Server.Close() が応答待ちのまま返らない。
	closing   chan struct{}
	closeOnce sync.Once

	srv *httptest.Server
}

// New はサービスを起動する。logPath に effect を追記していく（外の世界の記録）。
func New(logPath string, b Behavior) *Service {
	s := &Service{
		behavior: b,
		byKey:    map[string]Effect{},
		logPath:  logPath,
		closing:  make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/charge", s.handleCharge)
	mux.HandleFunc("/effects", s.handleEffects)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *Service) Close() {
	// hang させている応答を先に解放してから閉じる。
	// これをしないと httptest.Server.Close() が応答待ちのまま返らない。
	s.closeOnce.Do(func() { close(s.closing) })
	s.srv.CloseClientConnections()
	s.srv.Close()
}
func (s *Service) URL() string { return s.srv.URL }

// SetBehavior は途中で振る舞いを変える。
func (s *Service) SetBehavior(b Behavior) {
	s.mu.Lock()
	s.behavior = b
	s.mu.Unlock()
}

// Effects は起きた effect の一覧（重複判定で弾いたものは含まない）。
func (s *Service) Effects() []Effect {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Effect, len(s.effects))
	copy(out, s.effects)
	return out
}

// CountByRequest は request_id ごとの effect 回数。
// 1 を超えていれば二重送信、0 なら未実行。
func (s *Service) CountByRequest() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, e := range s.effects {
		out[e.RequestID]++
	}
	return out
}

// handleCharge は effect を1回起こす。
func (s *Service) handleCharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"request_id"`
		Amount    int    `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Idempotency-Key")

	s.mu.Lock()
	b := s.behavior
	if b.HonorIdempotencyKey && key != "" {
		if prev, ok := s.byKey[key]; ok {
			// 同じキーの再送。**effect は起こさず、前回の受領書を返す**
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"receipt_id": prev.ReceiptID,
				"duplicate":  true,
			})
			return
		}
	}
	s.mu.Unlock()

	if b.Delay > 0 {
		time.Sleep(b.Delay)
	}

	// ---- ここで effect が成立する（外の世界が変わる） ----
	s.mu.Lock()
	s.seq++
	e := Effect{
		Seq:       s.seq,
		Key:       key,
		RequestID: req.RequestID,
		Amount:    req.Amount,
		ReceiptID: fmt.Sprintf("rcpt-%06d", s.seq),
		AppliedAt: time.Now().UTC(),
	}
	s.effects = append(s.effects, e)
	if key != "" {
		s.byKey[key] = e
	}
	s.appendLogLocked(e)
	hook := s.behavior.OnEffect
	s.mu.Unlock()

	if hook != nil {
		hook(e) // テストはここで子プロセスを殺す
	}
	if b.HangAfterEffect {
		// 応答を返さない（クライアントから見れば「送ったが結果が分からない」状態）。
		// サービスを閉じるときだけ解放する
		<-s.closing
		return
	}
	if b.FailAfterEffect {
		// ★effect は起きているのに 500 を返す
		http.Error(w, "internal error after effect", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"receipt_id": e.ReceiptID,
		"duplicate":  false,
	})
}

// handleEffects は「独立した観測」のための問い合わせ。
//
// ★これがあることが決定的に重要。応答をもらえなかったとき、
// 「起きたのか起きていないのか」を **相手に聞く** 以外に確かめる方法は無い。
func (s *Service) handleEffects(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	reqID := r.URL.Query().Get("request_id")

	s.mu.Lock()
	defer s.mu.Unlock()
	var found []Effect
	for _, e := range s.effects {
		if (key != "" && e.Key == key) || (reqID != "" && e.RequestID == reqID) {
			found = append(found, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"effects": found})
}

func (s *Service) appendLogLocked(e Effect) {
	if s.logPath == "" {
		return
	}
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync() // 外の世界の記録は、落ちても消えないようにする
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
