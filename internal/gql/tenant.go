package gql

import (
	"context"
	"errors"

	"github.com/aq35/sample_manual/internal/model"
)

// テナントは「リクエストの主体」から決まる。GraphQL の引数や変数からは絶対に取らない
// （取ると、クライアントが tenant_id を詐称して他テナントを読める＝EXP-24 の事故）。
// 認証ミドルウェアが検証済みのテナントを context に載せ、リゾルバはそこからしか読まない。

type tenantKey struct{}

// ErrNoTenant はテナント未設定（認証ミドルウェアを通っていない）。
var ErrNoTenant = errors.New("gql: リクエストにテナントがない（認証ミドルウェア未通過）")

// WithTenant は検証済みテナントを context に載せる（認証ミドルウェアが呼ぶ）。
func WithTenant(ctx context.Context, t model.TenantID) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// tenantFrom は context のテナントを返す。無ければエラー（黙って全テナントを見せない）。
func tenantFrom(ctx context.Context) (model.TenantID, error) {
	t, ok := ctx.Value(tenantKey{}).(model.TenantID)
	if !ok || t == "" {
		return "", ErrNoTenant
	}
	return t, nil
}
