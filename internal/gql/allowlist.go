package gql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// 永続化クエリ（trusted documents / allowlist）。
//
// APQ は「一度見たクエリ」をキャッシュするだけで、任意のクエリを受け付ける。allowlist は
// 「登録済みのクエリ以外は実行しない」。攻撃者が自由な深い/広いクエリを投げる余地を消す
// （複雑度上限と併用するとさらに堅い）。本番クライアントのクエリは有限個なので、その
// ハッシュを事前登録しておく。

// AllowList は許可するクエリの sha256（hex）集合。
type AllowList struct {
	hashes map[string]struct{}
}

// NewAllowList は生クエリ文字列群から allowlist を作る（ビルド時に既知のクエリを登録）。
func NewAllowList(queries ...string) *AllowList {
	a := &AllowList{hashes: make(map[string]struct{}, len(queries))}
	for _, q := range queries {
		a.hashes[HashQuery(q)] = struct{}{}
	}
	return a
}

// HashQuery は allowlist 判定に使う正規化ハッシュ（そのままの生クエリの sha256）。
func HashQuery(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// ExtensionName は graphql.HandlerExtension のため。
func (a *AllowList) ExtensionName() string { return "PersistedQueryAllowList" }

// Validate は graphql.HandlerExtension のため（設定時に一度呼ばれる）。
func (a *AllowList) Validate(graphql.ExecutableSchema) error { return nil }

// MutateOperationContext は、実行前に生クエリが allowlist にあるか検査する。
// 無ければ実行させない（パース・実行の前段で弾く）。
func (a *AllowList) MutateOperationContext(ctx context.Context, oc *graphql.OperationContext) *gqlerror.Error {
	if _, ok := a.hashes[HashQuery(oc.RawQuery)]; !ok {
		return gqlerror.Errorf("query not allowed（未登録のクエリ・allowlist 外）")
	}
	return nil
}
