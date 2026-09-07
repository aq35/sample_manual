// Package datelab は EXP-18（日付範囲検索が何件で重くなるか）の実験本体。
//
// 問い: 索引を張っても、レコード数が何件あたりで日付検索が重くなるか。
//
// 主張（測って確かめる）:
//   - 重さは「テーブルの総行数」ではなく「範囲がヒットして走査/返す行数」で決まる。
//   - 索引が (tenant_id, observed_date) なら、日付範囲はテナント局所の range scan。
//     総行数が増えても、同じ日付幅なら所要はほぼ変わらない。
//   - 索引だけで完結する（covering）か、本体行へランダムに引きに行くかで大きく変わる。
//   - 範囲がテーブルの大部分を占めると、オプティマイザが full scan に切り替える（EXP-7 と同じ）。
package datelab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

// Schema は (tenant_id, observed_date) の複合索引を持つ日付検索用テーブル。
const Schema = `CREATE TABLE IF NOT EXISTS date_search (
  tenant_id     VARCHAR(32)  NOT NULL,
  id            BIGINT       NOT NULL,
  observed_date DATE         NOT NULL,
  observed_at   DATETIME(3)  NOT NULL,
  status        TINYINT UNSIGNED NOT NULL,
  payload       VARCHAR(500) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id),
  -- ★テナント先頭の複合索引。日付範囲がテナント局所の range scan になる
  KEY k_date (tenant_id, observed_date, id)
) ENGINE=InnoDB`

func Setup(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, Schema)
	return err
}

// Seed は tenant に rows 件を、days 日にわたって均等に散らして入れる。
// noise を >0 にすると別テナントの行も入れ、テナント局所性を検証できる。
func Seed(ctx context.Context, db *sql.DB, tenant string, rows, days int, base time.Time, noiseTenants, noisePer int) error {
	//smlint:allow rowsaffected 理由: 実験前の後片付け。消える行が 0 でも正しい
	if _, err := db.ExecContext(ctx, "DELETE FROM date_search WHERE tenant_id LIKE ?", "ds%"); err != nil {
		return err
	}
	insert := func(tn string, n, dys int, idBase int) error {
		const chunk = 1000
		pay := strings.Repeat("x", 300)
		for start := 0; start < n; start += chunk {
			end := start + chunk
			if end > n {
				end = n
			}
			var b strings.Builder
			b.WriteString("INSERT INTO date_search (tenant_id, id, observed_date, observed_at, status, payload) VALUES ")
			args := make([]any, 0, (end-start)*6)
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?,?,?,?,?,?)")
				day := 0
				if dys > 0 {
					day = i * dys / n
				}
				d := base.AddDate(0, 0, day)
				args = append(args, tn, idBase+i, d.Format("2006-01-02"),
					d.Format("2006-01-02 15:04:05.000"), i%5, pay)
			}
			//smlint:allow loopquery 理由: 実験データの一括投入（1000行/文）。N+1 ではない
			//smlint:allow rowsaffected 理由: 投入。入るかはエラーで判る
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := insert(tenant, rows, days, 0); err != nil {
		return err
	}
	for t := 0; t < noiseTenants; t++ {
		if err := insert(fmt.Sprintf("ds-noise%02d", t), noisePer, days, 0); err != nil {
			return err
		}
	}
	return nil
}

// Plan は EXPLAIN の要点。
type Plan struct {
	Type  string // range / ref / ALL ...
	Rows  int64  // 走査見込み行数
	Key   string
	Extra string
}

// Measure は日付範囲クエリの所要と実行計画を返す。
// covering=true なら索引に載る列だけ（本体行を引きに行かない）、false なら payload も取る。
func Measure(ctx context.Context, db *sql.DB, tenant string, from, to time.Time, covering bool, samples int) (expkit.LatencyStats, int64, Plan, error) {
	cols := "COUNT(*)"
	if !covering {
		cols = "id, observed_at, payload" // payload は索引に無い → 本体行へ引きに行く
	}
	q := fmt.Sprintf(`SELECT %s FROM date_search
	                   WHERE tenant_id = ? AND observed_date >= ? AND observed_date < ?`, cols)
	if !covering {
		q += " ORDER BY observed_date, id LIMIT 100000"
	}
	fromS, toS := from.Format("2006-01-02"), to.Format("2006-01-02")

	plan := explain(ctx, db, q, tenant, fromS, toS)

	lat := expkit.NewLatency()
	var matched int64
	for s := 0; s < samples; s++ {
		t0 := time.Now()
		if covering {
			//smlint:allow loopquery 理由: 同じクエリを samples 回計測するのが目的
			if err := db.QueryRowContext(ctx, q, tenant, fromS, toS).Scan(&matched); err != nil {
				return expkit.LatencyStats{}, 0, plan, err
			}
		} else {
			//smlint:allow loopquery 理由: 同じクエリを samples 回計測するのが目的
			rows, err := db.QueryContext(ctx, q, tenant, fromS, toS)
			if err != nil {
				return expkit.LatencyStats{}, 0, plan, err
			}
			var n int64
			for rows.Next() {
				var id int64
				var at, pay string
				if err := rows.Scan(&id, &at, &pay); err != nil {
					_ = rows.Close()
					return expkit.LatencyStats{}, 0, plan, err
				}
				n++
			}
			_ = rows.Close()
			matched = n
		}
		lat.Record(time.Since(t0))
	}
	return lat.Stats(), matched, plan, nil
}

func explain(ctx context.Context, db *sql.DB, q string, args ...any) Plan {
	var p Plan
	rows, err := db.QueryContext(ctx, "EXPLAIN "+q, args...)
	if err != nil {
		return p
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return p
		}
		m := map[string]string{}
		for i, c := range cols {
			m[c] = asStr(vals[i])
		}
		p.Type = m["type"]
		p.Key = m["key"]
		p.Extra = m["Extra"]
		fmt.Sscan(m["rows"], &p.Rows)
	}
	return p
}

func asStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}
