package config

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SecretFetch は秘密を取ってくる関数（SecretManager 等）。値と有効期限を返す。
type SecretFetch func(ctx context.Context) (value string, expiresAt time.Time, err error)

// ManagedSecret は「必須で・期限があり・期限前に自動更新される」設定値。
//
// 環境変数は期限を持たないので、期限つきの秘密（DB 資格情報・API 鍵）はこちらで扱う。
//   - 起動時に一度取得し、取れなければ fail-fast（Loader.Err に積む）。
//   - 期限の手前で先回りして更新する（期限ちょうどで切れるのを待たない）。
//   - 更新に失敗しても、期限内の古い値は握ったまま延命する（fail-open）。
type ManagedSecret struct {
	name          string
	fetch         SecretFetch
	refreshBefore time.Duration

	mu        sync.RWMutex
	value     string
	expiresAt time.Time
	loaded    bool

	stop chan struct{}
	once sync.Once
}

// RequiredSecret は必須の期限つき秘密を読む。
//
// 起動時に一度 fetch し、取れなければ Loader.Err に積む（fail-fast）。
// 成功したら Start() で先回り更新のループを回せる。
func (l *Loader) RequiredSecret(name string, fetch SecretFetch, refreshBefore time.Duration) *ManagedSecret {
	if refreshBefore <= 0 {
		refreshBefore = 30 * time.Second
	}
	s := &ManagedSecret{name: name, fetch: fetch, refreshBefore: refreshBefore, stop: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, exp, err := fetch(ctx)
	if err != nil {
		l.missing = append(l.missing, fmt.Sprintf("%s（秘密を取得できない: %v）", name, err))
		l.used[name] = "MISSING(secret)"
		return s
	}
	s.set(v, exp)
	l.used[name] = "secret(env外)"
	return s
}

func (s *ManagedSecret) set(v string, exp time.Time) {
	s.mu.Lock()
	s.value, s.expiresAt, s.loaded = v, exp, true
	s.mu.Unlock()
}

// Value は今の値。ExpiresAt を過ぎていても、更新が間に合っていなければ古い値を返す
// （呼び出し側は Expired() で判断できる）。
func (s *ManagedSecret) Value() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// ExpiresAt は現在値の有効期限。
func (s *ManagedSecret) ExpiresAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}

// Expired は期限を過ぎているか。
func (s *ManagedSecret) Expired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded && time.Now().After(s.expiresAt)
}

// Start は先回り更新のループを回す（1つの ManagedSecret につき1本）。
// onRotate は更新が成功して値が変わったときに呼ばれる（プールの入れ替え等に使う）。
func (s *ManagedSecret) Start(onRotate func(newValue string)) {
	s.once.Do(func() {
		go s.loop(onRotate)
	})
}

// Stop は更新ループを止める。
func (s *ManagedSecret) Stop() { close(s.stop) }

func (s *ManagedSecret) loop(onRotate func(string)) {
	for {
		s.mu.RLock()
		wait := time.Until(s.expiresAt) - s.refreshBefore
		s.mu.RUnlock()
		if wait < 0 {
			wait = 0
		}
		select {
		case <-s.stop:
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		v, exp, err := s.fetch(ctx)
		cancel()
		if err != nil {
			// 失敗しても古い値は残す（fail-open）。少し待って再試行。
			select {
			case <-s.stop:
				return
			case <-time.After(s.refreshBefore / 3):
			}
			continue
		}
		changed := v != s.Value()
		s.set(v, exp)
		if changed && onRotate != nil {
			onRotate(v)
		}
	}
}
