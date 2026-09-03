// Package planlab は EXP-7（実行計画とデータの偏り）の実験本体。
//
// 少量・均等なテストデータで実行計画を「固定」しても、本番の計画とは別物になる。
// ここでは **件数と偏りを変えて同じ問い合わせを測り**、
// 計画・走査行数・遅延がどう動くかを見る。
package planlab

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
)

// Dataset はデータの形。
type Dataset struct {
	Name    string
	Tenants int
	Rows    int  // 合計行数
	Skew    bool // true なら1テナントが 90% を占める
	HotKeys int  // status の種類（少ないほど選択性が低い）
}

// Build は表を作り直してデータを入れる。
func Build(ctx context.Context, db *sql.DB, ds Dataset) error {
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS plan_item"); err != nil {
		return err
	}
	const ddl = `CREATE TABLE plan_item (
		tenant_id  VARCHAR(32) NOT NULL,
		id         INT         NOT NULL,
		status     TINYINT     NOT NULL,
		created_at DATETIME(3) NOT NULL,
		payload    VARCHAR(200) NOT NULL,
		PRIMARY KEY (tenant_id, id),
		KEY idx_tenant_created (tenant_id, created_at),
		KEY idx_tenant_status (tenant_id, status)
	) ENGINE=InnoDB`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return err
	}

	rnd := rand.New(rand.NewSource(20260903))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	payload := strings.Repeat("x", 180)
	statuses := ds.HotKeys
	if statuses <= 0 {
		statuses = 3
	}

	// テナントごとの行数を決める
	counts := make([]int, ds.Tenants)
	if ds.Skew {
		big := ds.Rows * 9 / 10
		counts[0] = big
		rest := ds.Rows - big
		for i := 1; i < ds.Tenants; i++ {
			counts[i] = rest / (ds.Tenants - 1)
		}
	} else {
		for i := range counts {
			counts[i] = ds.Rows / ds.Tenants
		}
	}

	const batch = 500
	var (
		vals []string
		args []any
	)
	flush := func() error {
		if len(vals) == 0 {
			return nil
		}
		q := "INSERT INTO plan_item (tenant_id, id, status, created_at, payload) VALUES " + strings.Join(vals, ",")
		_, err := db.ExecContext(ctx, q, args...)
		vals, args = vals[:0], args[:0]
		return err
	}
	for tn := 0; tn < ds.Tenants; tn++ {
		tenant := fmt.Sprintf("t-%03d", tn)
		for i := 0; i < counts[tn]; i++ {
			vals = append(vals, "(?,?,?,?,?)")
			args = append(args, tenant, i, rnd.Intn(statuses),
				base.Add(time.Duration(i)*time.Second), payload)
			if len(vals) >= batch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "ANALYZE TABLE plan_item")
	return err
}

// Counts はテナントごとの行数（測定の前提を記録するため）。
func Counts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT tenant_id, COUNT(*) FROM plan_item GROUP BY tenant_id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// Query は測る問い合わせ。
type Query struct {
	Name string
	SQL  string
	Args []any
	// Rounds は測定の繰り返し回数。
	Rounds int
}

// Measurement は1つの問い合わせの測定結果。
type Measurement struct {
	Name string
	// Plan は EXPLAIN の要約。
	PlanType  string
	PlanKey   string
	PlanRows  int64
	PlanExtra string
	// Handler は実際に読んだ行数（走査行数の実測）。
	HandlerReadNext int64 // 索引を順にたどって読んだ行
	HandlerReadKey  int64 // 索引で直接引いた回数
	HandlerReadRnd  int64 // 主キーを引き直して読んだ行（二次索引からの往復）
	RowsReturned    int
	Latency         expkit.LatencyStats
	FirstTouch      time.Duration // 1回目（バッファプールが温まっていない可能性がある）
}

