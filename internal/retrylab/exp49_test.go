package retrylab_test

// EXP-49: 一時 vs 恒久エラーの分類とリトライ。
//
//	MYSQL_DSN=... go test ./internal/retrylab/ -run TestEXP49 -v
//
// 何でもリトライしてはいけない。恒久エラー（重複キー 1062・構文など）は何度叩いても失敗する＝
// 即失敗(fail-fast)すべき。一時エラー（デッドロック 1213・ロック待ちタイムアウト 1205・接続断）
// だけバックオフして再試行する。分類器 Retryable で判定し、実際の MySQL エラーで挙動を測る。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/retrylab"
)

func TestEXP49_一時vs恒久エラーの分類(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	if err := retrylab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-49", "retry-classification",
		"恒久エラーは fail-fast・一時エラーだけバックオフ再試行する")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 恒久エラー（重複キー 1062）は Retryable=false → Do は1回で返る（fail-fast、無駄叩きしない）。 " +
			"2) 一時エラー（ロック待ちタイムアウト 1205）は Retryable=true → Do はバックオフして再試行し、" +
			"ロックが解けたら成功する（attempts=2）。 " +
			"3) 何でも retry する素朴版は、恒久エラーで maxAttempts 回まで無駄に叩く。")

	// ---- ① 恒久エラー: 重複キー(1062) は fail-fast ----
	var permErr error
	permAttempts, permErr := retrylab.Do(5, 20*time.Millisecond, func() error {
		return retrylab.InsertDup(ctx, db) // id=1 を再 INSERT → 1062
	})
	var permMy *mysql.MySQLError
	permIs1062 := errors.As(permErr, &permMy) && permMy.Number == 1062

	// 素朴に「何でも 5 回 retry」した場合の叩き回数（比較）
	naiveHits := 0
	for i := 0; i < 5; i++ {
		naiveHits++
		if retrylab.InsertDup(ctx, db) == nil {
			break
		}
	}

	rec.Add(expkit.Variant{
		Name:     "恒久エラー(1062): 分類で即失敗 → 1回だけ",
		Counters: map[string]int64{"attempts": int64(permAttempts), "is_1062": okI(permIs1062), "naive_hits": int64(naiveHits)},
		Notes:    []string{"分類あり=" + itoa(permAttempts) + " 回で確定 / 素朴に何でも retry すると " + itoa(naiveHits) + " 回無駄叩き"},
	})

	// ---- ② 一時エラー: ロック待ちタイムアウト(1205) は retry して成功 ----
	// 別トランザクションで row(1) に X ロックを掛け、~1.2s 後に解放する。
	release, err := retrylab.HoldLock(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	releasedAt := make(chan struct{})
	go func() {
		time.Sleep(1200 * time.Millisecond)
		release() // ロック解放
		close(releasedAt)
	}()

	// victim 接続（innodb_lock_wait_timeout=1）で同じ行を UPDATE。
	// attempt1: ロック保持中 → 1秒待って 1205（retryable）→ backoff。
	// attempt2: この頃ロックが解けている → 成功。
	victim, err := retrylab.LockVictimConn(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Close() }()

	var lastErrRetryable int64 = -1
	t0 := time.Now()
	transAttempts, transErr := retrylab.Do(5, 300*time.Millisecond, func() error {
		_, e := victim.ExecContext(ctx, "UPDATE retry_row SET v = v + 1 WHERE id = 1")
		if e != nil {
			lastErrRetryable = okI(retrylab.Retryable(e))
		}
		return e
	})
	transElapsed := time.Since(t0)
	<-releasedAt // goroutine 完了を待つ

	rec.Add(expkit.Variant{
		Name:     "一時エラー(1205): retry して成功",
		Counters: map[string]int64{"attempts": int64(transAttempts), "succeeded": okI(transErr == nil), "first_err_retryable": lastErrRetryable},
		Metrics:  map[string]float64{"total_ms": float64(transElapsed.Milliseconds())},
		Notes:    []string{"attempt1 で 1205（retryable）→ backoff → attempt2 で成功。計 " + itoa(transAttempts) + " 回・" + transElapsed.String()},
	})
	t.Logf("perm: attempts=%d is1062=%v naive=%d / trans: attempts=%d err=%v elapsed=%v",
		permAttempts, permIs1062, naiveHits, transAttempts, transErr, transElapsed)

	// ---- 分類器の直接確認 ----
	dupErr := retrylab.InsertDup(ctx, db)
	classDup := retrylab.Retryable(dupErr)  // false のはず
	class1205 := retrylab.Retryable(&mysql.MySQLError{Number: 1205})
	class1213 := retrylab.Retryable(&mysql.MySQLError{Number: 1213})
	classNil := retrylab.Retryable(nil)

	rec.Add(expkit.Variant{
		Name:     "分類器 Retryable: 1213/1205=true・1062=false・nil=false",
		Counters: map[string]int64{"c_1062": okI(classDup), "c_1205": okI(class1205), "c_1213": okI(class1213), "c_nil": okI(classNil)},
	})

	// ---- 検証 ----
	if !permIs1062 {
		t.Errorf("恒久エラーが 1062 でない: %v", permErr)
	}
	if permAttempts != 1 {
		t.Errorf("恒久エラーで fail-fast していない: attempts=%d（1 のはず）", permAttempts)
	}
	if naiveHits <= permAttempts {
		t.Errorf("素朴 retry が無駄叩きしていない（するはず）: naive=%d", naiveHits)
	}
	if transErr != nil {
		t.Errorf("一時エラーが retry で成功していない: %v", transErr)
	}
	if transAttempts < 2 {
		t.Errorf("一時エラーで retry していない: attempts=%d（>=2 のはず）", transAttempts)
	}
	if lastErrRetryable != 1 {
		t.Errorf("一時エラーが retryable と判定されていない: %d", lastErrRetryable)
	}
	if classDup {
		t.Errorf("Retryable(1062) が true（false のはず）")
	}
	if !class1205 || !class1213 {
		t.Errorf("Retryable(1205/1213) が false（true のはず）: 1205=%v 1213=%v", class1205, class1213)
	}
	if classNil {
		t.Errorf("Retryable(nil) が true（false のはず）")
	}

	rec.Scope(
		"MySQL 8.0 / retry_row 1行 / victim 接続は innodb_lock_wait_timeout=1 / backoff 300ms・max5",
		"恒久=1062 は再 INSERT で必ず起きる。一時=1205 は別 tx が X ロック保持中に UPDATE して起こす",
		"接続断(driver.ErrBadConn)も retryable（張り直し・EXP-33）。ここでは 1205 で代表",
	)
	rec.Uncertain(
		"デッドロック 1213 は片方が犠牲になり自動ロールバック。tx 丸ごと retry する（部分再実行は不可）",
		"一時と恒久の境界は文脈次第（一意制約違反は基本恒久だが『後勝ちにしたい』設計なら upsert に）",
		"backoff は固定でなく指数＋ジッタが実務的（雪崩防止）。max回数と全体タイムアウトの両方で上限を",
		"『何でも retry』は恒久エラーを隠して DB を無駄に叩く。分類してから retry するのが要点",
	)
	rec.Artifact(
		"internal/retrylab: Retryable 分類器と Do リトライループ",
		"docs/retry.md: 一時 vs 恒久エラーの分類とリトライ",
	)
	rec.Next("（このセットの最後）")

	files, err := rec.Save(
		"リトライは『分類してから』。恒久エラー（重複キー・構文）は何度叩いても失敗する＝即失敗し、" +
			"一時エラー（デッドロック 1213・ロック待ち 1205・接続断）だけバックオフ再試行する。分類器 " +
			"Retryable で判定し、恒久は1回・一時はロックが解けたら成功（attempts=2）。何でも retry すると" +
			"恒久エラーを隠して DB を無駄に叩く。backoff は指数＋ジッタ、max回数と全体タイムアウトで上限を。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func okI(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
