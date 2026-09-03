package repo_test

// 排他制御の実験。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run TestExperimentLock_ -v
//
// 「GET_LOCK を使うべきか、代わりに何があるか」を、実際に動かして決めるためのコード。
// 結果は docs/locking.md にまとめてある。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/mysqltest"
)

// ---- 補助 ----

// getLock は指定の接続でロックを取る。戻り値: 1=取れた, 0=時間切れ, -1=NULL（エラー）
func getLock(t *testing.T, c *sql.Conn, name string, timeoutSec int) int {
	t.Helper()
	var got sql.NullInt64
	if err := c.QueryRowContext(context.Background(), "SELECT GET_LOCK(?, ?)", name, timeoutSec).Scan(&got); err != nil {
		t.Fatalf("GET_LOCK: %v", err)
	}
	if !got.Valid {
		return -1
	}
	return int(got.Int64)
}

func releaseLock(t *testing.T, c *sql.Conn, name string) int {
	t.Helper()
	var got sql.NullInt64
	if err := c.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", name).Scan(&got); err != nil {
		t.Fatalf("RELEASE_LOCK: %v", err)
	}
	if !got.Valid {
		return -1 // NULL = そのロックは存在しない
	}
	return int(got.Int64)
}

func connID(t *testing.T, c *sql.Conn) int64 {
	t.Helper()
	var id int64
	if err := c.QueryRowContext(context.Background(), "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func lockHolder(t *testing.T, db *sql.DB, name string) (int64, bool) {
	t.Helper()
	var id sql.NullInt64
	if err := db.QueryRow("SELECT IS_USED_LOCK(?)", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id.Int64, id.Valid
}

// 実験1: GET_LOCK は「接続（セッション）」に紐づく。
// database/sql のプールごしに使うと、取ったのと違う接続で解放しようとして失敗する。
func TestExperimentLock_接続に紐づく(t *testing.T) {
	db := mysqltest.Raw(t)
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	const name = "exp_lock_pool"

	// ① プールから適当に取った接続でロックし、別の呼び出しで解放しようとする
	//    （*sql.DB のメソッドは、どの接続に行くか保証が無い）
	var got int
	if err := db.QueryRowContext(ctx, "SELECT GET_LOCK(?, 1)", name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatal("ロックが取れない")
	}
	// 取った接続をプールに戻さないように、別の接続を先に埋める
	busy := make([]*sql.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		busy = append(busy, c)
	}
	var released sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&released)
	for _, c := range busy {
		_ = c.Close()
	}
	switch {
	case err != nil:
		t.Logf("① プールごしの RELEASE_LOCK: エラー %v", err)
	case !released.Valid:
		t.Logf("① プールごしの RELEASE_LOCK: NULL（そんなロックは無い）")
	case released.Int64 == 0:
		t.Logf("① プールごしの RELEASE_LOCK: 0 ＝ **解放できていない**（別の接続が持っている）")
	default:
		t.Logf("① プールごしの RELEASE_LOCK: 1（たまたま同じ接続に当たった）")
	}
	if holder, used := lockHolder(t, db, name); used {
		t.Logf("   → ロックはまだ接続 %d が持っている。プールに残った接続が持ち続ける", holder)
	} else {
		t.Logf("   → 解放された（この試行では同じ接続に当たった）")
	}
	// 後片付け: 持っている接続を特定して殺す
	if holder, used := lockHolder(t, db, name); used {
		if _, err := db.Exec(fmt.Sprintf("KILL %d", holder)); err != nil {
			t.Logf("   後片付けの KILL に失敗: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// ② 接続を固定すれば、取る・解放するが同じ接続で行われる
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if got := getLock(t, c, name, 1); got != 1 {
		t.Fatalf("固定した接続でロックが取れない: %d", got)
	}
	if r := releaseLock(t, c, name); r != 1 {
		t.Fatalf("固定した接続で解放できない: %d", r)
	}
	t.Log("② db.Conn() で接続を固定すれば、取得と解放が同じセッションになる → 正しく動く")
	t.Log("→ 結論: GET_LOCK を *sql.DB のメソッドで直接呼んではいけない。必ず db.Conn() で固定する")
}

// 実験2: ロックを持ったままの接続がプールに戻ると、次にその接続を使う処理が影響を受ける。
func TestExperimentLock_プールに残ったロックは他の処理に見える(t *testing.T) {
	db := mysqltest.Raw(t)
	db.SetMaxOpenConns(1) // 全員が同じ接続を使う状況を作る
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	const name = "exp_lock_leak"

	var got int
	if err := db.QueryRowContext(ctx, "SELECT GET_LOCK(?, 1)", name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatal("ロックが取れない")
	}
	// この時点で接続はプールに戻っている（ロックを持ったまま）

	// 「別の処理」が同じ接続を借りると、ロックを持っている状態で仕事を始める
	var free int
	if err := db.QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", name).Scan(&free); err != nil {
		t.Fatal(err)
	}
	// そして、取っていないはずの処理が解放できてしまう
	var released int
	if err := db.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil {
		t.Fatal(err)
	}
	t.Logf("ロックを持ったままプールに戻った接続を、別の処理が借りた: IS_FREE_LOCK=%d, その処理が RELEASE_LOCK=%d", free, released)
	t.Log("→ 「取った人だけが解放する」という前提が、接続プールでは成り立たない")
	t.Log("  接続を固定していない GET_LOCK は、ロックを持ったまま忘れる／他人に解放される、のどちらかになる")
}

// 実験3: GET_LOCK はトランザクションと無関係。COMMIT でも ROLLBACK でも解放されない。
func TestExperimentLock_トランザクションと無関係(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const name = "exp_lock_tx"

	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if got := getLock(t, c, name, 1); got != 1 {
		t.Fatal("ロックが取れない")
	}
	if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, used := lockHolder(t, db, name); used {
		t.Log("ROLLBACK してもロックは解放されない（トランザクションの外にある）")
	} else {
		t.Fatal("ROLLBACK で解放された（資料の理解と違う）")
	}
	_ = releaseLock(t, c, name)
	t.Log("→ 良い面: DDL の暗黙コミットに巻き込まれない。悪い面: 明示的に解放しないと残る")
}

// 実験4（重要）: 代替案「ロック用の行を SELECT ... FOR UPDATE」は、DDL では使えない。
// MySQL の DDL は暗黙にコミットするので、その瞬間に行ロックが外れる。
func TestExperimentLock_DDLは行ロックを外す(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	mustExec(t, db, "DROP TABLE IF EXISTS exp_lock_row")
	mustExec(t, db, "CREATE TABLE exp_lock_row (name VARCHAR(32) PRIMARY KEY) ENGINE=InnoDB")
	mustExec(t, db, "INSERT INTO exp_lock_row VALUES ('migrate')")
	defer func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_lock_row")
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_lock_ddl")
	}()

	// A: 行ロックを取ってから DDL を実行する
	a, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if _, err := a.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n string
	if err := a.QueryRowContext(ctx, "SELECT name FROM exp_lock_row WHERE name='migrate' FOR UPDATE").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// マイグレーションらしく DDL を1つ流す
	if _, err := a.ExecContext(ctx, "CREATE TABLE exp_lock_ddl (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	// B: 同じ行のロックを取ろうとする。A がまだ「マイグレーション中」なら待たされるはず
	b, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.ExecContext(ctx, "SET innodb_lock_wait_timeout = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	err = b.QueryRowContext(ctx, "SELECT name FROM exp_lock_row WHERE name='migrate' FOR UPDATE").Scan(&n)
	_, _ = b.ExecContext(ctx, "ROLLBACK")
	_, _ = a.ExecContext(ctx, "ROLLBACK")

	if err == nil {
		t.Log("★ A が DDL を1つ実行した時点で、A の行ロックは外れていた（DDL の暗黙コミット）")
		t.Log("  → B が同時にマイグレーションを始められる。行ロック方式は DDL には使えない")
	} else {
		t.Fatalf("B が待たされた（想定と違う）: %v", err)
	}

	// 比較: GET_LOCK は DDL をまたいでも保持される
	const name = "exp_lock_ddl_userlock"
	if got := getLock(t, a, name, 1); got != 1 {
		t.Fatal("ロックが取れない")
	}
	if _, err := a.ExecContext(ctx, "DROP TABLE exp_lock_ddl"); err != nil {
		t.Fatal(err)
	}
	if got := getLock(t, b, name, 1); got != 0 {
		t.Fatalf("DDL のあと B がロックを取れてしまった: %d", got)
	}
	t.Log("★ GET_LOCK は DDL をまたいでも保持される → マイグレーションの排他にはこちらが要る")
	_ = releaseLock(t, a, name)
}

// 実験5: ロック名は「サーバ全体」で共通。データベースが違っても衝突する。
func TestExperimentLock_名前はサーバ全体で共通(t *testing.T) {
	dsn := mysqltest.DSN(t)
	other := strings.Replace(dsn, "/workerdb?", "/workerdb2?", 1)
	if other == dsn {
		t.Skip("DSN の形が想定と違うため skip")
	}
	db2, err := sql.Open("mysql", other)
	if err != nil {
		t.Skipf("2つめのデータベースに接続できないため skip: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if err := db2.Ping(); err != nil {
		t.Skipf("2つめのデータベース（workerdb2）が無いため skip: %v", err)
	}

	ctx := context.Background()
	db1 := mysqltest.Raw(t)
	c1, err := db1.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c1.Close() }()
	c2, err := db2.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()

	const name = "migrate"
	if got := getLock(t, c1, name, 1); got != 1 {
		t.Fatal("ロックが取れない")
	}
	got := getLock(t, c2, name, 1)
	_ = releaseLock(t, c1, name)

	if got == 0 {
		t.Log("別のデータベース（workerdb2）からでも、同じ名前のロックは取れない")
		t.Log("→ ロック名はサーバ全体の名前空間。同居しているアプリと衝突する")
		t.Log("  対策: 名前にスキーマ名やアプリ名を含める（例: workerdb.migrate）")
	} else {
		t.Fatalf("別データベースからロックが取れた（想定と違う）: %d", got)
	}
}

// 実験6: 待ち方（タイムアウトの指定）と、状態の確認方法。
func TestExperimentLock_待ち方と状態の確認(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	a, _ := db.Conn(ctx)
	b, _ := db.Conn(ctx)
	defer func() { _ = a.Close(); _ = b.Close() }()
	const name = "exp_lock_wait"

	if got := getLock(t, a, name, 1); got != 1 {
		t.Fatal("ロックが取れない")
	}

	start := time.Now()
	got := getLock(t, b, name, 0) // 0 = 待たない
	t.Logf("タイムアウト 0（待たない）: 結果 %d / 所要 %v", got, time.Since(start).Round(time.Millisecond))

	start = time.Now()
	got = getLock(t, b, name, 1) // 1秒待つ
	t.Logf("タイムアウト 1秒       : 結果 %d / 所要 %v", got, time.Since(start).Round(time.Millisecond))

	var free int
	if err := db.QueryRow("SELECT IS_FREE_LOCK(?)", name).Scan(&free); err != nil {
		t.Fatal(err)
	}
	holder, used := lockHolder(t, db, name)
	t.Logf("IS_FREE_LOCK=%d / IS_USED_LOCK=%d (使用中=%v, これは接続 ID)", free, holder, used)
	t.Log("→ 誰が持っているかは IS_USED_LOCK で分かる。performance_schema.metadata_locks でも見える")

	// 同じセッションが同じ名前を2回取ると、参照カウントが増える（2回解放が要る）
	if got := getLock(t, a, name, 1); got != 1 {
		t.Fatal("同じセッションからの再取得に失敗")
	}
	r1 := releaseLock(t, a, name)
	_, stillUsed := lockHolder(t, db, name)
	r2 := releaseLock(t, a, name)
	_, afterUsed := lockHolder(t, db, name)
	t.Logf("同じ名前を2回取った場合: 1回目の解放=%d（まだ使用中=%v）→ 2回目の解放=%d（使用中=%v）",
		r1, stillUsed, r2, afterUsed)
	t.Log("→ 取得と解放の回数を合わせる必要がある。defer で1回ずつ対応させるのが安全")
}

// 実験7: 接続が切れたらロックはどうなるか。
func TestExperimentLock_接続が切れたら解放される(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	victim, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const name = "exp_lock_kill"
	if got := getLock(t, victim, name, 1); got != 1 {
		t.Fatal("ロックが取れない")
	}
	id := connID(t, victim)

	if _, err := db.Exec(fmt.Sprintf("KILL %d", id)); err != nil {
		t.Fatalf("KILL できない（権限不足かもしれない）: %v", err)
	}
	_ = victim.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, used := lockHolder(t, db, name); !used {
			t.Log("接続が切れると、そのセッションのロックは即座に解放される")
			t.Log("→ プロセスが落ちてもロックは残らない。これは GET_LOCK の良いところ")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("接続を切ってもロックが残っている")
}

// 実験8: 「そもそもロックは要るのか」— 無しで同時にマイグレーションすると何が起きるか。
func TestExperimentLock_ロック無しで同時にDDLを流す(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	mustExec(t, db, "DROP TABLE IF EXISTS exp_nolock_hist")
	mustExec(t, db, "DROP TABLE IF EXISTS exp_nolock")
	mustExec(t, db, `CREATE TABLE exp_nolock_hist (version INT PRIMARY KEY, applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3))`)
	defer func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_nolock_hist")
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_nolock")
	}()

	// 8つのコンテナが同時に「未適用ならDDLを流して記録する」を、ロック無しでやる
	migrate := func() error {
		var applied int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM exp_nolock_hist WHERE version = 1").Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE exp_nolock (id INT PRIMARY KEY)"); err != nil {
			return fmt.Errorf("DDL: %w", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO exp_nolock_hist (version) VALUES (1)"); err != nil {
			return fmt.Errorf("記録: %w", err)
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = migrate() }()
	}
	wg.Wait()

	failed := 0
	seen := map[string]int{}
	for _, err := range errs {
		if err == nil {
			continue
		}
		failed++
		msg := err.Error()
		if i := strings.Index(msg, ";"); i > 0 {
			msg = msg[:i]
		}
		seen[msg]++
	}
	t.Logf("ロック無しで8並列: %d/8 が失敗", failed)
	for msg, n := range seen {
		t.Logf("  %d 件: %s", n, msg)
	}
	var applied int
	if err := db.QueryRow("SELECT COUNT(*) FROM exp_nolock_hist").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	t.Logf("適用の記録: %d 件（1 であってほしい）", applied)
	t.Log("→ 起動のたびにこれが起きる。エラーで落ちたコンテナは再起動を繰り返す。")
	t.Log("  「DDL は結局1つしか通らないから大丈夫」ではなく、**残りは全部エラーになる** のが問題")
}

// 実験9: 排他の代金を測る。
func TestExperimentLock_代金(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	mustExec(t, db, "DROP TABLE IF EXISTS exp_lock_cost")
	mustExec(t, db, "CREATE TABLE exp_lock_cost (name VARCHAR(32) PRIMARY KEY) ENGINE=InnoDB")
	mustExec(t, db, "INSERT INTO exp_lock_cost VALUES ('x')")
	defer func() { _, _ = db.Exec("DROP TABLE IF EXISTS exp_lock_cost") }()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		if got := getLock(t, c, "exp_lock_cost", 1); got != 1 {
			t.Fatal("取れない")
		}
		if r := releaseLock(t, c, "exp_lock_cost"); r != 1 {
			t.Fatal("解放できない")
		}
	}
	userLock := time.Since(start) / n

	start = time.Now()
	for i := 0; i < n; i++ {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var name string
		if err := tx.QueryRowContext(ctx, "SELECT name FROM exp_lock_cost WHERE name='x' FOR UPDATE").Scan(&name); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	rowLock := time.Since(start) / n

	t.Logf("GET_LOCK + RELEASE_LOCK: %v /回（往復2回）", userLock.Round(time.Microsecond))
	t.Logf("行ロック（BEGIN + FOR UPDATE + COMMIT）: %v /回（往復3回）", rowLock.Round(time.Microsecond))
	t.Log("→ どちらも往復の回数ぶん。ロックの種類そのものの差ではない")
}

// 実験10: 長時間の「担当」を GET_LOCK でやるとどうなるか（リース方式との比較）。
func TestExperimentLock_長時間の担当には向かない(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	const tenants = 20

	// テナントごとに担当を GET_LOCK で表すと、テナント数ぶんの接続を占有し続ける
	conns := make([]*sql.Conn, 0, tenants)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	db.SetMaxOpenConns(20)
	for i := 0; i < tenants; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("%d 個目の接続が取れない: %v", i, err)
		}
		conns = append(conns, c)
		if got := getLock(t, c, fmt.Sprintf("exp_tenant_%02d", i), 1); got != 1 {
			t.Fatal("ロックが取れない")
		}
	}
	t.Logf("%d テナントの担当を GET_LOCK で保持 → 接続を %d 本、保持している間ずっと占有する", tenants, len(conns))

	// この状態では、通常の問い合わせに使える接続が無い
	shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var v int
	err := db.QueryRowContext(shortCtx, "SELECT 1").Scan(&v)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		t.Log("→ プールが枯渇し、通常の問い合わせが取れなくなった")
	} else {
		t.Logf("→ 通常の問い合わせ: %v（プール上限に余裕があった）", err)
	}
	for _, c := range conns {
		_ = c.Close()
	}
	conns = conns[:0]

	t.Log("→ 「誰が担当か」を長時間持つ用途に GET_LOCK は向かない。")
	t.Log("  接続を占有するうえ、期限も無い（アプリが固まって接続が生きていると、永久に持ち続ける）")
	t.Log("  この用途は internal/lease の『期限つきリース』が正しい（§2.8）")
}

// 実験11: ロックの代わりに「先に記録する」方式（insert-first）はどうか。
//
// 一意キーの取り合いで勝った1つだけが DDL を流す。ロックは要らない。
// ただし **途中で落ちたとき** に何が残るかが変わる。
func TestExperimentLock_先に記録する方式(t *testing.T) {
	db := mysqltest.Raw(t)
	ctx := context.Background()
	reset := func() {
		mustExec(t, db, "DROP TABLE IF EXISTS exp_ifirst")
		mustExec(t, db, "DROP TABLE IF EXISTS exp_ifirst_hist")
		mustExec(t, db, `CREATE TABLE exp_ifirst_hist (version INT PRIMARY KEY, done TINYINT NOT NULL DEFAULT 0)`)
	}
	reset()
	defer func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_ifirst")
		_, _ = db.Exec("DROP TABLE IF EXISTS exp_ifirst_hist")
	}()

	// ① 並行性: 8並列で「記録を取れた1つだけが DDL を流す」
	var applied, skipped, failed int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.ExecContext(ctx, "INSERT INTO exp_ifirst_hist (version) VALUES (1)")
			if err != nil {
				if strings.Contains(err.Error(), "1062") { // Duplicate entry
					atomic.AddInt32(&skipped, 1)
					return
				}
				atomic.AddInt32(&failed, 1)
				return
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE exp_ifirst (id INT PRIMARY KEY)"); err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			if _, err := db.ExecContext(ctx, "UPDATE exp_ifirst_hist SET done=1 WHERE version=1"); err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			atomic.AddInt32(&applied, 1)
		}()
	}
	wg.Wait()
	t.Logf("① 先に記録する方式・8並列: 適用 %d / 見送り %d / エラー %d", applied, skipped, failed)
	t.Log("   → ロック無しでも、エラーは出ない（ロック無しで DDL を撃ち合うと 7/8 が失敗した）")

	// ② 途中で落ちた場合: 記録だけ残って DDL は流れていない状態を作る
	reset()
	mustExec(t, db, "INSERT INTO exp_ifirst_hist (version) VALUES (1)") // 記録した直後に落ちた想定
	var done int
	if err := db.QueryRow("SELECT done FROM exp_ifirst_hist WHERE version=1").Scan(&done); err != nil {
		t.Fatal(err)
	}
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
	                       WHERE table_schema=DATABASE() AND table_name='exp_ifirst'`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	t.Logf("② 記録した直後に落ちた場合: 記録あり(done=%d) / 表は存在しない(%d)", done, exists)
	t.Log("   → 次の起動は「適用済み」と判断して飛ばす。**永久に当たらないマイグレーションが生まれる**")
	t.Log("   → 対策: done フラグを見て「開始したが終わっていない」を検出し、起動を止めて人に知らせる")

	// ③ GET_LOCK 方式で途中で落ちた場合: DDL は流れたが記録が無い
	reset()
	mustExec(t, db, "CREATE TABLE exp_ifirst (id INT PRIMARY KEY)") // DDL 後・記録前に落ちた想定
	_, err := db.ExecContext(ctx, "CREATE TABLE exp_ifirst (id INT PRIMARY KEY)")
	t.Logf("③ DDL 後・記録前に落ちた場合、次の起動で同じ DDL を流すと: %v", shortErr(err))
	t.Log("   → DDL を冪等に書く（CREATE TABLE IF NOT EXISTS / ALTER の前に存在確認）か、人が直す前提にする")
	t.Log("→ 結論: ロックは『同時に走らせない』ためのもので、『途中で落ちる』には効かない。両方別に手当てする")
}
