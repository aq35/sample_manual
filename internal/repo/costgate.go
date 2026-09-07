package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ErrTooCostly は「実行前に EXPLAIN したら、走査見込みが予算を超えていた」エラー。
//
// ★ErrTooManyRows は「返す行数」の上限、これは「走査する行数」の上限。
// 索引が効かずテーブルを舐めるクエリ（DDoS 的な問い合わせ）を、**実行する前に**弾く。
// EXP-18 で見たとおり、重さは走査/返す行数で決まる。ここで走査見込みに天井をかける。
var ErrTooCostly = fmt.Errorf("走査見込みが予算を超えている")

// MaxScanRows は Options に無い場合の既定の走査見込み上限。
const defaultMaxScanRows = 50000

// maxScan は走査見込みの上限（Options.MaxScanRows が 0 なら既定）。
func (d *DB) maxScan() int64 {
	if d.opt.MaxScanRows > 0 {
		return int64(d.opt.MaxScanRows)
	}
	return defaultMaxScanRows
}

// CheckCost は EXPLAIN して、どのステップの走査見込みも上限以下かを確かめる。
// 上限は maxRows（0 なら Options.MaxScanRows / 既定）。全表走査は無条件で拒否する。
func (s *Scope) CheckCost(ctx context.Context, op, query string, maxRows int64, args ...any) error {
	plan, err := s.Explain(ctx, op, query, args...)
	if err != nil {
		return err
	}
	limit := maxRows
	if limit <= 0 {
		limit = s.db.maxScan()
	}
	// ★EXPLAIN の rows は「範囲の見積もり」であって、LIMIT + 索引順のときは
	// 実際に読む行数はもっと少ない（keyset 一覧が典型: WHERE id > ? ORDER BY id LIMIT 50 は
	// 見積もりが大きくても、LIMIT で止まる）。だから走査見込みで弾くのは、
	//   - 全表走査（type=ALL）
	//   - filesort / temporary（範囲全体を materialize してから並べ替え・LIMIT）
	//   - LIMIT が無い（索引で止められない）
	// のいずれかで、見積もりがそのまま読まれる場合に限る。
	bounded := hasLimit(query)
	for _, p := range plan {
		if p.FullScan() {
			return fmt.Errorf("%w: %s が全表走査（type=ALL）。索引で絞ること\nSQL: %s", ErrTooCostly, p.Table, query)
		}
		materializes := p.UsesFilesort() || p.UsesTemporary()
		if p.Rows > limit && (materializes || !bounded) {
			why := "LIMIT で止められない"
			if materializes {
				why = "範囲全体を materialize する（" + p.Extra + "）"
			}
			return fmt.Errorf("%w: 走査見込み %d 行 > 上限 %d 行（%s・%s）\nSQL: %s",
				ErrTooCostly, p.Rows, limit, p.Table, why, query)
		}
	}
	return nil
}

func hasLimit(query string) bool {
	return strings.Contains(strings.ToUpper(query), "LIMIT")
}

// GuardedQuery は、先に CheckCost で走査見込みを確かめてから Query する。
//
//	rows, err := sc.GuardedQuery(ctx, "profile.search", 5000, q, args...)
//
// EXPLAIN のぶん往復が1回増えるので、ホットパスではなく「外から来る検索」に使う。
func (s *Scope) GuardedQuery(ctx context.Context, op string, maxRows int64, query string, args ...any) (*sql.Rows, error) {
	if err := s.CheckCost(ctx, op, query, maxRows, args...); err != nil {
		return nil, err
	}
	return s.Query(ctx, op, query, args...)
}
