package repo_test

// リポジトリ層が、実験で起きた事故を実際に止めることの確認。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run 'TestScope|TestRepo|TestTx|TestPage' -v

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/repo"
	"github.com/aq35/sample_manual/internal/repo/repotest"
)

const (
	tenantA = model.TenantID("t-alpha")
	tenantB = model.TenantID("t-bravo")
)

var profiles repo.ProfileRepo

func newDB(t *testing.T) *repo.DB {
	t.Helper()
	dsn := mysqltest.DSN(t)
	db, err := repo.Open(dsn, repo.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxTxDuration: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.Ping(ctx); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	if err := repo.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, tn := range []model.TenantID{tenantA, tenantB} {
		if _, err := db.SQL().ExecContext(ctx, "DELETE FROM robot_profile WHERE tenant_id = ?", string(tn)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func seedProfiles(t *testing.T, db *repo.DB, tenant model.TenantID, n int) {
	t.Helper()
	ctx := context.Background()
	sc := db.Tenant(tenant)
	for i := 0; i < n; i++ {
		p := repo.Profile{
			RobotID:   fmt.Sprintf("r%04d", i),
			Name:      fmt.Sprintf("ロボット%d", i),
			ModelName: "AGV-3000",
			Serial:    fmt.Sprintf("%s-SN%04d", tenant, i),
		}
		if err := profiles.Create(ctx, sc, p); err != nil {
			t.Fatal(err)
		}
	}
}

// seedBulk は実行計画のテスト用に、本番に近い件数をまとめて入れる。
func seedBulk(t *testing.T, db *repo.DB, tenant model.TenantID, n int) {
	t.Helper()
	ctx := context.Background()
	const batch = 500
	vals := make([]string, 0, batch)
	args := make([]any, 0, batch*5)
	flush := func() {
		if len(vals) == 0 {
			return
		}
		q := "INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial) VALUES " +
			strings.Join(vals, ",")
		if _, err := db.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
		vals, args = vals[:0], args[:0]
	}
	for i := 0; i < n; i++ {
		vals = append(vals, "(?,?,?,?,?)")
		args = append(args, string(tenant), fmt.Sprintf("r%06d", i),
			fmt.Sprintf("ロボット%d", i), "AGV-3000", fmt.Sprintf("%s-SN%06d", tenant, i))
		if len(vals) >= batch {
			flush()
		}
	}
	flush()
	if _, err := db.SQL().ExecContext(ctx, "ANALYZE TABLE robot_profile"); err != nil {
		t.Fatal(err)
	}
}

// 事故3の対策: テナント指定を忘れた SQL は、そもそも実行されない。
func TestScope_テナント指定の無いSQLは実行しない(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 3)
	seedProfiles(t, db, tenantB, 3)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	cases := []struct {
		name  string
		run   func() error
		errIs error
	}{
		{"tenant_id を書き忘れた UPDATE", func() error {
			_, err := sc.Exec(ctx, "test", "UPDATE robot_profile SET name='x' WHERE robot_id = ?", repo.ExpectOne, "r0000")
			return err
		}, repo.ErrMissingTenant},
		{"tenant_id を書き忘れた SELECT", func() error {
			_, err := sc.Query(ctx, "test", "SELECT robot_id FROM robot_profile WHERE robot_id = ? LIMIT 1", "r0000")
			return err
		}, repo.ErrMissingTenant},
		{"SET 側にだけ :tenant がある UPDATE", func() error {
			_, err := sc.Exec(ctx, "test", "UPDATE robot_profile SET name = :tenant WHERE robot_id = ?", repo.ExpectOne, "r0000")
			return err
		}, repo.ErrMissingTenant},
		{"WHERE の無い UPDATE", func() error {
			_, err := sc.Exec(ctx, "test", "UPDATE robot_profile SET name = :tenant", repo.ExpectAny)
			return err
		}, repo.ErrUnsafeStatement},
		{"WHERE の無い DELETE", func() error {
			_, err := sc.Exec(ctx, "test", "DELETE FROM robot_profile", repo.ExpectAny)
			return err
		}, repo.ErrMissingTenant},
		{"LIMIT の無い SELECT", func() error {
			_, err := sc.Query(ctx, "test", "SELECT robot_id FROM robot_profile WHERE tenant_id = :tenant")
			return err
		}, repo.ErrTooManyRows},
	}
	for _, c := range cases {
		err := c.run()
		if !errors.Is(err, c.errIs) {
			t.Errorf("%s: %v を期待したが %v", c.name, c.errIs, err)
			continue
		}
		t.Logf("止めた: %-34s → %v", c.name, rootMessage(err))
	}

	// 他テナントのデータが無事であること
	if n, err := profiles.Count(ctx, db.Tenant(tenantB)); err != nil || n != 3 {
		t.Fatalf("他テナントの行が壊れている: %d %v", n, err)
	}

	// 全件が必要なら、明示すれば通る（使う場所が目で見える）
	rows, err := sc.AllowUnbounded().Query(ctx, "test",
		"SELECT robot_id FROM robot_profile WHERE tenant_id = :tenant")
	if err != nil {
		t.Fatalf("AllowUnbounded でも通らない: %v", err)
	}
	_ = rows.Close()
	t.Log("AllowUnbounded を明示すれば LIMIT 無しでも通る（全件同期などの逃げ道）")
}

// テナントをまたいで見えないこと。
func TestScope_他テナントの行は見えない(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 2)
	seedProfiles(t, db, tenantB, 2)
	ctx := context.Background()

	// 同じ robot_id が両テナントに存在する
	pA, err := profiles.Get(ctx, db.Tenant(tenantA), "r0000")
	if err != nil {
		t.Fatal(err)
	}
	pB, err := profiles.Get(ctx, db.Tenant(tenantB), "r0000")
	if err != nil {
		t.Fatal(err)
	}
	if pA.Serial == pB.Serial {
		t.Fatal("テナントを跨いで同じ行を読んでいる")
	}

	// 片方を消しても、もう片方は残る
	if err := profiles.Delete(ctx, db.Tenant(tenantA), "r0000"); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Get(ctx, db.Tenant(tenantA), "r0000"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("消えていない: %v", err)
	}
	if _, err := profiles.Get(ctx, db.Tenant(tenantB), "r0000"); err != nil {
		t.Fatalf("他テナントの行まで消えた: %v", err)
	}
	t.Log("同じ robot_id が別テナントにあっても、互いに見えない・消せない")
}

// 事故2の対策: 影響行数が宣言と違えばロールバックする。
func TestScope_影響行数が想定と違えばロールバックする(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 5)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	// 「1台の名前を変える」つもりで、条件を model_name にしてしまった
	const q = `UPDATE robot_profile SET name = ? WHERE tenant_id = :tenant AND model_name = ?`
	n, err := sc.Exec(ctx, "test.rename", q, repo.ExpectOne, "書き換え", "AGV-3000")
	if !errors.Is(err, repo.ErrUnexpectedRowCount) {
		t.Fatalf("止まっていない: n=%d err=%v", n, err)
	}
	t.Logf("止めた: 1行のはずが %d 行に当たったのでロールバック", n)

	// ★ロールバックされているので、1行も書き換わっていない
	page, err := profiles.List(ctx, sc, repo.Keyset{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Items {
		if p.Name == "書き換え" {
			t.Fatalf("ロールバックされていない: %+v", p)
		}
	}
	t.Log("トランザクションの外から呼んでも、暗黙のトランザクションで包んで巻き戻す")
}

// 事故1の対策: 読んで書くときは version で守る。
func TestRepo_楽観ロック(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 1)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	p, err := profiles.Get(ctx, sc, "r0000")
	if err != nil {
		t.Fatal(err)
	}
	// 別の処理が先に更新した
	if err := profiles.Rename(ctx, sc, "r0000", "先に変えた", p.Version); err != nil {
		t.Fatal(err)
	}
	// こちらは古い version を持っている
	err = profiles.Rename(ctx, sc, "r0000", "後から変えた", p.Version)
	if !errors.Is(err, repo.ErrOptimisticLock) {
		t.Fatalf("更新の消失を検出できていない: %v", err)
	}
	t.Logf("止めた: 古い version での更新 → %v", rootMessage(err))

	if !repo.Retryable(err) {
		t.Fatal("やり直してよいエラーとして分類されていない")
	}
	after, getErr := profiles.Get(ctx, sc, "r0000")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.Name != "先に変えた" {
		t.Fatalf("先の更新が消えている: %+v", after)
	}
	t.Log("呼び出し側は Retryable(err) を見て、読み直してやり直せばよい")
}

// 存在しない行の更新・重複登録が、型で分かること。
func TestRepo_エラーが型で分かる(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 1)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	if _, err := profiles.Get(ctx, sc, "無い"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("ErrNotFound でない: %v", err)
	}
	dup := repo.Profile{RobotID: "r0000", Name: "重複", ModelName: "AGV-3000", Serial: string(tenantA) + "-SN9999"}
	if err := profiles.Create(ctx, sc, dup); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("ErrConflict でない: %v", err)
	}
	if err := profiles.Delete(ctx, sc, "無い"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("ErrNotFound でない: %v", err)
	}
	t.Log("ErrNotFound / ErrConflict / ErrOptimisticLock で分岐できる（driver のエラー番号を業務コードに漏らさない）")
}

