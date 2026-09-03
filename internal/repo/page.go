package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// Keyset は「前ページの最後のキーから続ける」ページ送り。
//
// 実験（TestExperiment_ページ送り）のとおり、OFFSET は捨てる行も読むので
// 深くなるほど比例して遅くなる。キーセット法は深さに依らない。
//
//	OFFSET     : 先頭 539µs → 4,000件目 2.08ms
//	キーセット : 先頭 465µs → 4,000件目  395µs
type Keyset struct {
	// After は前ページの最後のキー。空なら先頭から。
	After string
	// Limit は1ページの件数。0 なら 100。DB.Options().MaxRows を超えられない。
	Limit int
}

// Page は1ページぶんの結果。
type Page[T any] struct {
	Items []T
	// Next は次ページの Keyset.After。空なら次は無い。
	Next string
	// HasMore は次ページがあるか。
	HasMore bool
}

// Paginate はキーセット法のページ送りを行う。
//
// SQL は「前ページの最後のキーより後ろ」を最後の2引数（after, limit）で受ける形で書く。
//
//	const q = `SELECT robot_id, name FROM robot_profile
//	           WHERE tenant_id = :tenant AND robot_id > ?
//	           ORDER BY robot_id LIMIT ?`
//
// limit は「次があるか」を判定するため +1 して渡される（呼び出し側は意識しなくてよい）。
func Paginate[T any](ctx context.Context, s *Scope, op, query string, ks Keyset,
	scan func(*sql.Rows) (item T, cursor string, err error), args ...any) (Page[T], error) {

	limit := ks.Limit
	if limit <= 0 {
		limit = 100
	}
	if maxRows := s.db.opt.MaxRows; limit > maxRows {
		return Page[T]{}, wrap(op, string(s.tenant), query,
			fmt.Errorf("%w: 1ページ %d 件は上限 %d 件を超えている", ErrTooManyRows, limit, maxRows))
	}

	full := append(append([]any{}, args...), ks.After, limit+1)
	rows, err := s.Query(ctx, op, query, full...)
	if err != nil {
		return Page[T]{}, err
	}
	defer func() { _ = rows.Close() }()

	var page Page[T]
	page.Items = make([]T, 0, limit)
	for rows.Next() {
		item, cursor, err := scan(rows)
		if err != nil {
			return Page[T]{}, wrap(op, string(s.tenant), query, err)
		}
		if len(page.Items) == limit {
			// limit+1 件目。次があることだけ分かればよい
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, item)
		page.Next = cursor
	}
	if err := rows.Err(); err != nil {
		return Page[T]{}, wrap(op, string(s.tenant), query, err)
	}
	if !page.HasMore {
		page.Next = ""
	}
	return page, nil
}
