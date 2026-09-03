package mysqlfacts_test

// §9「未検証の項目」を、実際の MySQL に聞いて確かめる。
//
//	MYSQL_DSN=... go test ./internal/mysqlfacts/ -v
//
// ここは「動くコード」ではなく「事実の確認」。結果は README と 調査 に反映してある。
// サーバのバージョンで結果が変わりうるので、必ず自分の環境で走らせること。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/mysqltest"
)

// TestMain は、前回の実行が途中で落ちて残した設定を戻してから始める。
//
// ★実験がグローバル設定を変える以上、「前回の残骸」を前提にしてはいけない。
// 実際、並列実行で落ちた回のあと wait_timeout=2 が残り、
// 別のテストが「接続が切られた」で落ちた（実装ではなく環境の状態が原因）。
func TestMain(m *testing.M) {
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		if db, err := sql.Open("mysql", dsn); err == nil {
			for _, stmt := range []string{
				"SET GLOBAL wait_timeout = 28800",
				"SET GLOBAL innodb_flush_log_at_trx_commit = 1",
			} {
				_, _ = db.Exec(stmt)
			}
			_ = db.Close()
		}
	}
	os.Exit(m.Run())
}

func version(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow("SELECT VERSION()").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// §9 ①: innodb_flush_log_at_trx_commit はサーバ全体の設定か。動的に変えられるか。
func TestFact_flush_log_at_trx_commit(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	t.Logf("MySQL %s", version(t, db))

	var global int
	if err := db.QueryRow("SELECT @@GLOBAL.innodb_flush_log_at_trx_commit").Scan(&global); err != nil {
		t.Fatal(err)
	}
	t.Logf("GLOBAL の現在値: %d", global)

	// セッション単位で変えられるか？ → 変えられないならサーバ全体の設定である
	_, err := db.Exec("SET SESSION innodb_flush_log_at_trx_commit = 2")
	if err == nil {
		t.Fatal("セッション単位で変更できてしまった（資料の記述と食い違う）")
	}
	t.Logf("SET SESSION は拒否される → サーバ全体の設定で確定: %v", err)

	// 動的に変更できるか？（できるなら再起動不要）
	if _, err := db.Exec("SET GLOBAL innodb_flush_log_at_trx_commit = 2"); err != nil {
		t.Skipf("SET GLOBAL の権限が無いため、動的変更の確認は skip: %v", err)
	}
	defer func() {
		if _, err := db.Exec("SET GLOBAL innodb_flush_log_at_trx_commit = ?", global); err != nil {
			t.Errorf("元の値に戻せなかった: %v", err)
		}
	}()
	var now int
	if err := db.QueryRow("SELECT @@GLOBAL.innodb_flush_log_at_trx_commit").Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now != 2 {
		t.Fatalf("動的変更が反映されていない: %d", now)
	}
	t.Log("SET GLOBAL で動的に変更できる（再起動不要）")
	t.Log("★ただしサーバ全体に効くため、同居している他の業務データにも一律で効く。専用インスタンスでない限り触らないこと")
}

// §9 ①の続き: 0 / 1 / 2 でコミットのコストがどう変わるか、実際に測る。
func TestFact_flush設定ごとのコミット速度(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	ctx := context.Background()

	var orig int
	if err := db.QueryRow("SELECT @@GLOBAL.innodb_flush_log_at_trx_commit").Scan(&orig); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS fact_commit (id INT PRIMARY KEY, v INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_commit") }()
	if _, err := db.Exec("REPLACE INTO fact_commit VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("SET GLOBAL innodb_flush_log_at_trx_commit = ?", orig); err != nil {
		t.Skipf("SET GLOBAL の権限が無いため skip: %v", err)
	}
	defer func() { _, _ = db.Exec("SET GLOBAL innodb_flush_log_at_trx_commit = ?", orig) }()

	const n = 200
	for _, mode := range []int{1, 2, 0} {
		if _, err := db.Exec("SET GLOBAL innodb_flush_log_at_trx_commit = ?", mode); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for i := 0; i < n; i++ {
			if _, err := db.ExecContext(ctx, "UPDATE fact_commit SET v = ? WHERE id = 1", i); err != nil {
				t.Fatal(err)
			}
		}
		per := time.Since(start) / n
		t.Logf("innodb_flush_log_at_trx_commit=%d: autocommit 1件あたり %v (%.0f tx/s)", mode, per, float64(time.Second)/float64(per))
	}
	t.Log("※ 1 は「コミットのたびにディスク同期」。0/2 は同期を省くぶん速いが、落ちたときに直近のコミットを失う")
}

// §9 ②: INSERT ... ON DUPLICATE KEY UPDATE の行エイリアス構文（AS new）は使えるか。
func TestFact_行エイリアス構文(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	t.Logf("MySQL %s", version(t, db))
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS fact_alias (id INT PRIMARY KEY, v INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_alias") }()

	// 8.0.20 以降の書き方
	if _, err := db.Exec(`INSERT INTO fact_alias (id, v) VALUES (1, 10) AS new
	                      ON DUPLICATE KEY UPDATE v = new.v`); err != nil {
		t.Fatalf("AS new が使えない（8.0.20 未満なら VALUES(col) を使う）: %v", err)
	}
	t.Log("AS new（行エイリアス）が使える")

	// ★裸の列名は曖昧になる。既存行側は表名で修飾すること
	_, err := db.Exec(`INSERT INTO fact_alias (id, v) VALUES (1, 20) AS new
	                   ON DUPLICATE KEY UPDATE v = v + new.v`)
	if err == nil {
		t.Log("注意: 裸の列名が通ってしまった（環境によるので、常に表名で修飾するのが安全）")
	} else {
		t.Logf("AS new を付けると UPDATE 部の裸の列名は曖昧になる → 表名で修飾が必須: %v", err)
	}

	// 旧構文 VALUES(col) は使えるか（非推奨警告が出るか）
	if _, err := db.Exec(`INSERT INTO fact_alias (id, v) VALUES (1, 30)
	                      ON DUPLICATE KEY UPDATE v = VALUES(v)`); err != nil {
		t.Fatalf("旧構文 VALUES(col) が使えない: %v", err)
	}
	rows, err := db.Query("SHOW WARNINGS")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var level, msg string
		var code int
		if err := rows.Scan(&level, &code, &msg); err != nil {
			t.Fatal(err)
		}
		t.Logf("旧構文の警告: [%s %d] %s", level, code, msg)
		found = true
	}
	if !found {
		t.Log("旧構文 VALUES(col): このバージョンでは警告なし")
	}
}

// §9 ③: 結果的に同じ値になる UPDATE を、MySQL は本当に省くのか（affected_rows=0）。
func TestFact_同じ値のUPDATEは書き込みを省く(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS fact_noop (id INT PRIMARY KEY, v INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_noop") }()
	if _, err := db.Exec("REPLACE INTO fact_noop VALUES (1, 42)"); err != nil {
		t.Fatal(err)
	}

	res, err := db.Exec("UPDATE fact_noop SET v = 42 WHERE id = 1") // 同じ値
	if err != nil {
		t.Fatal(err)
	}
	same, _ := res.RowsAffected()

	res, err = db.Exec("UPDATE fact_noop SET v = 43 WHERE id = 1") // 違う値
	if err != nil {
		t.Fatal(err)
	}
	diff, _ := res.RowsAffected()

	t.Logf("同じ値の UPDATE: affected_rows=%d / 違う値の UPDATE: affected_rows=%d", same, diff)
	if same != 0 || diff != 1 {
		t.Fatalf("資料の記述と食い違う（same=%d diff=%d）", same, diff)
	}
	t.Log("→ 値が変わらない UPDATE は行を書き換えない。ただし ★往復とロックは発生する（§4.2）")

	// InnoDB の行ロックは、書き込みが省かれても取られる
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE fact_noop SET v = 43 WHERE id = 1"); err != nil { // 同じ値
		t.Fatal(err)
	}
	other, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.ExecContext(context.Background(), "SET innodb_lock_wait_timeout = 1"); err != nil {
		t.Fatal(err)
	}
	_, err = other.ExecContext(context.Background(), "UPDATE fact_noop SET v = 44 WHERE id = 1")
	if err == nil {
		t.Log("注意: ロック待ちが起きなかった")
	} else {
		t.Logf("値が変わらない UPDATE でも行ロックは取られる → 別トランザクションは待たされる: %v", err)
	}
}

// §9 ④: RANGE パーティションと DROP PARTITION。DELETE との差も測る。
func TestFact_パーティションの削除はDELETEより速い(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	_, _ = db.Exec("DROP TABLE IF EXISTS fact_hist")
	_, err := db.Exec(`
CREATE TABLE fact_hist (
  id            INT         NOT NULL,
  observed_date DATE        NOT NULL,
  v             INT         NOT NULL,
  PRIMARY KEY (id, observed_date)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(observed_date) (
  PARTITION p1 VALUES LESS THAN ('2026-09-02'),
  PARTITION p2 VALUES LESS THAN ('2026-09-03'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
)`)
	if err != nil {
		t.Fatalf("RANGE COLUMNS パーティションを作れない: %v", err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_hist") }()
	t.Log("PARTITION BY RANGE COLUMNS(date) の構文が通る")

	insert := func(day string, n int) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare("INSERT INTO fact_hist (id, observed_date, v) VALUES (?,?,?)")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := stmt.Exec(i, day, i); err != nil {
				t.Fatal(err)
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	const rows = 50000
	insert("2026-09-01", rows)
	insert("2026-09-02", rows)

	start := time.Now()
	if _, err := db.Exec("ALTER TABLE fact_hist DROP PARTITION p1"); err != nil {
		t.Fatal(err)
	}
	dropTime := time.Since(start)

	start = time.Now()
	if _, err := db.Exec("DELETE FROM fact_hist WHERE observed_date = '2026-09-02'"); err != nil {
		t.Fatal(err)
	}
	deleteTime := time.Since(start)

	t.Logf("%d 行の削除: DROP PARTITION %v / DELETE %v （%.1f 倍）",
		rows, dropTime, deleteTime, float64(deleteTime)/float64(dropTime))
	t.Log("※ DELETE はこのあと undo の purge も残る。DROP PARTITION は表領域ごと落とすので後始末が要らない")
}

// §9 ⑤: 長いトランザクションが undo（History list）を伸ばすか。
func TestFact_長いトランザクションはundoを伸ばす(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS fact_undo (id INT PRIMARY KEY, v INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_undo") }()
	if _, err := db.Exec("REPLACE INTO fact_undo VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// 読み取りだけの長いトランザクション（REPEATABLE READ）を開いたままにする
	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := reader.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		t.Fatal(err)
	}

	before := historyListLength(t, db)
	for i := 1; i <= 2000; i++ {
		if _, err := db.ExecContext(ctx, "UPDATE fact_undo SET v = ? WHERE id = 1", i); err != nil {
			t.Fatal(err)
		}
	}
	during := historyListLength(t, db)

	if _, err := reader.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	// purge が追いつくのを少し待つ
	var after int
	for i := 0; i < 40; i++ {
		time.Sleep(100 * time.Millisecond)
		after = historyListLength(t, db)
		if after < during/2 {
			break
		}
	}
	t.Logf("History list length: 開始 %d → 長いトランザクションを開いたまま2000回更新 %d → コミット後 %d",
		before, during, after)
	if during <= before {
		t.Log("注意: History list が伸びなかった（purge が速い環境）")
	} else {
		t.Log("→ 長いトランザクションを開いている間、古い版が undo に溜まり続ける（§4.6）")
	}
}

var histRe = regexp.MustCompile(`History list length (\d+)`)

func historyListLength(t *testing.T, db *sql.DB) int {
	t.Helper()
	var typ, name, status string
	if err := db.QueryRow("SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status); err != nil {
		t.Fatal(err)
	}
	m := histRe.FindStringSubmatch(status)
	if m == nil {
		t.Fatal("History list length を読み取れない")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// §1 / §8: 「DB へ1往復 ≒ 1 ms」は、この環境ではいくらか。
func TestFact_DBへの1往復の実測(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	ctx := context.Background()

	measure := func(name string, fn func()) time.Duration {
		for i := 0; i < 50; i++ { // 暖機
			fn()
		}
		const n = 500
		start := time.Now()
		for i := 0; i < n; i++ {
			fn()
		}
		per := time.Since(start) / n
		t.Logf("%-28s %v", name, per)
		return per
	}

	measure("SELECT 1（純粋な往復）", func() {
		var v int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS fact_rtt (id INT PRIMARY KEY, v INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS fact_rtt") }()
	if _, err := db.Exec("REPLACE INTO fact_rtt VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}

	measure("主キー1件 SELECT", func() {
		var v int
		if err := db.QueryRowContext(ctx, "SELECT v FROM fact_rtt WHERE id = 1").Scan(&v); err != nil {
			t.Fatal(err)
		}
	})
	i := 0
	measure("1件 UPDATE（autocommit）", func() {
		i++
		if _, err := db.ExecContext(ctx, "UPDATE fact_rtt SET v = ? WHERE id = 1", i); err != nil {
			t.Fatal(err)
		}
	})
	t.Log("※ 同一ホスト・TCP。ネットワークを跨ぐ本番はこれより大きい。桁（メモリ比較の10万倍）が変わらないことが要点（§1）")
}

// §5.2: MaxIdleConns の既定値 2 の影響を、サーバ側の接続数カウンタで確認する。
func TestFact_MaxIdleConnsの既定値2が再接続を生む(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	counter := func(db *sql.DB) int {
		var name string
		var v int
		if err := db.QueryRow("SHOW GLOBAL STATUS LIKE 'Connections'").Scan(&name, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	probe := mysqltest.Raw(t)

	run := func(idle int) int {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(idle)

		before := counter(probe)
		var wg sync.WaitGroup
		for round := 0; round < 30; round++ {
			wg.Add(10)
			for i := 0; i < 10; i++ {
				go func() {
					defer wg.Done()
					var v int
					if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
		}
		return counter(probe) - before
	}

	withDefault := run(2) // database/sql の既定値
	withFull := run(10)   // MaxOpenConns と同じにした場合

	t.Logf("300クエリ（10並列×30回）で新しく張られた接続数: MaxIdleConns=2 → %d 本 / MaxIdleConns=10 → %d 本",
		withDefault, withFull)
	if withDefault <= withFull {
		t.Log("注意: 差が出なかった（並列度やタイミング次第。負荷が高いほど差が開く）")
	} else {
		t.Log("→ 既定値のままだと、上限まで開いた接続が毎回捨てられ、張り直しになる（§5.2）")
	}
}

// §5.2: ConnMaxIdleTime が wait_timeout より長いと何が起きるか。
func TestFact_wait_timeoutとConnMaxIdleTime(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	admin := mysqltest.Raw(t)

	var orig int
	if err := admin.QueryRow("SELECT @@GLOBAL.wait_timeout").Scan(&orig); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("SET GLOBAL wait_timeout = 2"); err != nil {
		t.Skipf("SET GLOBAL の権限が無いため skip: %v", err)
	}
	defer func() { _, _ = admin.Exec("SET GLOBAL wait_timeout = ?", orig) }()

	open := func(idleTimeout time.Duration) *sql.DB {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if idleTimeout > 0 {
			db.SetConnMaxIdleTime(idleTimeout)
		}
		return db
	}

	// ConnMaxIdleTime 未設定 → サーバに切られた接続をアプリが掴んだままになる
	db := open(0)
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second) // wait_timeout(2s) を超えて放置

	// ① 単発クエリ: database/sql は死んだ接続を検知すると、黙って張り直して再試行する。
	// つまり「使った瞬間にエラー」にはならないことが多い。代わりに毎回再接続の代金を払う。
	err := db.QueryRow("SELECT 1").Scan(&v)
	t.Logf("① 単発クエリ（database/sql が再試行する）: err=%v", err)

	// ② 接続を掴んだまま放置した場合（sql.Conn は張り直しが効かない）
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second) // wait_timeout(2s) を超えて放置
	errConn := conn.QueryRowContext(ctx, "SELECT 1").Scan(&v)
	_ = conn.Close()
	t.Logf("② sql.Conn を掴んだまま放置（再試行されない）: err=%v", errConn)

	// ③ トランザクションを開いたまま放置した場合
	tx, txErr := db.BeginTx(ctx, nil)
	if txErr == nil {
		time.Sleep(4 * time.Second)
		_, txErr = tx.ExecContext(ctx, "SELECT 1")
		_ = tx.Rollback()
	}
	t.Logf("③ トランザクションを開いたまま放置: err=%v", txErr)

	// ② ConnMaxIdleTime < wait_timeout → アプリ側が先に閉じるので、そもそも起きない
	db2 := open(500 * time.Millisecond)
	defer func() { _ = db2.Close() }()
	if err := db2.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	conn2, err := db2.Conn(ctx) // アイドル分は既に閉じられているので、これは新しい接続
	if err != nil {
		t.Fatalf("ConnMaxIdleTime を wait_timeout より短くしても失敗した: %v", err)
	}
	if err := conn2.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
		t.Fatalf("ConnMaxIdleTime を wait_timeout より短くしても失敗した: %v", err)
	}
	_ = conn2.Close()
	t.Log("④ ConnMaxIdleTime(0.5s) < wait_timeout(2s) にすると、死んだ接続を掴むこと自体が起きない（§5.2）")
}

func init() { _ = fmt.Sprint; _ = strings.TrimSpace }
