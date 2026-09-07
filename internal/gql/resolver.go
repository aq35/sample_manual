package gql

import "github.com/aq35/sample_manual/internal/repo"

// Resolver は依存の注入口。テナント束縛済みの *repo.DB だけを持つ。
// リゾルバは context のテナントで db.Tenant(t) を作り、その Scope 経由でしか DB を触らない。
type Resolver struct {
	DB *repo.DB

	// MaxPageSize は robots(first) の上限。クライアントがいくつ要求してもこれで頭打ちにする
	// （無制限一覧を作らせない・DoS 対策）。0 なら defaultMaxPageSize。
	MaxPageSize int
}

const defaultMaxPageSize = 100

func (r *Resolver) maxPage() int {
	if r.MaxPageSize > 0 {
		return r.MaxPageSize
	}
	return defaultMaxPageSize
}
