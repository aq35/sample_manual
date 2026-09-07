package gql

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// ロール（テナント内の権限）も、テナントと同じく「主体」から決める。引数からは取らない。
// 認証ミドルウェアが検証済みロールを context に載せ、@auth ディレクティブがそれを見る。

type roleKey struct{}

// WithRole は検証済みロールを context に載せる（認証ミドルウェアが呼ぶ）。
func WithRole(ctx context.Context, role Role) context.Context {
	return context.WithValue(ctx, roleKey{}, role)
}

func roleFrom(ctx context.Context) (Role, bool) {
	r, ok := ctx.Value(roleKey{}).(Role)
	return r, ok
}

// rank は VIEWER < OPERATOR < ADMIN の強さ。未知は最弱。
func rank(r Role) int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// authDirective は @auth(requires:) の実装。解決関数の「前に」ロールを検査し、
// 足りなければ next を呼ばずに拒否する（＝DB にも触らない）。
func authDirective(ctx context.Context, obj any, next graphql.Resolver, requires Role) (any, error) {
	role, ok := roleFrom(ctx)
	if !ok || rank(role) < rank(requires) {
		return nil, gqlerror.Errorf("forbidden: %s 権限が必要", requires)
	}
	return next(ctx)
}
