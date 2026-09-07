package resiliencelab_test

// EXP-33: DB フェイルオーバ/再起動への耐性（プールの自動回復・安全なリトライ）。
//
//	MYSQL_DSN=... go test ./internal/resiliencelab/ -run TestEXP33 -v
//
// DB が切れたとき、(1) アイドル接続が切られても次のクエリで自動回復するか、
// (2) 実行中のクエリは切られると error になる（自動リトライされない）か、を実測する。

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/resiliencelab"
)

func init() {
	// この実験はわざと接続を切るので、driver の「bad idle connection」ログは想定内。黙らせる。
	_ = mysql.SetLogger(log.New(io.Discard, "", 0))
}

func TestEXP33_DB切断への耐性(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)

	admin, err := resiliencelab.Open(dsn, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}

	rec := expkit.NewRecorder("EXP-33", "db-failover-resilience",
		"接続が切られたときのプール自動回復と、実行中クエリの扱い")
	rec.Env(expkit.CaptureEnv(ctx, admin))
	rec.Freeze(
		"1) プールのアイドル接続が DB 側で切られても、次のクエリで database/sql が新しい接続を張り直し、" +
			"成功する（自前の再接続コードは要らない）。 " +
			"2) 実行中のクエリが切られたら、それは error になり自動リトライされない。冪等な読みだけを" +
			"アプリ側で retry する。 " +
			"3) ConnMaxLifetime を短めにしておくと、フェイルオーバ後に古い接続を掴み続けない。")

	// 被験プール
	db, err := resiliencelab.Open(dsn, 5, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// ---- ① アイドル接続を切られても、次のクエリで自動回復 ----
	resiliencelab.Warm(ctx, db, 5)
	errsBefore := resiliencelab.RunN(ctx, db, 20)
	killed, err := resiliencelab.KillWorkerConns(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	errsAfter := resiliencelab.RunN(ctx, db, 50)
	rec.Add(expkit.Variant{
		Name:     "アイドル接続を切断 → 次のクエリで自動回復",
		Counters: map[string]int64{"killed": int64(killed), "errs_before": int64(errsBefore), "errs_after": int64(errsAfter)},
		Notes:    []string{"切断 " + itoa(int64(killed)) + " 本の後、50 クエリ中エラー " + itoa(int64(errsAfter)) + "（自動で張り直す）"},
	})
	t.Logf("回復: killed=%d errs_before=%d errs_after=%d", killed, errsBefore, errsAfter)

	// ---- ② 実行中のクエリが切られたら error（自動リトライされない）----
	resiliencelab.Warm(ctx, db, 5)
	inflightErr := make(chan error, 1)
	go func() { inflightErr <- resiliencelab.Inflight(ctx, db) }() // SLEEP(2) を実行中に
	time.Sleep(300 * time.Millisecond)
	_, _ = resiliencelab.KillWorkerConns(ctx, admin) // 実行中の接続も切る
	errInflight := <-inflightErr
	// 切断後、新しいクエリは成功する（回復）
	errsRecover := resiliencelab.RunN(ctx, db, 10)
	rec.Add(expkit.Variant{
		Name:     "実行中のクエリを切断 → その1本は error（自動リトライ無し）",
		Accident: true,
		Counters: map[string]int64{"inflight_failed": okI(errInflight != nil), "errs_after_recover": int64(errsRecover)},
		Notes:    []string{"実行中クエリ error: " + errStr(errInflight) + " / 以降の新クエリは回復"},
	})
	t.Logf("実行中切断: inflight_err=%v / 回復後エラー=%d", errInflight, errsRecover)

	// ---- 検証 ----
	if errsBefore != 0 {
		t.Errorf("切断前からエラーが出ている: %d", errsBefore)
	}
	if killed < 1 {
		t.Errorf("接続を切れていない（実験にならない）: killed=%d", killed)
	}
	if errsAfter > 2 { // 自動回復。多少の遷移は許容
		t.Errorf("アイドル切断後に回復していない: 50 中 %d エラー", errsAfter)
	}
	if errInflight == nil {
		t.Errorf("実行中のクエリが切られても error にならない（なるはず）")
	}
	if errsRecover != 0 {
		t.Errorf("実行中切断の後、新クエリが回復していない: %d", errsRecover)
	}

	rec.Scope(
		"MySQL 8.0 / 被験プール5本・ConnMaxLifetime 30s / 切断は worker スレッドの KILL で模す",
		"『アイドル切断→次で回復』は database/sql が ErrBadConn を検知して新接続で retry する挙動",
		"『実行中切断→error』は送信済みクエリなので retry されない（当然）",
	)
	rec.Uncertain(
		"実 DB のフェイルオーバは DNS 切替・read-only 昇格など、KILL より複雑（ここは接続断のみ）",
		"回復に要する時間・一時的な error 本数は、切断本数・プール・タイミングで動く",
		"トランザクション中の切断はロールバックされる。途中まで書いた副作用は冪等(EXP-27)で吸収する",
		"ConnMaxLifetime はフェイルオーバ後の『古い接続を掴み続ける』を短時間に抑えるための保険",
	)
	rec.Artifact(
		"internal/resiliencelab: 接続断とプール自動回復・実行中クエリの扱い",
		"docs/db-resilience.md: DB 切断/フェイルオーバへの耐性と安全なリトライ",
	)
	rec.Next("EXP-34 ノイジーネイバー（テナント公平性）")

	files, err := rec.Save(
		"アイドル接続が切られても database/sql が自動で張り直すので、自前の再接続は要らない。" +
			"必要なのは (a) ConnMaxLifetime を短めにして古い接続を掴み続けない、(b) 実行中に切られたクエリは" +
			"自動リトライされないので、冪等な読み/操作(EXP-27)だけをアプリ側で安全に retry する、の2点。")
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
func errStr(err error) string {
	if err == nil {
		return "(なし)"
	}
	return err.Error()
}
func itoa(n int64) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