// キーセット法のページ送りが、全件を重複なく返すこと。
func TestPage_キーセットで全件たどれる(t *testing.T) {
	db := newDB(t)
	const total = 25
	seedProfiles(t, db, tenantA, total)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	seen := map[string]bool{}
	ks := repo.Keyset{Limit: 10}
	pages := 0
	for {
		page, err := profiles.List(ctx, sc, ks)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, p := range page.Items {
			if seen[p.RobotID] {
				t.Fatalf("同じ行が2回出た: %s", p.RobotID)
			}
			seen[p.RobotID] = true
		}
		if !page.HasMore {
			break
		}
		ks.After = page.Next
		if pages > 10 {
			t.Fatal("終わらない")
		}
	}
	if len(seen) != total {
		t.Fatalf("取れた件数が違う: %d / %d", len(seen), total)
	}
	t.Logf("%d 件を %d ページで、重複も欠けもなくたどれた", total, pages)

	// 上限を超える取得は止める
	if _, err := profiles.List(ctx, sc, repo.Keyset{Limit: 100000}); !errors.Is(err, repo.ErrTooManyRows) {
		t.Fatalf("上限を超える取得が止まっていない: %v", err)
	}
}

// 実行計画をテストで固定する（性能を崩す変更をコミットさせない）。
func TestRepo_実行計画を固定する(t *testing.T) {
	db := newDB(t)
	// ★実行計画を固定するテストは、本番に近い件数で行うこと。
	// 行が少ないとオプティマイザの選択が変わり、本番と違う計画を「固定」してしまう
	// （50行だと uq_tenant_serial + filesort、5,000行だと PRIMARY の range になる）。
	seedBulk(t, db, tenantA, 5000)
	sc := db.Tenant(tenantA)

	const list = `SELECT robot_id, name, model_name, serial, version, updated_at
	                FROM robot_profile
	               WHERE tenant_id = :tenant AND robot_id > ?
	            ORDER BY robot_id
	               LIMIT ?`
	repotest.RequireIndexed(t, sc, "profile.List", list, "", 10)
	repotest.RequireRowsScanned(t, sc, 5000, "profile.List", list, "", 10)

	const get = `SELECT name FROM robot_profile WHERE tenant_id = :tenant AND robot_id = ?`
	repotest.RequireIndexed(t, sc, "profile.Get", get, "r000000")
	repotest.RequireRowsScanned(t, sc, 1, "profile.Get", get, "r000000")

	// 索引の無い列で絞ると全表走査になる。それをテストが見つける
	const bad = `SELECT robot_id FROM robot_profile WHERE tenant_id = :tenant AND name = ? LIMIT 10`
	plan, err := sc.Explain(context.Background(), "test.bad", bad, "ロボット1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("索引の無い列で絞った場合の計画: %s", plan[0])
	t.Logf("→ テナント内 %d 行を舐める。RequireRowsScanned(t, sc, 100, ...) ならここで落ちる", plan[0].Rows)
}

// N+1 を、問い合わせ回数で検出する。
func TestRepo_N1を回数で検出する(t *testing.T) {
	db := newDB(t)
	const n = 20
	seedProfiles(t, db, tenantA, n)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("r%04d", i)
	}

	// まとめて引けば1回
	repotest.RequireQueries(t, db, 1, func() {
		got, err := profiles.GetMany(ctx, sc, ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n {
			t.Fatalf("取得件数が違う: %d", len(got))
		}
	})
	t.Log("GetMany は 1 回の問い合わせで済む")

	// 1件ずつ引くと n 回になる（これを検出できることの確認）
	before := db.Stats()
	for _, id := range ids {
		if _, err := profiles.Get(ctx, sc, id); err != nil {
			t.Fatal(err)
		}
	}
	after := db.Stats()
	t.Logf("1件ずつ引くと %d 回（テストで回数を縛れば、N+1 はレビュー前に落ちる）",
		after.Queries-before.Queries)
}

// デッドロックは自動でやり直す。
func TestTx_デッドロックは自動でやり直す(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 2)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	const q = `UPDATE robot_profile SET version = version + 1
	            WHERE tenant_id = :tenant AND robot_id = ?`
	touch := func(order []string) error {
		return sc.Tx(ctx, func(tx *repo.Scope) error {
			for i, id := range order {
				if i > 0 {
					time.Sleep(120 * time.Millisecond)
				}
				if _, err := tx.Exec(ctx, "test.touch", q, repo.ExpectOne, id); err != nil {
					return err
				}
			}
			return nil
		})
	}

	before := db.Stats()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = touch([]string{"r0000", "r0001"}) }()
	go func() {
		defer wg.Done()
		time.Sleep(60 * time.Millisecond)
		errs[1] = touch([]string{"r0001", "r0000"})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("%d 番目が失敗した（やり直しで吸収できていない）: %v", i, err)
		}
	}
	after := db.Stats()
	t.Logf("すれ違う順序で更新 → デッドロックを %d 回やり直して、どちらも成功した",
		after.Retries-before.Retries)
	if after.Retries == before.Retries {
		t.Log("注意: この実行ではデッドロックが起きなかった（タイミング次第）")
	}
	t.Log("※ やり直しても安全なのは、トランザクションの中で外部 API を呼んでいないから")
}

