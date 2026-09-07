package gqllab_test

// EXP-28: 行レベル認可（この robot を操作してよいか）。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP28 -v
//
// @auth（EXP-25）が「どのフィールドか」を守るのに対し、行レベルは「どの行（対象）か」を守る。
// 同じテナント内でも、主体（operator）は許可された robot だけを操作できる。対象と主体に依るので
// ディレクティブでなくリゾルバで判定し、拒否時は書き込まない。

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/99designs/gqlgen/client"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/gqllab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP28_行レベル認可(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.Seed(ctx, db, tenantA, 5, 0); err != nil {
		t.Fatal(err)
	}
	// u-op は r0000 だけ操作できる。r0001 の grant は無い。u-other は何も持たない。
	const opAllowed, opOther = "u-op", "u-other"
	if err := gqllab.GrantOperator(ctx, db, tenantA, opAllowed, "r0000"); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-28", "gqlgen-row-level-authz",
		"行レベル認可: この robot を操作してよいか（対象×主体）")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) grant のある (u-op, r0000) は操作できる。 " +
			"2) 同じテナント内でも grant の無い robot（u-op, r0001）は拒否。 " +
			"3) grant を持たない別主体（u-other, r0000）は、他人が操作できる robot でも拒否。 " +
			"4) 拒否時は命令を書き込まない（行を作らない）。")

	cli := newMutationClient(db, gql.DefaultServerConfig())
	send := func(user, robotID, idem string) (sendResp, error) {
		var r sendResp
		opts := []client.Option{
			client.AddHeader("X-Tenant", tenantA),
			client.AddHeader("X-Role", "OPERATOR"),
			client.AddHeader("X-User", user),
			sendVars(robotID, "move", idem),
		}
		return r, cli.Post(mutQuery, &r, opts...)
	}

	// ① 許可された robot → 成功
	okResp, okErr := send(opAllowed, "r0000", "e28-ok")

	// ② 同テナントだが grant 無しの robot → 拒否・行を作らない
	_, denyErr := send(opAllowed, "r0001", "e28-deny-robot")
	denyRows, _ := gqllab.CountCommandsByIdem(ctx, db, tenantA, "e28-deny-robot")

	// ③ grant を持たない別主体 → 他人が操作できる robot でも拒否
	_, otherErr := send(opOther, "r0000", "e28-deny-user")
	otherRows, _ := gqllab.CountCommandsByIdem(ctx, db, tenantA, "e28-deny-user")

	rec.Add(expkit.Variant{
		Name:     "grant あり (u-op, r0000) → 操作できる",
		Counters: map[string]int64{"ok": b2i(okErr == nil && okResp.SendCommand.ID != "")},
	})
	rec.Add(expkit.Variant{
		Name:     "grant 無しの robot (u-op, r0001) → 拒否・行を作らない",
		Accident: true,
		Counters: map[string]int64{"rejected": b2i(denyErr != nil), "rows": int64(denyRows)},
		Notes:    []string{"error: " + errStr(denyErr)},
	})
	rec.Add(expkit.Variant{
		Name:     "別主体 (u-other, r0000) → 他人が操作できる robot でも拒否",
		Accident: true,
		Counters: map[string]int64{"rejected": b2i(otherErr != nil), "rows": int64(otherRows)},
		Notes:    []string{"error: " + errStr(otherErr)},
	})
	t.Logf("行レベル: 許可ok=%v / robot拒否err=%v(rows=%d) / 別主体拒否err=%v(rows=%d)",
		okErr == nil, denyErr, denyRows, otherErr, otherRows)

	// ---- 検証 ----
	if okErr != nil || okResp.SendCommand.ID == "" {
		t.Errorf("grant のある操作が失敗した: %v", okErr)
	}
	if denyErr == nil || denyRows != 0 {
		t.Errorf("grant 無しの robot が拒否されていない: err=%v rows=%d", denyErr, denyRows)
	}
	if otherErr == nil || otherRows != 0 {
		t.Errorf("別主体が拒否されていない: err=%v rows=%d", otherErr, otherRows)
	}

	rec.Scope(
		"MySQL 8.0 / gqlgen v0.17 / robot_operator (tenant_id, operator, robot_id) が grant",
		"主体（operator）は context（X-User→principal）から。引数から取らない",
		"行レベルは対象×主体に依るのでリゾルバで判定（@auth ディレクティブでは表せない）",
	)
	rec.Uncertain(
		"ここは『操作(mutation)』の行レベル認可。読み取り側の行フィルタは別途（一覧に混ぜない）",
		"grant の管理（誰がいつ付与・失効するか）は本実験外",
		"ADMIN のバイパス等のポリシーは要件次第。ここでは grant 必須で統一",
	)
	rec.Artifact(
		"internal/gql: canOperate（robot_operator による行レベル認可）と sendCommand リゾルバ",
		"docs/graphql.md: 行レベル認可（対象×主体）",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"行レベル認可は『この主体がこの対象を操作してよいか』を、対象と主体を見てリゾルバで判定する。" +
			"フィールド単位の @auth では表せない（行に依る）。主体は context から取り、拒否時は書き込まない。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
