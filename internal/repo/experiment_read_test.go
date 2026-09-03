package repo_test

// リポジトリ層の設計を「実験して決める」ためのコード（その2: 読み方）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/mysqltest"
)

// 一覧用のテーブルを用意する（テナント20 × 5,000行）。
func seedList(t *testing.T) {
	t.Helper()
	db := mysqltest.Raw(t)
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM exp_list").Scan(&n)
	if n == expTenants*expPerOwner {
		return
	}
	mustExec(t, db, "DROP TABLE IF EXISTS exp_list")
	mustExec(t, db, `CREATE TABLE exp_list (
		tenant_id VARCHAR(32) NOT NULL,
		id        INT          NOT NULL,
		status    TINYINT      NOT NULL,
		payload   VARCHAR(120) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	) ENGINE=InnoDB`)
	payload := fmt.Sprintf("%0100d", 1)
	const batch = 200
	var vals []string
	var args []any
	flush := func() {
		if len(vals) == 0 {
			return
		}
		mustExec(t, db, "INSERT INTO exp_list (tenant_id, id, status, payload) VALUES "+joinComma(vals), args...)
		vals, args = vals[:0], args[:0]
	}
	for i := 0; i < expPerOwner; i++ {
		for tn := 0; tn < expTenants; tn++ {
			vals = append(vals, "(?,?,?,?)")
			args = append(args, fmt.Sprintf("t-%03d", tn), i, i%3, payload)
			if len(vals) >= batch {
				flush()
			}
		}
	}
	flush()
	mustExec(t, db, "ANALYZE TABLE exp_list")
}

// 実験3: ページ送りを OFFSET でやるか、前ページの最後のキーから続けるか（キーセット法）。
func TestExperiment_ページ送り(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedList(t)
	const pageSize = 50

	measure := func(name string, fn func(offset int) int) {
		for _, depth := range []int{0, 1000, 4000} {
			start := time.Now()
			const n = 20
			got := 0
			for i := 0; i < n; i++ {
				got = fn(depth)
			}
			t.Logf("%-16s %4d 件目から %d 件: %v (取得 %d 件)",
				name, depth, pageSize, (time.Since(start) / n).Round(time.Microsecond), got)
		}
	}

	measure("OFFSET", func(offset int) int {
		rows, err := db.Query(`SELECT id, status, payload FROM exp_list
		                       WHERE tenant_id = ? ORDER BY id LIMIT ? OFFSET ?`, "t-010", pageSize, offset)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		return countRows(t, rows)
	})

	measure("キーセット", func(offset int) int {
		// 前ページの最後の id を知っている前提（実際のページ送りではそうなる）
		rows, err := db.Query(`SELECT id, status, payload FROM exp_list
		                       WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT ?`, "t-010", offset, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		return countRows(t, rows)
	})

	t.Logf("OFFSET の計画     : %s", explainLine(t, db,
		"SELECT id FROM exp_list WHERE tenant_id='t-010' ORDER BY id LIMIT 50 OFFSET 4000"))
	t.Logf("キーセットの計画  : %s", explainLine(t, db,
		"SELECT id FROM exp_list WHERE tenant_id='t-010' AND id > 4000 ORDER BY id LIMIT 50"))
	t.Log("※ OFFSET は捨てる行も読む。深くなるほど比例して遅くなる。キーセットは深さに依らない")
}

// 実験4: 1件ずつ引く（N+1）と、まとめて引く。
func TestExperiment_N1問題(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedList(t)
	const n = 500

	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}

	start := time.Now()
	for _, id := range ids {
		var status int
		var payload string
		if err := db.QueryRow("SELECT status, payload FROM exp_list WHERE tenant_id=? AND id=?",
			"t-010", id).Scan(&status, &payload); err != nil {
			t.Fatal(err)
		}
	}
	oneByOne := time.Since(start)

	ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
	args := make([]any, 0, n+1)
	args = append(args, "t-010")
	for _, id := range ids {
		args = append(args, id)
	}
	start = time.Now()
	rows, err := db.Query("SELECT id, status, payload FROM exp_list WHERE tenant_id=? AND id IN ("+ph+")", args...)
	if err != nil {
		t.Fatal(err)
	}
	got := countRows(t, rows)
	_ = rows.Close()
	inQuery := time.Since(start)

	start = time.Now()
	rows, err = db.Query("SELECT id, status, payload FROM exp_list WHERE tenant_id=? AND id BETWEEN ? AND ?",
		"t-010", 0, n-1)
	if err != nil {
		t.Fatal(err)
	}
	_ = countRows(t, rows)
	_ = rows.Close()
	rangeQuery := time.Since(start)

	t.Logf("%d 件を取る: 1件ずつ %v / IN でまとめて %v / 範囲で %v", n,
		oneByOne.Round(time.Microsecond), inQuery.Round(time.Microsecond), rangeQuery.Round(time.Microsecond))
	t.Logf("→ 1件ずつは %.0f 倍遅い（取得 %d 件）", float64(oneByOne)/float64(inQuery), got)
	t.Log("※ 往復1回あたり 0.1ms 前後（§1）。N+1 は「遅いクエリ」ではなく「速いクエリを N 回」なので、")
	t.Log("  スロークエリログには出ない。テストで問い合わせ回数を数えるのが確実")
}

// 実験5: 索引だけで足りる問い合わせ（covering index）にすると何が変わるか。
func TestExperiment_必要な列だけ取る(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedList(t)

	measure := func(name, q string) time.Duration {
		for i := 0; i < 3; i++ {
			rows, err := db.Query(q, "t-010")
			if err != nil {
				t.Fatal(err)
			}
			_ = countRows(t, rows)
			_ = rows.Close()
		}
		const n = 10
		start := time.Now()
		for i := 0; i < n; i++ {
			rows, err := db.Query(q, "t-010")
			if err != nil {
				t.Fatal(err)
			}
			_ = countRows(t, rows)
			_ = rows.Close()
		}
		d := time.Since(start) / n
		t.Logf("%-24s %v", name, d.Round(time.Microsecond))
		return d
	}

	all := measure("全列 (SELECT *)", "SELECT tenant_id, id, status, payload FROM exp_list WHERE tenant_id = ?")
	few := measure("主キーの列だけ", "SELECT id FROM exp_list WHERE tenant_id = ?")
	t.Logf("→ %.1f 倍の差。%s", float64(all)/float64(few),
		explainLine(t, db, "SELECT id FROM exp_list WHERE tenant_id = 't-010'"))
	t.Log("※ 一覧画面が payload まで持ってくる必要がないなら、取らない。転送量とバッファプールの両方に効く")
}