// Measure は EXPLAIN と実測を1つの接続で行う。
//
// ★同じ接続で行うのは、Handler_* がセッション単位の値だから。
// プールから別の接続に行くと、他のテストの数字を拾ってしまう。
func Measure(ctx context.Context, db *sql.DB, q Query) (Measurement, error) {
	m := Measurement{Name: q.Name}
	rounds := q.Rounds
	if rounds <= 0 {
		rounds = 20
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return m, err
	}
	defer func() { _ = conn.Close() }()

	// 実行計画
	plan, err := explain(ctx, conn, q)
	if err != nil {
		return m, err
	}
	m.PlanType, m.PlanKey, m.PlanRows, m.PlanExtra = plan.typ, plan.key, plan.rows, plan.extra

	// 1回目（初回アクセス）
	start := time.Now()
	n, err := run(ctx, conn, q)
	if err != nil {
		return m, err
	}
	m.FirstTouch = time.Since(start)
	m.RowsReturned = n

	// 走査行数（1回ぶんを Handler_* の差分で取る）
	before, err := handlerStats(ctx, conn)
	if err != nil {
		return m, err
	}
	if _, err := run(ctx, conn, q); err != nil {
		return m, err
	}
	after, err := handlerStats(ctx, conn)
	if err != nil {
		return m, err
	}
	m.HandlerReadNext = after["Handler_read_next"] - before["Handler_read_next"]
	m.HandlerReadKey = after["Handler_read_key"] - before["Handler_read_key"]
	m.HandlerReadRnd = after["Handler_read_rnd_next"] - before["Handler_read_rnd_next"] +
		after["Handler_read_rnd"] - before["Handler_read_rnd"]

	// 遅延（温まった状態で）
	lat := expkit.NewLatency()
	for i := 0; i < rounds; i++ {
		start := time.Now()
		if _, err := run(ctx, conn, q); err != nil {
			return m, err
		}
		lat.Record(time.Since(start))
	}
	m.Latency = lat.Stats()
	return m, nil
}

type planRow struct {
	typ, key, extra string
	rows            int64
}

func explain(ctx context.Context, conn *sql.Conn, q Query) (planRow, error) {
	var p planRow
	rows, err := conn.QueryContext(ctx, "EXPLAIN "+q.SQL, q.Args...)
	if err != nil {
		return p, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return p, err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return p, err
		}
		get := func(name string) string {
			for i, c := range cols {
				if strings.EqualFold(c, name) && vals[i] != nil {
					if b, ok := vals[i].([]byte); ok {
						return string(b)
					}
					return fmt.Sprint(vals[i])
				}
			}
			return ""
		}
		p.typ = get("type")
		p.key = get("key")
		p.extra = get("Extra")
		_, _ = fmt.Sscan(get("rows"), &p.rows)
		break // 単一表の問い合わせだけを扱う
	}
	return p, rows.Err()
}

func run(ctx context.Context, conn *sql.Conn, q Query) (int, error) {
	rows, err := conn.QueryContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func handlerStats(ctx context.Context, conn *sql.Conn) (map[string]int64, error) {
	rows, err := conn.QueryContext(ctx, "SHOW SESSION STATUS LIKE 'Handler_read%'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// NPlusOne は「1件ずつ引く」を測る（比較用）。
func NPlusOne(ctx context.Context, db *sql.DB, tenant string, ids []int) (time.Duration, int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	n := 0
	for _, id := range ids {
		var status int
		var payload string
		//smlint:allow loopquery 理由: EXP-7 の実験対象。N+1 の遅さを測るための実装
		err := conn.QueryRowContext(ctx,
			"SELECT status, payload FROM plan_item WHERE tenant_id = ? AND id = ?", tenant, id).
			Scan(&status, &payload)
		if err != nil {
			return 0, n, err
		}
		n++
	}
	return time.Since(start), n, nil
}

// InClause は「まとめて引く」を測る（比較用）。
func InClause(ctx context.Context, db *sql.DB, tenant string, ids []int) (time.Duration, int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()

	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, tenant)
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	for _, id := range sorted {
		args = append(args, id)
	}
	start := time.Now()
	n, err := run(ctx, conn, Query{
		SQL:  "SELECT id, status, payload FROM plan_item WHERE tenant_id = ? AND id IN (" + ph + ")",
		Args: args,
	})
	return time.Since(start), n, err
}
