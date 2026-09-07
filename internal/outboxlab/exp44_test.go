package outboxlab_test

// EXP-44: トランザクショナル outbox で外部作用を exactly-once に。
//
//	MYSQL_DSN=... go test ./internal/outboxlab/ -run TestEXP44 -v
//
// 「DB 更新」と「外部呼び出し」の間で落ちると、素朴だと二重指示 or 未送信。outbox（意図を原子的に
// 記録→relay が at-least-once 送信）＋冪等な受け側 = 実質 exactly-once。同じ crash 注入で比べる。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/outboxlab"
)

func TestEXP44_outbox_exactly_once(t *testing.T) {
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
	const tenant = "ob"
	if err := outboxlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-44", "transactional-outbox",
		"DB 更新と外部呼び出しをまたぐ crash で二重/未送信を出さない（outbox＋冪等）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 素朴（外部送信→done、非冪等）: 送信直後・done 前に落ちると再送で二重効果（effects > 命令数）。 " +
			"2) outbox（意図を原子的に記録→relay が at-least-once 送信）＋冪等受け側: 再送があっても効果は" +
			"命令数ぴったり（exactly-once）。未送信も残らない（pending=0）。 " +
			"3) 同じ crash 注入（3件で送信後 mark 前に落ちる）で比較する。")

	const n = 20
	// 3 の倍数の命令で「送信直後・確定前に落ちる」を注入
	crash := func(id int64) bool { return id%3 == 0 } // 0,3,6,9,12,15,18 = 7件
	crashN := 7

	// ---- ① 素朴（outbox 無し・非冪等）----
	naiveRobot := outboxlab.NewRobot()
	outboxlab.NaiveDeliver(n, naiveRobot, func(i int) bool { return crash(int64(i)) })
	rec.Add(expkit.Variant{
		Name:     "素朴: 外部送信→done・非冪等（crash 再送で二重効果）",
		Accident: true,
		Counters: map[string]int64{"commands": n, "attempts": int64(naiveRobot.Attempts()), "effects": int64(naiveRobot.Effects())},
		Notes:    []string{"効果 " + itoa(naiveRobot.Effects()) + " > 命令 " + itoa(n) + "（" + itoa(crashN) + "件が二重に効いた）"},
	})
	t.Logf("naive: attempts=%d effects=%d (commands=%d)", naiveRobot.Attempts(), naiveRobot.Effects(), n)

	// ---- ② outbox＋冪等 ----
	if err := outboxlab.SeedOutbox(ctx, db, tenant, n); err != nil {
		t.Fatal(err)
	}
	obRobot := outboxlab.NewRobot()
	// 1周目: crash 注入（該当行は送信後 sent 前に落ちる → pending 残留）
	if err := outboxlab.Relay(ctx, db, tenant, obRobot, crash); err != nil {
		t.Fatal(err)
	}
	pendAfter1, _ := outboxlab.PendingCount(ctx, db, tenant)
	// 2周目: crash 無し（残った pending を再送＋sent。冪等なので効果は増えない）
	if err := outboxlab.Relay(ctx, db, tenant, obRobot, nil); err != nil {
		t.Fatal(err)
	}
	pendAfter2, _ := outboxlab.PendingCount(ctx, db, tenant)
	rec.Add(expkit.Variant{
		Name:     "outbox＋冪等: at-least-once 送信でも効果は命令数ぴったり",
		Counters: map[string]int64{"commands": n, "attempts": int64(obRobot.Attempts()), "effects": int64(obRobot.Effects()), "pending_after1": int64(pendAfter1), "pending_after2": int64(pendAfter2)},
		Notes:    []string{"送信 " + itoa(obRobot.Attempts()) + " 回（再送含む）だが効果 " + itoa(obRobot.Effects()) + " = 命令 " + itoa(n) + "。未送信 " + itoa(pendAfter2)},
	})
	t.Logf("outbox: attempts=%d effects=%d pending %d→%d (commands=%d)",
		obRobot.Attempts(), obRobot.Effects(), pendAfter1, pendAfter2, n)

	// ---- 検証 ----
	// 素朴は二重効果（crash 件数ぶん多い）
	if naiveRobot.Effects() <= n {
		t.Errorf("素朴で二重効果が出ていない: effects=%d commands=%d", naiveRobot.Effects(), n)
	}
	if naiveRobot.Effects() != n+crashN {
		t.Errorf("素朴の二重効果数が想定と違う: effects=%d（%d のはず）", naiveRobot.Effects(), n+crashN)
	}
	// outbox は exactly-once（効果=命令数）だが送信は再送で多い（at-least-once）
	if obRobot.Effects() != n {
		t.Errorf("outbox で効果が命令数と違う: effects=%d（%d のはず）", obRobot.Effects(), n)
	}
	if obRobot.Attempts() <= n {
		t.Errorf("outbox で再送が起きていない（crash 注入が効いていない）: attempts=%d", obRobot.Attempts())
	}
	if pendAfter1 != crashN {
		t.Errorf("1周目後の未送信が想定と違う: %d（%d のはず）", pendAfter1, crashN)
	}
	if pendAfter2 != 0 {
		t.Errorf("2周目後も未送信が残っている: %d（0 のはず）", pendAfter2)
	}

	rec.Scope(
		"MySQL 8.0 / outbox 20件・3の倍数(7件)で送信後 sent 前に crash / 外部はメモリの擬似ロボット",
		"効果=distinct な適用（冪等受け側は idem_key で重複無視）。送信=呼び出し回数（再送含む）",
		"業務変更と outbox 行は同一トランザクションで書く（ここでは outbox 投入で代表）",
	)
	rec.Uncertain(
		"『原子的に業務変更＋outbox』の tx は SeedOutbox で代表。実装では業務行の INSERT/UPDATE と同 tx",
		"relay の並行実行は claim（原子的 UPDATE・EXP-30）で1件1レプリカに。ここは単一 relay",
		"外部の冪等性は受け側の責務（idem_key）。受け側が冪等でないなら二重効果は防げない",
		"outbox のパージ（sent の掃除）は保持期間で（EXP-15 の DROP PARTITION 等）",
	)
	rec.Artifact(
		"internal/outboxlab: outbox テーブル・relay・冪等/非冪等の外部擬似",
		"docs/outbox.md: トランザクショナル outbox と exactly-once 外部作用",
	)
	rec.Next("EXP-45 poison / dead-letter")

	files, err := rec.Save(
		"DB 更新と外部呼び出しをまたぐ処理は、業務変更と『送信意図(outbox)』を1トランザクションで原子的に" +
			"書き、別 relay が at-least-once で送る。外部を冪等(idem_key)にすれば、crash による再送があっても" +
			"効果は命令数ぴったり（exactly-once）で、未送信も残らない。素朴な『送信→done・非冪等』は" +
			"crash 再送で二重効果になる。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
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
