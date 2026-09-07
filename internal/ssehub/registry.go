package ssehub

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Registry はテナント→hub を1つに束ねる。テナント初の購読者で hub＋poller を起動し、
// 最後の購読者が抜けたら poller を止める（空テナントで DB を回し続けない）。
//
// これで「接続がいくら増えても DB 読みは poller の分だけ」を1コンテナ内で実現する。
type Registry struct {
	load     Loader
	interval time.Duration
	buf      int

	mu    sync.Mutex
	hubs  map[string]*entry
	reads int64 // poller が DB を読んだ総回数（テスト・監視用）
}

// Loader はテナントの現在の版を1回読む関数（DB アクセスをここに閉じる）。
type Loader func(ctx context.Context, tenant string) (int64, error)

type entry struct {
	hub    *Hub
	cancel context.CancelFunc
	refs   int
	last   int64 // 直近に配った版（新規購読者への初期値。-1 は未取得）
}

// NewRegistry は load を interval ごとに呼ぶレジストリを作る。buf は購読チャネルのバッファ。
func NewRegistry(load Loader, interval time.Duration, buf int) *Registry {
	return &Registry{load: load, interval: interval, buf: buf, hubs: make(map[string]*entry)}
}

// Reads は poller が DB を読んだ総回数。
func (r *Registry) Reads() int64 { return atomic.LoadInt64(&r.reads) }

// ActiveTenants は今 hub＋poller が動いているテナント数。
func (r *Registry) ActiveTenants() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hubs)
}

// Subscribe はテナントの hub に相乗りする。受信チャネル・初期版（-1 は未取得）・解放関数を返す。
func (r *Registry) Subscribe(tenant string) (<-chan int64, int64, func()) {
	r.mu.Lock()
	e := r.hubs[tenant]
	if e == nil { // このテナント初の購読者 → hub＋poller を起動（DB IO はロック外の poller で行う）
		ctx, cancel := context.WithCancel(context.Background())
		e = &entry{hub: NewHub(r.buf), cancel: cancel, last: -1}
		r.hubs[tenant] = e
		go r.poll(ctx, tenant, e)
	}
	e.refs++
	id, ch := e.hub.Subscribe()
	last := atomic.LoadInt64(&e.last)
	r.mu.Unlock()
	return ch, last, func() { r.release(tenant, id) }
}

func (r *Registry) release(tenant string, id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.hubs[tenant]
	if e == nil {
		return
	}
	e.hub.Unsubscribe(id)
	e.refs--
	if e.refs == 0 { // 最後の1人が抜けた → poller を止めて掃除
		e.cancel()
		delete(r.hubs, tenant)
	}
}

// poll はテナントに1本。interval ごとに load し、版が変わったら全購読者へ配る（coalesce）。
func (r *Registry) poll(ctx context.Context, tenant string, e *entry) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			atomic.AddInt64(&r.reads, 1)
			v, err := r.load(ctx, tenant)
			if err != nil {
				continue // 次の tick で再試行（DB 一時障害は EXP-33）
			}
			if v != atomic.LoadInt64(&e.last) { // 変化時だけ配る
				atomic.StoreInt64(&e.last, v)
				e.hub.Broadcast(v)
			}
		}
	}
}

// Handler は SSE エンドポイント。テナントは tenantOf（認証済み）から取る。引数/URL から取らない。
func (r *Registry) Handler(tenantOf func(*http.Request) (string, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		tenant, ok := tenantOf(req)
		if !ok || tenant == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // nginx 等のバッファ無効化
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, initial, release := r.Subscribe(tenant)
		defer release() // 切断時に必ず購読解除（漏れ防止）

		if initial >= 0 { // 初期スナップショット（poller のキャッシュ。接続ごとの DB 読みは無い）
			fmt.Fprintf(w, "data: %d\n\n", initial)
			flusher.Flush()
		}
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case v := <-ch:
				fmt.Fprintf(w, "data: %d\n\n", v)
				flusher.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n") // idle 切断防止＋死活
				flusher.Flush()
			case <-req.Context().Done(): // クライアント切断
				return
			}
		}
	}
}
