package gql

import (
	"context"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
)

// レート制限（テナントごとのトークンバケツ）。
//
// GraphQL は1エンドポイントなので、URL 単位のレート制限では粗い。ここでは「主体（テナント）」
// ごとにバケツを持ち、1テナントの暴走が他テナントを巻き込まないようにする。複雑度上限が
// 「1クエリの重さ」を抑えるのに対し、これは「単位時間の本数」を抑える。

// RateLimiter はテナントごとのトークンバケツ。rate=毎秒補充、burst=バケツ容量。
type RateLimiter struct {
	rate  float64
	burst float64
	now   func() time.Time // テスト用に差し替え可能

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter は毎秒 rate 本・バースト burst 本の制限を作る。
func NewRateLimiter(rate, burst float64) *RateLimiter {
	return NewRateLimiterWithClock(rate, burst, time.Now)
}

// NewRateLimiterWithClock は時計を差し替えられる版（テストで決定的にするため）。
func NewRateLimiterWithClock(rate, burst float64, now func() time.Time) *RateLimiter {
	return &RateLimiter{rate: rate, burst: burst, now: now, buckets: map[string]*bucket{}}
}

// Allow は key（テナント）に1本ぶんのトークンがあれば消費して true。
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	t := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		rl.buckets[key] = &bucket{tokens: rl.burst - 1, last: t}
		return true
	}
	// 経過ぶん補充する。
	b.tokens += rl.rate * t.Sub(b.last).Seconds()
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = t
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// --- gqlgen 拡張として差し込む（テナントが context に載った後の operation 段で効く）---

func (rl *RateLimiter) ExtensionName() string                  { return "TenantRateLimit" }
func (rl *RateLimiter) Validate(graphql.ExecutableSchema) error { return nil }

// InterceptOperation はテナント単位でレート制限し、超過時は実行させずにエラーを返す。
func (rl *RateLimiter) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	t, err := tenantFrom(ctx)
	if err != nil {
		return next(ctx) // テナント未設定は認証段で弾かれる（ここでは素通し）
	}
	if !rl.Allow(string(t)) {
		return graphql.OneShot(graphql.ErrorResponse(ctx, "rate limited（テナントの単位時間あたり本数を超過）"))
	}
	return next(ctx)
}
