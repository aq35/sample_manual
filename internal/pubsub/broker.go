// Package pubsub は EXP-43（複数プロセス跨ぎの SSE fan-out）のための最小の pub/sub。
//
// SSE を複数プロセス（ゲートウェイ）に分けると、in-memory hub はプロセス内だけなので、
// プロセス跨ぎの通知に pub/sub が要る（MySQL に LISTEN/NOTIFY は無い・EXP-38）。
// ここでは Broker インターフェースと、その振る舞いを再現するインメモリ実装を置く
// （実運用は Redis/NATS 等に差し替える。トピックはテナントで分ける）。
package pubsub

import (
	"sync"
	"sync/atomic"
)

// Broker はトピック単位の発行/購読。Publish は fire-and-forget（詰まった購読者には送らず落とす、
// Redis pub/sub 相当）。トピックにテナントIDを使えば、配信はそのテナントに閉じる。
type Broker interface {
	Publish(topic string, v int64)
	Subscribe(topic string) (<-chan int64, func())
}

// MemBroker は同一プロセス内で Broker を再現する（実 Redis/NATS の代わり。意味論の検証用）。
type MemBroker struct {
	buf     int
	mu      sync.Mutex
	next    int
	subs    map[string]map[int]chan int64
	dropped int64
}

func NewMemBroker(buf int) *MemBroker {
	return &MemBroker{buf: buf, subs: make(map[string]map[int]chan int64)}
}

// Publish は topic の全購読者へ非ブロッキングで配る（詰まっていれば落とす）。
func (b *MemBroker) Publish(topic string, v int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[topic] {
		select {
		case ch <- v:
		default:
			atomic.AddInt64(&b.dropped, 1)
		}
	}
}

// Subscribe は topic の受信チャネルと解除関数を返す。
func (b *MemBroker) Subscribe(topic string) (<-chan int64, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[int]chan int64)
	}
	id := b.next
	b.next++
	ch := make(chan int64, b.buf)
	b.subs[topic][id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if m := b.subs[topic]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(b.subs, topic)
			}
		}
	}
}

func (b *MemBroker) Dropped() int64 { return atomic.LoadInt64(&b.dropped) }

// Topics は現在購読者がいるトピック数（掃除の確認用）。
func (b *MemBroker) Topics() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
