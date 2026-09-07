package gqllab_test

// EXP-27: mutation の冪等性と入力検証（sendCommand）。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP27 -v
//
// 書き込みは「二重発行」と「不正入力」を必ず塞ぐ。冪等キー（uq_idem）で再送を吸収し、
// 入力は DB に触れる前に検証してクライアント起因のエラーで返す（内部 500 にしない）。

import (
	"context"
	"net/http"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/99designs/gqlgen/client"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/gqllab"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/repo"
)

// newMutationClient は tenant / role / principal(X-User) を載せるクライアント。
func newMutationClient(db *repo.DB, cfg gql.ServerConfig) *client.Client {
	base := gql.MiddlewareWithRole(db, tenantHeader, roleHeader, true, gql.NewServer(db, cfg))
	withPrincipal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User"); u != "" {
			r = r.WithContext(gql.WithPrincipal(r.Context(), u))
		}
		base.ServeHTTP(w, r)
	})
	return client.New(withPrincipal)
}

const mutQuery = `mutation($in: SendCommandInput!){ sendCommand(input:$in){ id type state } }`

type sendResp struct {
	SendCommand struct {
		ID    string
		Type  string
		State string
	}
}

func sendVars(robotID, typ, idem string) client.Option {
	return client.Var("in", map[string]any{
		"robotId": robotID, "type": typ, "idempotencyKey": idem,
	})
}

func TestEXP27_mutation冪等性と入力検証(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.Seed(ctx, db, tenantA, 5, 0); err != nil {
		t.Fatal(err)
	}
	const op = "u-op"
	if err := gqllab.GrantOperator(ctx, db, tenantA, op, "r0000"); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-27", "gqlgen-mutation-idempotency",
		"sendCommand の冪等性（uq_idem）と入力検証")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) 同じ idempotencyKey の再送は、新しい行を作らず既存の命令を返す（DB 上も1行）。 " +
			"2) 違う idempotencyKey は別の命令になる。 " +
			"3) 不正入力（未知の type・空の冪等キー・長すぎる payload）は DB に触れる前に拒否し、行は作らない。 " +
			"4) 検証エラーはクライアント起因として返す（内部 500 にしない）。")

	cli := newMutationClient(db, gql.DefaultServerConfig())
	hdr := func() []client.Option {
		return []client.Option{
			client.AddHeader("X-Tenant", tenantA),
			client.AddHeader("X-Role", "OPERATOR"),
			client.AddHeader("X-User", op),
		}
	}
	post := func(robotID, typ, idem string) (sendResp, error) {
		var r sendResp
		opts := append(hdr(), sendVars(robotID, typ, idem))
		err := cli.Post(mutQuery, &r, opts...)
		return r, err
	}

	// ① 初回発行
	const k1 = "idem-k1"
	r1, err1 := post("r0000", "move", k1)
	if err1 != nil {
		t.Fatalf("初回 sendCommand: %v", err1)
	}
	// ② 同じ冪等キーで再送 → 同じ命令 ID・DB 上は1行
	r2, err2 := post("r0000", "move", k1)
	cnt1, _ := gqllab.CountCommandsByIdem(ctx, db, tenantA, k1)
	rec.Add(expkit.Variant{
		Name:     "冪等: 同じ idempotencyKey の再送は同じ命令・DB 1行",
		Counters: map[string]int64{"same_id": b2i(err2 == nil && r1.SendCommand.ID == r2.SendCommand.ID), "rows": int64(cnt1)},
		Notes:    []string{"初回 " + r1.SendCommand.ID + " / 再送 " + r2.SendCommand.ID},
	})
	t.Logf("冪等: id1=%s id2=%s rows=%d", r1.SendCommand.ID, r2.SendCommand.ID, cnt1)

	// ③ 違う冪等キー → 別命令
	const k2 = "idem-k2"
	r3, err3 := post("r0000", "stop", k2)
	rec.Add(expkit.Variant{
		Name:     "違う idempotencyKey は別命令",
		Counters: map[string]int64{"different_id": b2i(err3 == nil && r3.SendCommand.ID != r1.SendCommand.ID)},
	})

	// ④ 入力検証（DB に触れず拒否・行を作らない）
	_, eType := post("r0000", "explode", "idem-bad-type") // 未知の type
	_, eIdem := post("r0000", "move", "")                 // 空の冪等キー
	badTypeRows, _ := gqllab.CountCommandsByIdem(ctx, db, tenantA, "idem-bad-type")
	rec.Add(expkit.Variant{
		Name:     "入力検証: 未知の type は拒否・行を作らない",
		Accident: true,
		Counters: map[string]int64{"rejected": b2i(eType != nil), "rows": int64(badTypeRows)},
		Notes:    []string{"error: " + errStr(eType)},
	})
	rec.Add(expkit.Variant{
		Name:     "入力検証: 空の idempotencyKey は拒否",
		Accident: true,
		Notes:    []string{"error: " + errStr(eIdem)},
	})
	t.Logf("検証: type err=%v / idem err=%v / bad-type rows=%d", eType, eIdem, badTypeRows)

	// ---- 検証 ----
	if err2 != nil || r1.SendCommand.ID != r2.SendCommand.ID {
		t.Errorf("冪等でない: id1=%s id2=%s err=%v", r1.SendCommand.ID, r2.SendCommand.ID, err2)
	}
	if cnt1 != 1 {
		t.Errorf("同じ冪等キーで %d 行できた（1 のはず）", cnt1)
	}
	if err3 != nil || r3.SendCommand.ID == r1.SendCommand.ID {
		t.Errorf("違う冪等キーが別命令になっていない: %s vs %s", r3.SendCommand.ID, r1.SendCommand.ID)
	}
	if eType == nil || eIdem == nil {
		t.Errorf("不正入力が拒否されていない: type=%v idem=%v", eType, eIdem)
	}
	if badTypeRows != 0 {
		t.Errorf("不正入力で行ができた: %d", badTypeRows)
	}

	rec.Scope(
		"MySQL 8.0 / gqlgen v0.17 / cmd_command.uq_idem (tenant_id, idem_key)",
		"許可 type=move/stop/charge/reset・payload<=255・idempotencyKey 必須",
		"冪等は『既存を探して返す／無ければ INSERT、競合時は既存を返す』",
	)
	rec.Uncertain(
		"競合時のフォールバック（ErrConflict→再取得）は同時再送の一方のみ検証。厳密な並行試験は別",
		"入力検証はサーバ側の最小限。スキーマ制約（enum 化等）でさらに前段に寄せられる",
	)
	rec.Artifact(
		"internal/gql: sendCommand（validateSendCommand・冪等発行）",
		"docs/graphql.md: mutation の冪等性・入力検証",
	)
	rec.Next("EXP-28 行レベル認可")

	files, err := rec.Save(
		"書き込みは冪等キー（uq_idem）で再送を吸収し、同じキーは新しい行を作らず既存を返す。" +
			"入力は DB に触れる前に検証し、不正はクライアント起因エラーで返して行を作らない。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
