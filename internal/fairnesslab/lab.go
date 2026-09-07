// Package fairnesslab は EXP-34（ノイジーネイバー＝テナント公平性）の実験本体。
//
// 1テナント(hog)がキューに大量投入すると、素朴な「到着順に捌く」では他テナント(victim)の
// 命令が hog の後ろで延々待たされる。テナントをまたいで公平に回す（round-robin）と、
// victim は hog の量に関係なくすぐ捌ける。キューは DB に置き、捌く順序（＝dispatch 位置）で比べる。
package fairnesslab

import (
	"context"
	"database/sql"
	"strings"
)

// Item はキューの1件（どのテナントの、到着順 seq 何番か）。
type Item struct {
	Tenant string
	Seq    int64
}

func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS nq",
		`CREATE TABLE nq (
		   tenant_id VARCHAR(32) NOT NULL,
		   seq       BIGINT      NOT NULL,
		   PRIMARY KEY (seq),
		   KEY by_tenant (tenant_id, seq)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Seed は hog テナントに hogN 件（到着が先＝小さい seq）、victims 個のテナントに各1件
// （到着が後＝大きい seq）を入れる。現実の「1テナントが先に大量投入」を模す。
func Seed(ctx context.Context, db *sql.DB, hogN, victims int) error {
	seq := int64(0)
	ins := func(tenant string, n int) error {
		const chunk = 500
		for start := 0; start < n; start += chunk {
			end := start + chunk
			if end > n {
				end = n
			}
			var b strings.Builder
			b.WriteString("INSERT INTO nq (tenant_id, seq) VALUES ")
			var args []any
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?,?)")
				args = append(args, tenant, seq)
				seq++
			}
			//smlint:allow loopquery 理由: シード。500行/文の一括投入
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ins("hog", hogN); err != nil {
		return err
	}
	for v := 0; v < victims; v++ {
		if err := ins(victimName(v), 1); err != nil {
			return err
		}
	}
	return nil
}

func victimName(v int) string { return "victim-" + string(rune('A'+v)) }

// VictimNames は victims 個の victim テナント名。
func VictimNames(victims int) []string {
	out := make([]string, victims)
	for v := 0; v < victims; v++ {
		out[v] = victimName(v)
	}
	return out
}

// LoadOrdered は DB から全件を到着順(seq)で読む。
func LoadOrdered(ctx context.Context, db *sql.DB) ([]Item, error) {
	rows, err := db.QueryContext(ctx, "SELECT tenant_id, seq FROM nq ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.Tenant, &it.Seq); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Unfair は「到着順に捌く」。hog が先に大量投入していると victim は後ろで待つ。
func Unfair(items []Item) []Item { return items }

// Fair は「テナントをまたいで round-robin（1周に各テナント1件）」。victim は hog の量に関係なく
// 早い周で捌ける。テナントの順は初出（seq）順。
func Fair(items []Item) []Item {
	var order []string
	q := map[string][]Item{}
	for _, it := range items {
		if _, ok := q[it.Tenant]; !ok {
			order = append(order, it.Tenant)
		}
		q[it.Tenant] = append(q[it.Tenant], it)
	}
	out := make([]Item, 0, len(items))
	for {
		progressed := false
		for _, tn := range order {
			if len(q[tn]) > 0 {
				out = append(out, q[tn][0])
				q[tn] = q[tn][1:]
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// WorstVictimPosition は、処理順序 order の中で victim 各テナントの命令が捌かれる位置
// （前に何件処理されたか）の最大値を返す。小さいほど公平（待たされない）。
func WorstVictimPosition(order []Item, victims []string) int {
	isVictim := map[string]bool{}
	for _, v := range victims {
		isVictim[v] = true
	}
	worst := 0
	for pos, it := range order {
		if isVictim[it.Tenant] && pos > worst {
			worst = pos
		}
	}
	return worst
}