// 入れ子の Tx は、同じトランザクションに参加する（二重コミットしない）。
func TestTx_入れ子は同じトランザクションになる(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 1)
	ctx := context.Background()
	sc := db.Tenant(tenantA)

	before := db.Stats()
	err := sc.Tx(ctx, func(tx *repo.Scope) error {
		if err := profiles.Rename(ctx, tx, "r0000", "外側", 0); err != nil {
			return err
		}
		// 内側でも Tx を呼ぶ（リポジトリを組み合わせると普通に起きる）
		return tx.Tx(ctx, func(inner *repo.Scope) error {
			if !inner.InTx() {
				t.Error("内側がトランザクションになっていない")
			}
			return fmt.Errorf("内側で失敗した")
		})
	})
	if err == nil {
		t.Fatal("エラーが伝わっていない")
	}
	after := db.Stats()
	if n := after.Txs - before.Txs; n != 1 {
		t.Fatalf("トランザクションが %d 個できた（入れ子になっている）", n)
	}
	p, err := profiles.Get(ctx, sc, "r0000")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name == "外側" {
		t.Fatal("内側の失敗で外側が巻き戻っていない")
	}
	t.Log("入れ子の Tx は同じトランザクションに参加し、内側の失敗で全体が巻き戻る")
}

// テナントを跨ぐ操作は、明示したときだけできる（そして記録される）。
func TestScope_テナントを跨ぐには明示が要る(t *testing.T) {
	db := newDB(t)
	seedProfiles(t, db, tenantA, 2)
	seedProfiles(t, db, tenantB, 2)
	ctx := context.Background()

	before := db.Stats()
	sc := db.Unscoped("移行バッチ: 全テナントの件数を数える")
	var n int64
	if err := sc.QueryRow(ctx, "admin.count", "SELECT COUNT(*) FROM robot_profile").Scan(&n); err != nil {
		t.Fatal(err)
	}
	after := db.Stats()
	if n < 4 {
		t.Fatalf("全テナントを数えられていない: %d", n)
	}
	if after.CrossTenant-before.CrossTenant != 1 {
		t.Fatal("テナントを跨ぐ操作が記録されていない")
	}
	t.Logf("Unscoped は理由つきで記録される（%d 件を横断して数えた）", n)
}

// DSN に sql_safe_updates が必ず付くこと。
func TestScope_安全装置が有効になっている(t *testing.T) {
	db := newDB(t)
	var v int
	if err := db.SQL().QueryRow("SELECT @@SESSION.sql_safe_updates").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatal("sql_safe_updates が有効になっていない")
	}
	t.Log("Open が DSN に sql_safe_updates=1 を強制する（アプリ側の検査をすり抜けた分の最後の網）")
}

func rootMessage(err error) string {
	var re *repo.Error
	if errors.As(err, &re) {
		return re.Err.Error()
	}
	return err.Error()
}
