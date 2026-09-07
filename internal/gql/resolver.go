package gql

import (
	"context"

	"github.com/aq35/sample_manual/internal/repo"
	"github.com/aq35/sample_manual/internal/ssehub"
)

// Resolver は依存の注入口。テナント束縛済みの *repo.DB だけを持つ。
// リゾルバは context のテナントで db.Tenant(t) を作り、その Scope 経由でしか DB を触らない。
type Resolver struct {
	DB *repo.DB

	// MaxPageSize は robots(first) の上限。クライアントがいくつ要求してもこれで頭打ちにする
	// （無制限一覧を作らせない・DoS 対策）。0 なら defaultMaxPageSize。
	MaxPageSize int

	// Events はサブスクリプション用のテナント単位 hub（nil なら subscription は使えない）。
	// 接続ごとに DB を引かず、ここに相乗りする（EXP-38/39）。
	Events *ssehub.Registry
}

const defaultMaxPageSize = 100

func (r *Resolver) maxPage() int {
	if r.MaxPageSize > 0 {
		return r.MaxPageSize
	}
	return defaultMaxPageSize
}

// scope は context の検証済みテナントから Scope を作る。ここが全リゾルバの DB 入口。
// （resolver ファイルでなくここに置く: gqlgen は resolver ファイルからヘルパを追い出すため）
func (r *Resolver) scope(ctx context.Context) (*repo.Scope, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	return r.DB.Tenant(t), nil
}
