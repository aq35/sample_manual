package gql

import "github.com/vektah/gqlparser/v2/gqlerror"

// クライアント起因のエラー（入力不正・認可拒否・受付拒否）は、そのメッセージを client に見せてよい。
// 一方、内部エラー（SQL 等）は秘匿する。userErr で作ったものだけを「公開してよい」と印付けし、
// エラープレゼンタがそれ以外を "internal error" に一般化する。

const publicCode = "USER_ERROR"

// userErr はクライアントに見せてよいエラーを作る（拡張に公開印を付ける）。
func userErr(format string, a ...any) *gqlerror.Error {
	e := gqlerror.Errorf(format, a...)
	if e.Extensions == nil {
		e.Extensions = map[string]any{}
	}
	e.Extensions["code"] = publicCode
	return e
}

// isPublic は userErr で作られた（公開してよい）エラーか。
func isPublic(e *gqlerror.Error) bool {
	if e == nil || e.Extensions == nil {
		return false
	}
	code, _ := e.Extensions["code"].(string)
	return code == publicCode
}
