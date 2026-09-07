package gqllab_test

// EXP-23: gqlgen のパフォーマンス。N+1 と DataLoader、射影、ページ上限。
//
//	MYSQL_DSN=... go test ./internal/gqllab/ -run TestEXP23 -v
//
// GraphQL はクライアントが「何を・どうネストして」取るか決める。素朴に書くと、一覧の各要素で
// 子フィールドを解決するたびに DB を引く（N+1）。DataLoader で1クエリに畳む。件数×クエリ数を測る。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"
	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/gql"
	"github.com/aq35/sample_manual/internal/gqllab"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/repo"
)

const tenantA = "t-gql-a"

func openDB(t *testing.T) *repo.DB {
	t.Helper()
	dsn := mysqltest.DSN(t)
	db, err := repo.Open(dsn, repo.Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxRows: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(context.Background()); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	return db
}

// tenantHeader は「認証済みテナント」を X-Tenant ヘッダから読む（実運用は JWT 等に差し替え）。
func tenantHeader(r *http.Request) (model.TenantID, bool) {
	t := r.Header.Get("X-Tenant")
	return model.TenantID(t), t != ""
}

func newClient(db *repo.DB, cfg gql.ServerConfig, withLoaders bool) *client.Client {
	h := gql.Middleware(db, tenantHeader, withLoaders, gql.NewServer(db, cfg))
	return client.New(h)
}

func TestEXP23_gqlgenパフォーマンス(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := gqllab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	const robots, cmdsPer = 150, 10 // 150台: ページ上限(100)が実際に効くことを見るため
	if err := gqllab.Seed(ctx, db, tenantA, robots, cmdsPer); err != nil {
		t.Fatal(err)
	}
	const listN = 50 // 一覧で取る台数（N+1 の N）

	rec := expkit.NewRecorder("EXP-23", "gqlgen-performance",
		"gqlgen の N+1 と DataLoader・射影・ページ上限")
	rec.Env(expkit.CaptureEnv(ctx, db.SQL()))
	rec.Freeze(
		"1) 一覧の各要素で子（commands）を解決すると、DataLoader が無ければ N+1（robots 1 + 台数ぶん）。 " +
			"2) DataLoader を入れると、子の取得は robot_id をまとめた1クエリに畳まれる（robots 1 + commands 1）。 " +
			"3) 射影: name（別表 robot_profile）を要求しなければ、その解決関数は呼ばれず profile を引かない。 " +
			"4) robots(first) はサーバ側で上限が掛かる（10万件要求しても MaxPageSize で頭打ち）。")
	rec.Workload("robots", robots).Workload("cmds_per_robot", cmdsPer)

	// クエリ: 一覧 + 各ロボットの commands（name は取らない＝射影）
	const q = `{ robots(first: 50) { robots { id commands(first: 5) { id } } } }`
	type resp struct {
		Robots struct {
			Robots []struct {
				ID       string
				Commands []struct{ ID string }
			}
		}
	}

	// ① N+1（ローダ無し）
	perfCfg := gql.ServerConfig{ComplexityLimit: 0, Introspection: false, MaxPageSize: 100}
	naive := newClient(db, perfCfg, false)
	before := db.Stats().Queries
	t0 := time.Now()
	var r1 resp
	if err := naive.Post(q, &r1, client.AddHeader("X-Tenant", tenantA)); err != nil {
		t.Fatalf("naive query: %v", err)
	}
	n1Queries := db.Stats().Queries - before
	n1Latency := time.Since(t0)

	// ② DataLoader（ローダ有り）
	batched := newClient(db, perfCfg, true)
	before = db.Stats().Queries
	t0 = time.Now()
	var r2 resp
	if err := batched.Post(q, &r2, client.AddHeader("X-Tenant", tenantA)); err != nil {
		t.Fatalf("batched query: %v", err)
	}
	dlQueries := db.Stats().Queries - before
	dlLatency := time.Since(t0)

	rec.Add(expkit.Variant{
		Name:     "N+1: ローダ無し（各ロボットで commands を1クエリずつ）",
		Accident: true,
		Counters: map[string]int64{"db_queries": n1Queries},
		Metrics:  map[string]float64{"latency_ms": msf(n1Latency)},
		Notes:    []string{"robots 1 + 各台の commands = 1 + 台数ぶんのクエリ"},
	})
	rec.Add(expkit.Variant{
		Name:     "DataLoader: robot_id をまとめて1クエリ",
		Counters: map[string]int64{"db_queries": dlQueries},
		Metrics:  map[string]float64{"latency_ms": msf(dlLatency)},
		Notes:    []string{"ローダ無し " + itoa(n1Queries) + " クエリ → 有り " + itoa(dlQueries) + " クエリ"},
	})
	t.Logf("N+1=%d クエリ (%v) / DataLoader=%d クエリ (%v)", n1Queries, n1Latency, dlQueries, dlLatency)

	// ③ 射影: name を要求しない vs する（commands は取らない）
	const qNoName = `{ robots(first: 50) { robots { id } } }`
	const qWithName = `{ robots(first: 50) { robots { id name } } }`
	before = db.Stats().Queries
	var rn resp
	_ = naive.Post(qNoName, &rn, client.AddHeader("X-Tenant", tenantA))
	noName := db.Stats().Queries - before
	before = db.Stats().Queries
	var rw resp
	_ = naive.Post(qWithName, &rw, client.AddHeader("X-Tenant", tenantA))
	withName := db.Stats().Queries - before
	rec.Add(expkit.Variant{
		Name:     "射影: name を要求しない（profile を引かない）",
		Counters: map[string]int64{"db_queries": noName},
	})
	rec.Add(expkit.Variant{
		Name:     "射影: name を要求する（別表 robot_profile を引く）",
		Counters: map[string]int64{"db_queries": withName},
		Notes:    []string{"name 無し " + itoa(noName) + " クエリ → 有り " + itoa(withName) + " クエリ（要求時だけ別表を引く）"},
	})
	t.Logf("射影: name無し=%d / name有り=%d クエリ", noName, withName)

	// ④ ページ上限: 10万件要求しても MaxPageSize(100) で頭打ち
	const qBig = `{ robots(first: 100000) { robots { id } hasNext } }`
	var rb struct {
		Robots struct {
			Robots  []struct{ ID string }
			HasNext bool
		}
	}
	if err := naive.Post(qBig, &rb, client.AddHeader("X-Tenant", tenantA)); err != nil {
		t.Fatalf("big page query: %v", err)
	}
	rec.Add(expkit.Variant{
		Name:     "ページ上限: first=100000 要求 → MaxPageSize で頭打ち",
		Counters: map[string]int64{"returned": int64(len(rb.Robots.Robots))},
		Notes:    []string{"要求 10万 → 返却 " + itoa(int64(len(rb.Robots.Robots))) + "（無制限一覧を作らせない）"},
	})
	t.Logf("ページ上限: 要求10万 → 返却 %d", len(rb.Robots.Robots))

	// ---- 検証 ----
	if len(r1.Robots.Robots) != listN || len(r2.Robots.Robots) != listN {
		t.Fatalf("robots 件数が違う: naive=%d batched=%d (期待 %d)", len(r1.Robots.Robots), len(r2.Robots.Robots), listN)
	}
	if dlQueries >= n1Queries {
		t.Errorf("DataLoader がクエリを減らしていない: N+1=%d DL=%d", n1Queries, dlQueries)
	}
	if dlQueries > 3 { // robots 1 + commands 1（多少の揺れを許容）
		t.Errorf("DataLoader なのにクエリが多い: %d", dlQueries)
	}
	if withName <= noName {
		t.Errorf("name 要求で profile を引いていない（射影が効いていない）: 無=%d 有=%d", noName, withName)
	}
	if len(rb.Robots.Robots) != 100 {
		t.Errorf("ページ上限(100)どおりに返っていない: %d 件（150台シード・first=10万要求）", len(rb.Robots.Robots))
	}

	rec.Scope(
		"MySQL 8.0 / 1テナント 50台・各10命令 / gqlgen v0.17 / DataLoader=dataloadgen",
		"クエリ数は repo.Stats().Queries の差分（SELECT の回数）",
		"commands は cmd_command を robot_id で引く（by_robot 索引）",
	)
	rec.Uncertain(
		"絶対レイテンシはローカルのもの。ネットワーク往復や N が増えると N+1 の差はさらに開く",
		"DataLoader の待ち窓（WithWait）や並行度で畳まれ方は動く",
		"name はここでは素朴解決。実務では name も別ローダで畳める",
	)
	rec.Artifact(
		"internal/gql: gqlgen サーバ（リゾルバ・DataLoader・複雑度・射影・ページ上限）",
		"docs/graphql.md: gqlgen ベストプラクティス（パフォーマンス）",
	)
	rec.Next("EXP-24 セキュリティ（テナント分離・複雑度 DoS）")

	files, err := rec.Save(
		"一覧の子フィールド（commands）は DataLoader で畳む（N+1 → 1）。name のような別表は要求時だけ引く（射影）。" +
			"robots(first) はサーバ側で必ず上限を掛け、無制限一覧を作らせない。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
func itoa(n int64) string         { return strconv.FormatInt(n, 10) }
