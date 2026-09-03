package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// PlanRow は EXPLAIN の1行（見るのはこれだけで足りる）。
type PlanRow struct {
	Table    string
	Type     string // ALL なら全表走査。ref / range / const / eq_ref なら索引を使えている
	Key      string // 実際に使われた索引。空なら索引なし
	Rows     int64  // 走査見込み行数
	Filtered float64
	Extra    string
}

func (p PlanRow) String() string {
	return fmt.Sprintf("table=%s type=%s key=%s rows=%d filtered=%.0f extra=%s",
		p.Table, p.Type, orDash(p.Key), p.Rows, p.Filtered, orDash(p.Extra))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// FullScan は全表走査になっているか。
func (p PlanRow) FullScan() bool { return strings.EqualFold(p.Type, "ALL") }

// UsesFilesort は Extra に Using filesort が出ているか（並べ替えが索引で済んでいない）。
func (p PlanRow) UsesFilesort() bool { return strings.Contains(p.Extra, "Using filesort") }

// UsesTemporary は一時表を作っているか。
func (p PlanRow) UsesTemporary() bool { return strings.Contains(p.Extra, "Using temporary") }

// Explain は実行計画を取る。SQL は Query と同じ書き方（:tenant を含む）でよい。
//
// 「速いか」ではなく「どう読むか」を見る。実行計画はデータ量が増えても変わらないので、
// **テストで固定できる**（repotest.RequireIndexed）。
func (s *Scope) Explain(ctx context.Context, op, query string, args ...any) ([]PlanRow, error) {
	// EXPLAIN は行を返さないので、LIMIT の有無は問わない
	o := s.opts()
	o.singleRow = true
	bound, boundArgs, err := s.prepareWith(op, query, args, o)
	if err != nil {
		return nil, err
	}
	rows, err := s.ex.QueryContext(ctx, "EXPLAIN "+bound, boundArgs...)
	if err != nil {
		return nil, wrap(op, string(s.tenant), query, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []PlanRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		get := func(name string) string {
			for i, c := range cols {
				if !strings.EqualFold(c, name) || vals[i] == nil {
					continue
				}
				if b, ok := vals[i].([]byte); ok {
					return string(b)
				}
				return fmt.Sprint(vals[i])
			}
			return ""
		}
		var p PlanRow
		p.Table = get("table")
		p.Type = get("type")
		p.Key = get("key")
		p.Extra = get("Extra")
		_, _ = fmt.Sscan(get("rows"), &p.Rows)
		_, _ = fmt.Sscan(get("filtered"), &p.Filtered)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ExplainDB は :tenant を使わない素の SQL 用（移行中の調査などに使う）。
func ExplainDB(ctx context.Context, db *sql.DB, query string, args ...any) ([]PlanRow, error) {
	d := &DB{sqldb: db}
	d.opt.setDefaults()
	s := &Scope{db: d, tenant: "*", ex: db, crossTenant: true, unbounded: true}
	return s.Explain(ctx, "explain", query, args...)
}
