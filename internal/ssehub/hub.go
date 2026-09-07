// Package ssehub は EXP-38（同一テナントで多数が SSE 購読するときのアプリ側の対応）の実験本体。
//
// 素朴だと、300 接続が各自 DB をポーリング＝同じテナントのデータに 300 倍の読み負荷（ファンイン）。
// 対応:
//   1. テナントに1つの poller が DB を1回引き、hub で全購読者へ配る（300 クエリ → 1）。
//   2. 一斉接続時の初期スナップショットは singleflight で1回に畳む（stampede 防止）。
//   3. 遅い購読者は他をブロックしない（有界バッファ＋非ブロッキング送信で落とす/畳む）。
package ssehub

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

// --- DB 側（テナントの「版」。データが変わると version が上がる）---

func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS subq",
		`CREATE TABLE subq (tenant_id VARCHAR(32) PRIMARY KEY, version BIGINT NOT NULL) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func Seed(ctx context.Context, db *sql.DB, tenant string) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO subq (tenant_id, version) VALUES (?, 0) ON DUPLICATE KEY UPDATE version=0", tenant)
	return err
}

// Load はテナントの現在の版を DB から読む（＝SSE が配る中身の代理）。呼ぶたび hits を1増やす。
func Load(ctx context.Context, db *sql.DB, tenant string, hits *int64) (int64, error) {
	atomic.AddInt64(hits, 1)
	var v int64
	err := db.QueryRowContext(ctx, "SELECT version FROM subq WHERE tenant_id=?", tenant).Scan(&v)
	return v, err
}

// Bump はテナントの版を1つ進める（データが変わったことを模す）。
func Bump(ctx context.Context, db *sql.DB, tenant string) error {
	//smlint:allow rowsaffected 理由: 版を進めるだけ。件数は使わない
	_, err := db.ExecContext(ctx, "UPDATE subq SET version=version+1 WHERE tenant_id=?", tenant)
	return err
}

// Snapshot は singleflight で「同時に来た初期スナップショット取得」を1回の DB 読みに畳む。
func Snapshot(ctx context.Context, sf *singleflight.Group, db *sql.DB, tenant string, hits *int64) (int64, error) {
	v, err, _ := sf.Do(tenant, func() (any, error) {
		return Load(ctx, db, tenant, hits)
	})
	if err != nil {
		return 0, err
	}
	return v.(int64), nil
}

// --- fan-out hub（テナントに1つ。poller が1回引いた版を全購読者へ配る）---

type sub struct {
	ch chan int64
}

// Hub は1テナントぶんの購読者管理。有界バッファで、遅い購読者は落とす（他をブロックしない）。
type Hub struct {
	buf     int
	mu      sync.Mutex
	nextID  int
	subs    map[int]*sub
	dropped int64
}

func NewHub(buf int) *Hub {
	return &Hub{buf: buf, subs: make(map[int]*sub)}
}

// Subscribe は購読者を1つ足し、その受信チャネルを返す。
func (h *Hub) Subscribe() (int, <-chan int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	s := &sub{ch: make(chan int64, h.buf)}
	h.subs[id] = s
	return id, s.ch
}

func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, id)
}

// Broadcast は版 v を全購読者へ非ブロッキングで配る。バッファが一杯の購読者には送らず落とす
// （遅い1人が全体を止めない）。落とした件数は Dropped に積む。
func (h *Hub) Broadcast(v int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		select {
		case s.ch <- v:
		default:
			atomic.AddInt64(&h.dropped, 1) // この購読者は詰まっている → 落とす（最新版はまた来る）
		}
	}
}

func (h *Hub) SubCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) Dropped() int64 { return atomic.LoadInt64(&h.dropped) }
