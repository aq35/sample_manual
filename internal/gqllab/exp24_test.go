package gqllab_test

// EXP-24: gqlgen のセキュリティ。テナント分離・複雑度 DoS・内観・エラー秘匿。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP24 -v
//
// GraphQL は「1エンドポイントに何でも投げられる」。守りの要は:
//   - テナントは主体から決める（引数から取らない）。他テナントの id は見えない。
//   - 深い/広いクエリは複雑度で実行前に弾く（DoS）。
//   - 本番は内観オフ。内部エラーは client に漏らさない。

import (
	"context"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/99designs/gqlgen/client"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/gqllab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

const tenantB = "t-gql-b"

func TestEXP24_gqlgenセキュリティ(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	// A と B に同じ robot_id を入れる（衝突しても跨げないことを見る）。B だけの id も1つ。
	if err := gqllab.Seed(ctx, db, tenantA, 20, 5); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.Seed(ctx, db, tenantB, 20, 5); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.SeedOne(ctx, db, tenantB, "bonly-1"); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-24", "gqlgen-security",
		"gqlgen のテナント分離・複雑度 DoS・内観・エラー秘匿")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) テナントは主体から決める（context）。スキーマに tenant 引数が無く、詐称できない。 " +
			"2) 他テナントだけの id を問い合わせても、:tenant で弾かれ null（存在も漏らさない）。 " +
			"3) 複雑度上限を超える深い/広いクエリは、DB に触れる前に弾かれる（クエリ数 0）。 " +
			"4) 本番は内観オフ（__schema は拒否）。内部エラーは一般化して client に漏らさない。")

	prod := gql.ServerConfig{ComplexityLimit: 200, Introspection: false, MaxPageSize: 100}

	// ① テナント分離: A として B だけの id を引く → null（跨げない）
	cliA := newClient(db, prod, true)
	var rBonly struct {
		Robot *struct{ ID string }
	}
	if err := cliA.Post(`query($id:ID!){ robot(id:$id){ id } }`, &rBonly,
		client.Var("id", "bonly-1"), client.AddHeader("X-Tenant", tenantA)); err != nil {
		t.Fatalf("robot(bonly) as A: %v", err)
	}
	crossLeak := rBonly.Robot != nil
	// A として A に在る id は見える（対照）
	var rOwn struct {
		Robot *struct{ ID string }
	}
	_ = cliA.Post(`query($id:ID!){ robot(id:$id){ id } }`, &rOwn,
		client.Var("id", "r0000"), client.AddHeader("X-Tenant", tenantA))
	rec.Add(expkit.Variant{
		Name:     "テナント分離: A から B だけの id を引く → null",
		Accident: crossLeak,
		Counters: map[string]int64{"leaked": b2i(crossLeak), "own_visible": b2i(rOwn.Robot != nil)},
		Notes:    []string{"他テナントの id は :tenant で弾かれ、存在も漏れない"},
	})
	t.Logf("テナント分離: B専用id漏れ=%v / 自テナントid可視=%v", crossLeak, rOwn.Robot != nil)

	// ② 認証なし（テナント無し）→ unauthenticated（黙って全件見せない）
	var rNoAuth struct {
		Robots *struct{ Robots []struct{ ID string } }
	}
	errNoAuth := cliA.Post(`{ robots(first:5){ robots{ id } } }`, &rNoAuth) // ヘッダ無し
	rec.Add(expkit.Variant{
		Name:  "認証なし → unauthenticated（テナント未設定では何も返さない）",
		Notes: []string{"error: " + errStr(errNoAuth)},
	})
	t.Logf("認証なし: err=%v", errNoAuth)

	// ③ 複雑度 DoS: 深い/広いクエリは DB に触れる前に弾かれる
	const heavy = `{ robots(first:100){ robots{ id commands(first:100){ id type state } } } }`
	before := db.Stats().Queries
	var rHeavy struct{}
	errHeavy := cliA.Post(heavy, &rHeavy, client.AddHeader("X-Tenant", tenantA))
	heavyQueries := db.Stats().Queries - before
	rec.Add(expkit.Variant{
		Name:     "複雑度 DoS: 上限超クエリは実行前に拒否（DB クエリ 0）",
		Counters: map[string]int64{"db_queries": heavyQueries},
		Notes:    []string{"error: " + errStr(errHeavy) + " / DB クエリ=" + itoa(heavyQueries)},
	})
	t.Logf("複雑度: err=%v / DBクエリ=%d", errHeavy, heavyQueries)

	// ④ 内観: 本番(オフ)は拒否 / 開発(オン)は通る
	introQ := `{ __schema { queryType { name } } }`
	var ri struct{}
	errIntroOff := cliA.Post(introQ, &ri, client.AddHeader("X-Tenant", tenantA))
	devCli := newClient(db, gql.ServerConfig{ComplexityLimit: 200, Introspection: true, MaxPageSize: 100}, true)
	var ri2 struct {
		Schema struct{ QueryType struct{ Name string } } `json:"__schema"`
	}
	errIntroOn := devCli.Post(introQ, &ri2, client.AddHeader("X-Tenant", tenantA))
	rec.Add(expkit.Variant{
		Name:  "内観: 本番オフは拒否 / 開発オンは通る",
		Notes: []string{"オフ: " + errStr(errIntroOff) + " / オン: queryType=" + ri2.Schema.QueryType.Name},
	})
	t.Logf("内観: オフerr=%v / オンname=%q", errIntroOff, ri2.Schema.QueryType.Name)

	// ⑤ エラー秘匿: 認証なしのメッセージが内部語（ErrNoTenant の生文）でなく一般化されている
	masked := errNoAuth != nil && strings.Contains(errNoAuth.Error(), "unauthenticated") &&
		!strings.Contains(errNoAuth.Error(), "認証ミドルウェア")
	rec.Add(expkit.Variant{
		Name:     "エラー秘匿: 内部エラー文を client に出さない",
		Counters: map[string]int64{"masked": b2i(masked)},
		Notes:    []string{"client には unauthenticated だけ。内部の理由文は出さない"},
	})

	// ---- 検証 ----
	if crossLeak {
		t.Errorf("テナント分離が破れた: A から B 専用の id が見えた")
	}
	if rOwn.Robot == nil {
		t.Errorf("自テナントの id が見えない（対照が壊れている）")
	}
	if errNoAuth == nil {
		t.Errorf("認証なしでも通ってしまった（テナント無しは拒否のはず）")
	}
	if errHeavy == nil {
		t.Errorf("複雑度上限を超えたクエリが通ってしまった")
	}
	if heavyQueries != 0 {
		t.Errorf("複雑度で拒否されたのに DB を引いた: %d クエリ", heavyQueries)
	}
	if errIntroOff == nil {
		t.Errorf("内観オフなのに __schema が通った")
	}
	if errIntroOn != nil || ri2.Schema.QueryType.Name == "" {
		t.Errorf("内観オンで __schema が通らない: err=%v name=%q", errIntroOn, ri2.Schema.QueryType.Name)
	}
	if !masked {
		t.Errorf("エラーが秘匿されていない: %v", errNoAuth)
	}

	rec.Scope(
		"MySQL 8.0 / gqlgen v0.17 / 複雑度上限=200 / 内観=本番オフ",
		"テナントは X-Tenant（実運用は JWT 等）→ context。スキーマに tenant 引数は無い",
		"複雑度は commands/robots に first 比例で計上（server.go）",
	)
	rec.Uncertain(
		"複雑度の適正値は本番のクエリ形状で決める。ここは 200 の例",
		"認可（このテナント内で誰が何を見られるか）は本実験外。ここはテナント境界のみ",
		"永続化クエリ（allowlist）でさらに攻撃面を狭められる（本実験外）",
	)
	rec.Artifact(
		"internal/gql: テナント束縛 Scope・複雑度上限・内観トグル・エラー秘匿",
		"docs/graphql.md: gqlgen ベストプラクティス（セキュリティ）",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"テナントは主体から決め（引数から取らない）、全 DB アクセスをテナント束縛 Scope に通す。" +
			"深い/広いクエリは複雑度上限で実行前に弾き、本番は内観オフ・内部エラーは秘匿する。")
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
func errStr(e error) string {
	if e == nil {
		return "(なし)"
	}
	return e.Error()
}
