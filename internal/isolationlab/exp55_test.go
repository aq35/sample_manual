package isolationlab_test

// EXP-55: トランザクション分離レベル（REPEATABLE READ vs READ COMMITTED）。
//
//	MYSQL_DSN=... go test ./internal/isolationlab/ -run TestEXP55 -v
//
// MySQL 既定は RR。RR は tx 開始時点のスナップショットを読み続け（再読で同じ＝non-repeatable read
// が起きない）、範囲のロック読みで gap ロックを取り phantom INSERT を防ぐ。RC は文ごとに最新の確定値を
// 読み（再読で変わる）、gap ロックを取らないので競合は減るが phantom は起きうる。両方を実挙動で示す。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/isolationlab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP55_分離レベル(t *testing.T) {
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
	if err := isolationlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-55", "isolation-rr-vs-rc",
		"RR はスナップショットで再読安定＋gap ロックで phantom 防止、RC は最新読みで gap 取らない")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 非再読(non-repeatable read): RR は tx 内で再読しても同じ値（開始時スナップショット）。 " +
			"RC は別 tx の commit が見え、再読で値が変わる。 " +
			"2) phantom/gap: RR は範囲のロック読みで gap ロックを取り、隙間への INSERT をブロック(1205)。 " +
			"RC は gap を取らず INSERT が通る。 " +
			"3) RC は競合が減る代わりに phantom を許す（trade-off）。")

	// --- ① 非再読 ---
	rrFirst, rrSecond, err := isolationlab.NonRepeatableRead(ctx, db, "REPEATABLE READ")
	if err != nil {
		t.Fatal(err)
	}
	rcFirst, rcSecond, err := isolationlab.NonRepeatableRead(ctx, db, "READ COMMITTED")
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "RR: tx 内の再読は同じ値（スナップショット）",
		Counters: map[string]int64{"first": int64(rrFirst), "second": int64(rrSecond), "changed": b2i(rrFirst != rrSecond)},
		Notes:    []string{"1回目=" + itoa(rrFirst) + " → 2回目=" + itoa(rrSecond) + "（別 tx が +100 commit しても見えない）"},
	})
	rec.Add(expkit.Variant{
		Name:     "RC: 再読で最新の確定値に変わる（non-repeatable read）",
		Accident: true,
		Counters: map[string]int64{"first": int64(rcFirst), "second": int64(rcSecond), "changed": b2i(rcFirst != rcSecond)},
		Notes:    []string{"1回目=" + itoa(rcFirst) + " → 2回目=" + itoa(rcSecond) + "（別 tx の commit が見える）"},
	})

	// --- ② phantom / gap ロック ---
	rrBlocked, err := isolationlab.GapInsertBlocked(ctx, db, "REPEATABLE READ")
	if err != nil {
		t.Fatal(err)
	}
	rcBlocked, err := isolationlab.GapInsertBlocked(ctx, db, "READ COMMITTED")
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "RR: 範囲ロック読み中は隙間 INSERT を gap ロックでブロック（phantom 防止）",
		Counters: map[string]int64{"gap_insert_blocked": b2i(rrBlocked)},
		Notes:    []string{"k∈(10,30) を FOR UPDATE → 隙間 15 の INSERT は 1205（ブロック）"},
	})
	rec.Add(expkit.Variant{
		Name:     "RC: gap ロックを取らないので隙間 INSERT が通る（phantom・競合は少ない）",
		Accident: true,
		Counters: map[string]int64{"gap_insert_blocked": b2i(rcBlocked)},
		Notes:    []string{"同じ範囲でも隙間 15 の INSERT が成功する"},
	})
	t.Logf("RR: read %d→%d blocked=%v / RC: read %d→%d blocked=%v",
		rrFirst, rrSecond, rrBlocked, rcFirst, rcSecond, rcBlocked)

	// ---- 検証 ----
	if rrFirst != rrSecond {
		t.Errorf("RR で再読が変わった（スナップショットのはず）: %d→%d", rrFirst, rrSecond)
	}
	if rcSecond != rcFirst+100 {
		t.Errorf("RC で再読が最新に変わっていない: %d→%d（+100 のはず）", rcFirst, rcSecond)
	}
	if !rrBlocked {
		t.Errorf("RR で gap INSERT がブロックされていない（されるはず）")
	}
	if rcBlocked {
		t.Errorf("RC で gap INSERT がブロックされた（通るはず）")
	}

	rec.Scope(
		"MySQL 8.0 InnoDB / iso_row(k PK 10,20,30) / 2接続を db.Conn で固定・victim は lock_wait_timeout=1",
		"non-repeatable: 別 tx の commit が tx 内の再読に見えるか。phantom: 範囲ロック中に隙間 INSERT できるか",
		"gap ロックは『索引レンジのロック読み(FOR UPDATE)』で発生。読み取り専用の一貫性読みには不要",
	)
	rec.Uncertain(
		"RR の一貫性読み(非ロック SELECT)は MVCC スナップショット。ロック読み(FOR UPDATE/共有)は最新＋gap ロック",
		"RC は gap ロックが減り並行 INSERT に強いが、再読が変わる・phantom を許す。アプリが版/条件で守る必要",
		"デッドロック(1213)や外部キー・ユニーク競合は別軸（EXP-49/2）。ここは分離レベルの2挙動に集中",
		"binlog を ROW にすれば RC でもレプリケーションは安全（STATEMENT だと RC は不可）。本実験はロック挙動のみ",
	)
	rec.Artifact(
		"internal/isolationlab: NonRepeatableRead / GapInsertBlocked（RR/RC の実挙動）",
		"docs/isolation.md: 分離レベル RR vs RC（再読の安定・gap ロック）",
	)
	rec.Next("EXP-56 bulk INSERT の正規化")

	files, err := rec.Save(
		"MySQL 既定の RR は tx 開始時スナップショットを読み続け（再読が安定・non-repeatable read が起きない）、" +
			"範囲のロック読みで gap ロックを取り phantom INSERT を防ぐ。RC は文ごとに最新の確定値を読み" +
			"（再読で変わる）、gap ロックを取らないので並行 INSERT の競合は減るが phantom を許す。" +
			"既定の RR のままで多くは安全。ロック競合が問題で phantom をアプリ側（版・一意制約）で守れるなら " +
			"RC を検討する、という順で選ぶ。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func b2i(b bool) int64 {
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
