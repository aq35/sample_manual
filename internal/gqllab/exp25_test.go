package gqllab_test

// EXP-25: gqlgen の認可（テナント内のロール）。@auth ディレクティブでフィールド単位に守る。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP25 -v
//
// テナント分離（EXP-24）は「どのテナントのデータか」。認可は「そのテナント内で、この主体は
// 何を見てよいか」。ここでは serial（機微）を ADMIN だけに見せる。ロールも主体から決める
// （引数から取らない）。@auth は解決の前に走るので、拒否時は DB にも触れない。

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

// roleHeader は「認証済みロール」を X-Role から読む（実運用は JWT のクレーム等）。
func roleHeader(r *http.Request) (gql.Role, bool) {
	switch r.Header.Get("X-Role") {
	case "ADMIN":
		return gql.RoleAdmin, true
	case "OPERATOR":
		return gql.RoleOperator, true
	case "VIEWER":
		return gql.RoleViewer, true
	default:
		return "", false
	}
}

func newAuthClient(db *repo.DB, cfg gql.ServerConfig) *client.Client {
	h := gql.MiddlewareWithRole(db, tenantHeader, roleHeader, true, gql.NewServer(db, cfg))
	return client.New(h)
}

func TestEXP25_gqlgen認可(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.Seed(ctx, db, tenantA, 5, 2); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-25", "gqlgen-authorization",
		"@auth ディレクティブでフィールド単位の認可（テナント内ロール）")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) serial は @auth(requires: ADMIN)。ADMIN は取得でき、VIEWER/OPERATOR は拒否され null＋エラー。 " +
			"2) ロールも主体（context）から決める。GraphQL 引数に role を置かない（詐称不能）。 " +
			"3) @auth は解決関数の前に走るので、拒否時は serial 用の DB クエリを発行しない。 " +
			"4) 拒否は serial だけを null にし、id/name など他フィールドは返る（nullable フィールド）。")

	cli := newAuthClient(db, gql.DefaultServerConfig())
	const q = `query($id:ID!){ robot(id:$id){ id name serial } }`
	type robotResp struct {
		Robot *struct {
			ID     string
			Name   string
			Serial *string
		}
	}

	// ADMIN: serial が見える。DB は getRobot + name + serial。
	before := db.Stats().Queries
	var admin robotResp
	if err := cli.Post(q, &admin, client.Var("id", "r0000"),
		client.AddHeader("X-Tenant", tenantA), client.AddHeader("X-Role", "ADMIN")); err != nil {
		t.Fatalf("admin: %v", err)
	}
	adminQueries := db.Stats().Queries - before
	adminSerial := admin.Robot != nil && admin.Robot.Serial != nil

	// VIEWER: serial は拒否（null）。id/name は返る。serial の DB は引かない。
	before = db.Stats().Queries
	var viewer robotResp
	errViewer := cli.Post(q, &viewer, client.Var("id", "r0000"),
		client.AddHeader("X-Tenant", tenantA), client.AddHeader("X-Role", "VIEWER"))
	viewerQueries := db.Stats().Queries - before
	viewerSerialNull := viewer.Robot != nil && viewer.Robot.Serial == nil
	viewerNameOK := viewer.Robot != nil && viewer.Robot.Name != ""

	// OPERATOR: これも serial は拒否（ADMIN 未満）。
	var op robotResp
	errOp := cli.Post(q, &op, client.Var("id", "r0000"),
		client.AddHeader("X-Tenant", tenantA), client.AddHeader("X-Role", "OPERATOR"))
	opSerialNull := op.Robot != nil && op.Robot.Serial == nil

	rec.Add(expkit.Variant{
		Name:     "ADMIN: serial が見える",
		Counters: map[string]int64{"serial_visible": b2i(adminSerial), "db_queries": adminQueries},
		Notes:    []string{"getRobot + name + serial を引く"},
	})
	rec.Add(expkit.Variant{
		Name:     "VIEWER: serial 拒否（null＋エラー）・id/name は返る・serial の DB は引かない",
		Accident: true,
		Counters: map[string]int64{"serial_null": b2i(viewerSerialNull), "name_ok": b2i(viewerNameOK), "db_queries": viewerQueries},
		Notes:    []string{"error: " + errStr(errViewer)},
	})
	rec.Add(expkit.Variant{
		Name:     "OPERATOR: serial 拒否（ADMIN 未満）",
		Accident: true,
		Counters: map[string]int64{"serial_null": b2i(opSerialNull)},
		Notes:    []string{"error: " + errStr(errOp)},
	})
	t.Logf("ADMIN serial可視=%v(q=%d) / VIEWER serial=null:%v name:%v(q=%d) / OPERATOR serial=null:%v",
		adminSerial, adminQueries, viewerSerialNull, viewerNameOK, viewerQueries, opSerialNull)

	// ---- 検証 ----
	if !adminSerial {
		t.Errorf("ADMIN で serial が見えない")
	}
	if !viewerSerialNull || errViewer == nil {
		t.Errorf("VIEWER で serial が拒否されていない: null=%v err=%v", viewerSerialNull, errViewer)
	}
	if !viewerNameOK {
		t.Errorf("VIEWER で name まで巻き添えで消えた（serial だけ null のはず）")
	}
	if !opSerialNull || errOp == nil {
		t.Errorf("OPERATOR で serial が拒否されていない")
	}
	if viewerQueries >= adminQueries {
		t.Errorf("拒否時に serial の DB を引いている: VIEWER=%d ADMIN=%d", viewerQueries, adminQueries)
	}

	rec.Scope(
		"MySQL 8.0 / gqlgen v0.17 / serial=@auth(requires: ADMIN) / ロールは X-Role（実運用は JWT）",
		"ロール強さ VIEWER<OPERATOR<ADMIN。@auth は不足なら next を呼ばず拒否",
		"serial は nullable。拒否時は serial だけ null になり他フィールドは返る",
	)
	rec.Uncertain(
		"ここはフィールド単位のロール認可のみ。行単位（この robot を操作してよいか）は別途",
		"ロールの出所（JWT 検証・失効）は本実験外。ここは検証済みロールが context にある前提",
	)
	rec.Artifact(
		"internal/gql: @auth ディレクティブ（authz.go）とロール context",
		"docs/graphql.md: 認可（フィールド単位ロール）",
	)
	rec.Next("EXP-26 永続化クエリ allowlist・レート制限")

	files, err := rec.Save(
		"認可はフィールド単位に @auth(requires:) で宣言し、解決の前にロールを検査する（拒否時は DB に触れない）。" +
			"ロールもテナントと同様に主体から決め、GraphQL 引数には置かない。nullable にすれば拒否は該当フィールドだけを null にする。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
