package gqllab_test

// EXP-26: gqlgen の受付制御。永続化クエリ allowlist と、テナント単位のレート制限。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP26 -v
//
// 複雑度上限（EXP-24）が「1クエリの重さ」を抑えるのに対し、ここは「そもそも受け付けるか」。
//   - allowlist: 登録済みクエリ以外は実行しない（自由なクエリを投げる余地を消す）。
//   - レート制限: 1テナントの暴走が他テナントを巻き込まないよう、単位時間の本数を抑える。

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/99designs/gqlgen/client"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/gqllab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP26_gqlgen受付制御(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := gqllab.Seed(ctx, db, tenantA, 5, 2); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-26", "gqlgen-admission-control",
		"永続化クエリ allowlist とテナント単位のレート制限")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) allowlist: 事前登録したクエリだけ実行できる。1文字でも違えば（無害でも）拒否される。 " +
			"2) レート制限: テナントごとのトークンバケツ。burst を超えると拒否。時間が経てば補充されて再び通る。 " +
			"3) レート制限はテナント単位。あるテナントが枯らしても、別テナントは自分のバケツで通る。")

	// ---- ① 永続化クエリ allowlist ----
	const allowedQ = `{ robots(first: 3) { robots { id } } }`
	const otherQ = `{ robots(first: 3) { robots { id name } } }` // 無害だが未登録
	allow := gql.NewAllowList(allowedQ)
	cfgAllow := gql.ServerConfig{ComplexityLimit: 0, MaxPageSize: 100, AllowList: allow}
	cliAllow := newClient(db, cfgAllow, false)

	var okResp struct {
		Robots struct{ Robots []struct{ ID string } }
	}
	errAllowed := cliAllow.Post(allowedQ, &okResp, client.AddHeader("X-Tenant", tenantA))
	var otherResp struct{}
	errOther := cliAllow.Post(otherQ, &otherResp, client.AddHeader("X-Tenant", tenantA))

	rec.Add(expkit.Variant{
		Name:     "allowlist: 登録済みクエリは通る",
		Counters: map[string]int64{"ok": b2i(errAllowed == nil), "returned": int64(len(okResp.Robots.Robots))},
	})
	rec.Add(expkit.Variant{
		Name:     "allowlist: 未登録クエリは拒否（無害でも）",
		Accident: true,
		Notes:    []string{"error: " + errStr(errOther)},
	})
	t.Logf("allowlist: 登録済み err=%v / 未登録 err=%v", errAllowed, errOther)

	// ---- ② レート制限（決定的な時計で）----
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := gql.NewRateLimiterWithClock(1, 3, clk.now) // 毎秒1本・バースト3本
	cfgRate := gql.ServerConfig{ComplexityLimit: 0, MaxPageSize: 100, RateLimiter: rl}
	cliRate := newClient(db, cfgRate, false)
	rlQ := `{ robots(first: 1) { robots { id } } }`

	type rlResp struct {
		Robots struct{ Robots []struct{ ID string } }
	}
	post := func() bool { // true=通った
		var r rlResp
		return cliRate.Post(rlQ, &r, client.AddHeader("X-Tenant", tenantA)) == nil
	}

	// burst=3: 最初の3本は通り、4本目は拒否（時計は進めない）
	var passed, rejected int
	for i := 0; i < 4; i++ {
		if post() {
			passed++
		} else {
			rejected++
		}
	}
	// 2秒進める → 2本ぶん補充される
	clk.advance(2 * time.Second)
	refillPassed := 0
	for i := 0; i < 3; i++ {
		if post() {
			refillPassed++
		}
	}
	// 別テナントは自分のバケツ（枯れていない）で通る
	var rb rlResp
	otherTenantOK := cliRate.Post(rlQ, &rb, client.AddHeader("X-Tenant", tenantB)) == nil

	rec.Add(expkit.Variant{
		Name:     "レート制限: burst=3 → 3本通り4本目拒否",
		Counters: map[string]int64{"passed": int64(passed), "rejected": int64(rejected)},
	})
	rec.Add(expkit.Variant{
		Name:     "レート制限: 2秒後に補充 → 2本通る / 別テナントは自分のバケツで通る",
		Counters: map[string]int64{"refill_passed": int64(refillPassed), "other_tenant_ok": b2i(otherTenantOK)},
	})
	t.Logf("レート制限: burst passed=%d rejected=%d / 補充後 passed=%d / 別テナント=%v",
		passed, rejected, refillPassed, otherTenantOK)

	// ---- 検証 ----
	if errAllowed != nil {
		t.Errorf("登録済みクエリが拒否された: %v", errAllowed)
	}
	if errOther == nil {
		t.Errorf("未登録クエリが通ってしまった（allowlist が効いていない）")
	}
	if passed != 3 || rejected != 1 {
		t.Errorf("burst 制限が想定と違う: passed=%d rejected=%d（3/1 のはず）", passed, rejected)
	}
	if refillPassed != 2 {
		t.Errorf("2秒補充で通る本数が想定と違う: %d（2 のはず）", refillPassed)
	}
	if !otherTenantOK {
		t.Errorf("別テナントまで巻き込んで制限した（テナント単位のはず）")
	}

	rec.Scope(
		"MySQL 8.0 / gqlgen v0.17 / allowlist=生クエリの sha256 / レート=毎秒1本・burst3・決定的時計",
		"allowlist は OperationContext.RawQuery のハッシュで判定（パース前段）",
		"レート制限は InterceptOperation でテナントごとにトークンバケツ",
	)
	rec.Uncertain(
		"allowlist はビルド時に既知のクエリを登録する運用前提（クライアントのクエリは有限個）",
		"レートの適正値は本番のトラフィックで決める。ここは 1/s・burst3 の例",
		"分散環境ではバケツを共有ストア（Redis 等）に置く必要がある。本実験はプロセス内",
	)
	rec.Artifact(
		"internal/gql: allowlist.go（永続化クエリ）・ratelimit.go（テナント単位トークンバケツ）",
		"docs/graphql.md: 受付制御（allowlist・レート制限）",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"複雑度上限に加え、受付段でも締める。永続化クエリ allowlist で未登録クエリを実行させず、" +
			"テナント単位のレート制限で単位時間の本数を抑える（1テナントの暴走を他テナントに波及させない）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

// fakeClock は決定的なレート制限テスト用の時計。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
