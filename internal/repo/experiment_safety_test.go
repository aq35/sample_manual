package repo_test

// リポジトリ層の設計を「実験して決める」ためのコード（その3: 事故）。
//
// ここでは「素直に書くと、どう壊れるか」を実際に起こす。
// 防ぎ方は internal/repo の層（guard.go / tx.go）にまとめてあり、repo_test.go で検証している。

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/mysqltest"
)

func seedAccounts(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, "DROP TABLE IF EXISTS exp_acct")
	mustExec(t, db, `CREATE TABLE exp_acct (
		tenant_id VARCHAR(32)     NOT NULL,
		id        INT             NOT NULL,
		balance   INT             NOT NULL,
		status    TINYINT         NOT NULL,
		version   BIGINT UNSIGNED NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, id)
	) ENGINE=InnoDB`)
	for _, tenant := range []string{"t-a", "t-b"} {
		for i := 1; i <= 5; i++ {
			mustExec(t, db, "INSERT INTO exp_acct (tenant_id, id, balance, status) VALUES (?,?,?,?)",
				tenant, i, 100, 1)
		}
	}
}

// 事故1: 読んで、計算して、書く（read-modify-write）を同時にやると、更新が消える。
func TestExperiment_更新が消える(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedAccounts(t, db)
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS exp_acct") }()

	const workers = 8
	const perWorker = 25

	// ① 素直な read-modify-write
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				var bal int
				if err := db.QueryRow("SELECT balance FROM exp_acct WHERE tenant_id=? AND id=?", "t-a", 1).Scan(&bal); err != nil {
					t.Error(err)
					return
				}
				if _, err := db.Exec("UPDATE exp_acct SET balance=? WHERE tenant_id=? AND id=?", bal+1, "t-a", 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	var got int
	if err := db.QueryRow("SELECT balance FROM exp_acct WHERE tenant_id=? AND id=?", "t-a", 1).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := 100 + workers*perWorker
	t.Logf("① 読んで書く: 期待 %d / 実際 %d （%d 回ぶんの更新が消えた）", want, got, want-got)
	if got >= want {
		t.Log("  注意: この環境では競合が起きなかった（並列度やタイミング次第）。起きるときは静かに起きる")
	}

	// ② version（楽観ロック）で守る: 読んだときの version を条件に入れる
	mustExec(t, db, "UPDATE exp_acct SET balance=100, version=0 WHERE tenant_id=? AND id=?", "t-a", 1)
	var conflicts int
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				for { // 衝突したら読み直してやり直す
					var bal int
					var ver uint64
					if err := db.QueryRow("SELECT balance, version FROM exp_acct WHERE tenant_id=? AND id=?",
						"t-a", 1).Scan(&bal, &ver); err != nil {
						t.Error(err)
						return
					}
					res, err := db.Exec(`UPDATE exp_acct SET balance=?, version=version+1
					                     WHERE tenant_id=? AND id=? AND version=?`, bal+1, "t-a", 1, ver)
					if err != nil {
						t.Error(err)
						return
					}
					if n, _ := res.RowsAffected(); n == 1 {
						break
					}
					mu.Lock()
					conflicts++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if err := db.QueryRow("SELECT balance FROM exp_acct WHERE tenant_id=? AND id=?", "t-a", 1).Scan(&got); err != nil {
		t.Fatal(err)
	}
	t.Logf("② version で守る: 期待 %d / 実際 %d （衝突して やり直した回数 %d）", want, got, conflicts)
	if got != want {
		t.Fatalf("楽観ロックでも更新が消えている: %d", got)
	}

	// ③ そもそも読まずに書く（可能ならこれが最善）
	mustExec(t, db, "UPDATE exp_acct SET balance=100 WHERE tenant_id=? AND id=?", "t-a", 1)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := db.Exec("UPDATE exp_acct SET balance=balance+1 WHERE tenant_id=? AND id=?", "t-a", 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := db.QueryRow("SELECT balance FROM exp_acct WHERE tenant_id=? AND id=?", "t-a", 1).Scan(&got); err != nil {
		t.Fatal(err)
	}
	t.Logf("③ 読まずに書く (balance=balance+1): 期待 %d / 実際 %d / 所要 %v",
		want, got, time.Since(start).Round(time.Millisecond))
	if got != want {
		t.Fatalf("加算更新で値がずれた: %d", got)
	}
	t.Log("→ 「読んで書く」しかない場合だけ version を使う。DB 側で計算できるなら読まないほうが速くて安全")
}

// 事故2: WHERE の書き間違いで、意図した以上の行が更新される。
func TestExperiment_WHEREの書き間違い(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedAccounts(t, db)
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS exp_acct") }()

	// 「id=3 の口座を停止する」つもりで status を条件にしてしまった
	res, err := db.Exec("UPDATE exp_acct SET status=9 WHERE tenant_id=? AND status=1", "t-a")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	t.Logf("1行のつもりの UPDATE が %d 行に当たった（エラーは出ない）", n)
	if n <= 1 {
		t.Fatalf("この実験の前提が崩れている: %d", n)
	}
	t.Log("→ 影響行数を検査していなければ、そのままコミットされる。")
	t.Log("  リポジトリ層では『1行のはず』を宣言させ、違ったらロールバックする（repo.ExpectOne）")
}

// 事故3: tenant_id を忘れると、他テナントの行まで書き換わる。
func TestExperiment_テナント指定を忘れる(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	seedAccounts(t, db)
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS exp_acct") }()

	// 「自分のテナントの id=1」のつもりで tenant_id を書き忘れた
	res, err := db.Exec("UPDATE exp_acct SET status=9 WHERE id=?", 1)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()

	var other int
	if err := db.QueryRow("SELECT status FROM exp_acct WHERE tenant_id=? AND id=?", "t-b", 1).Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Logf("tenant_id を書き忘れた UPDATE: %d 行に当たり、他テナント(t-b)の status が %d になった", n, other)
	if other != 9 {
		t.Fatal("この実験の前提が崩れている")
	}
	t.Log("→ SQL 文を書く人が毎回 tenant_id を書く設計だと、いつか誰かが忘れる。")
	t.Log("  リポジトリ層では『テナントに束縛したハンドル』からしか SQL を出せないようにする")
}

// 事故4: MySQL の安全装置 sql_safe_updates は、何を止めて何を止めないか。
func TestExperiment_safe_updatesは何を止めるか(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("mysql", dsn+sep+"sql_safe_updates=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var v int
	if err := db.QueryRow("SELECT @@SESSION.sql_safe_updates").Scan(&v); err != nil {
		t.Skipf("接続できないため skip: %v", err)
	}
	t.Logf("DSN に sql_safe_updates=1 と書くだけで有効にできる（go-sql-driver は未知のパラメータをシステム変数として送る）: %d", v)

	seedAccounts(t, db)
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS exp_acct") }()

	cases := []struct{ name, q string }{
		{"WHERE なしの UPDATE", "UPDATE exp_acct SET status=2"},
		{"キーを使わない WHERE", "UPDATE exp_acct SET status=2 WHERE status=1"},
		{"キーを使わない WHERE + LIMIT", "UPDATE exp_acct SET status=2 WHERE status=1 LIMIT 100"},
		{"主キーの先頭列だけ (tenant_id)", "UPDATE exp_acct SET status=2 WHERE tenant_id='t-a'"},
		{"主キー完全一致", "UPDATE exp_acct SET status=2 WHERE tenant_id='t-a' AND id=1"},
		{"tenant_id 忘れ (id=1)", "UPDATE exp_acct SET status=2 WHERE id=1"},
		{"WHERE なしの DELETE", "DELETE FROM exp_acct"},
	}
	for _, c := range cases {
		res, err := db.Exec(c.q)
		if err != nil {
			t.Logf("  拒否  %-30s %s", c.name, shortErr(err))
			continue
		}
		n, _ := res.RowsAffected()
		t.Logf("  通過  %-30s 影響 %d 行", c.name, n)
	}
	t.Log("→ 止まるのは「キー列を使わない UPDATE/DELETE」。ただし LIMIT を付けると通る。")
	t.Log("  『tenant_id 忘れ』は主キーの先頭列を欠くので拒否されるが、これは表の主キー次第で変わる。")
	t.Log("  安全装置としては有効だが、テナント分離をこれに頼ってはいけない（アプリ側の保証が要る）")
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "; try"); i > 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "..."
	}
	return s
}

func TestMain(m *testing.M) {
	code := m.Run()
	// 実験用テーブルの後片付け
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		if db, err := sql.Open("mysql", dsn); err == nil {
			for _, tbl := range []string{"exp_acct", "exp_list", "exp_pk_composite", "exp_pk_secondary", "exp_seq", "exp_uuid"} {
				_, _ = db.Exec("DROP TABLE IF EXISTS " + tbl)
			}
			_ = db.Close()
		}
	}
	fmt.Fprintln(os.Stderr)
	os.Exit(code)
}
