package fanoutlab_test

// EXP-14: ポーリングの fan-out を畳む（テナント分離を保ったまま）。
//
//	MYSQL_DSN=... go test ./internal/fanoutlab/ -run TestEXP14 -v
//
// 多数テナントを担当する1コンテナで、テナントごとに引く（fan-out）と、
// 担当テナント集合を1クエリに畳む、を比べる。
// ★畳んでもテナント越えの処理が起きない（誤ルーティング 0）ことを同時に確かめる。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/cadencelab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/fanoutlab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP14_fanoutを畳む(t *testing.T) {
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
	if err := cadencelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-14", "fanout-fold",
		"ポーリングの fan-out を畳む: テナントごと vs 1クエリ、分離を保ったまま")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) テナントごとにポーリングすると、問い合わせ回数が テナント数×ラウンド になる。",
		"2) 担当テナント集合を1クエリに畳むと、問い合わせは ラウンド数 になり、テナント数に比例しない。",
		"3) 畳んでも、取得後にテナント別へ切り分けて処理すれば、誤ルーティング（テナント越え処理）は 0。",
		"4) 畳む問い合わせは担当テナントに限定した IN であり、全テナント無条件ではない。",
	}, " "))

	const nTenants = 100
	const perTenant = 5
	tenants := make([]string, nTenants)
	for i := range tenants {
		tenants[i] = fmt.Sprintf("fo-t%03d", i)
	}
	window := 500 * time.Millisecond
	rec.Workload("tenants", nTenants).Workload("per_tenant", perTenant).
		Workload("total_commands", nTenants*perTenant).Workload("window", window.String())

	// ---- ① テナントごとにポーリング（fan-out）----
	if err := fanoutlab.SeedTenants(ctx, db, tenants, perTenant, window, time.Now()); err != nil {
		t.Fatal(err)
	}
	per, err := fanoutlab.PerTenantPoll(ctx, db, tenants, 50, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "テナントごとにポーリング（fan-out）",
		Desc:     "テナント数 × ラウンド の問い合わせ",
		Counters: map[string]int64{"poll_queries": per.PollQueries, "rounds": per.Rounds, "dispatched": per.Dispatched, "misrouted": per.Misrouted},
		Metrics:  map[string]float64{"latency_p95_ms": msf(per.Latency.P95)},
	})
	t.Logf("per-tenant  queries=%d rounds=%d dispatched=%d misrouted=%d p95=%v",
		per.PollQueries, per.Rounds, per.Dispatched, per.Misrouted, per.Latency.P95)

	// ---- ② 1クエリに畳む（担当テナントに限定）----
	if err := fanoutlab.SeedTenants(ctx, db, tenants, perTenant, window, time.Now()); err != nil {
		t.Fatal(err)
	}
	fold, err := fanoutlab.FoldedPoll(ctx, db, tenants, 50, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "1クエリに畳む（担当テナント限定 IN + テナント別に切り分け）",
		Desc:     "ラウンドごとに1クエリ。取得後 tenant_id で bucket に分け、テナント別ワーカーで処理",
		Counters: map[string]int64{"poll_queries": fold.PollQueries, "rounds": fold.Rounds, "dispatched": fold.Dispatched, "misrouted": fold.Misrouted},
		Metrics:  map[string]float64{"latency_p95_ms": msf(fold.Latency.P95)},
		Notes: []string{
			fmt.Sprintf("問い合わせ %d → %d（テナント %d 件でも増えない）", per.PollQueries, fold.PollQueries, nTenants),
			"誤ルーティング 0 = テナント越えの処理は起きていない",
		},
	})
	t.Logf("folded      queries=%d rounds=%d dispatched=%d misrouted=%d p95=%v",
		fold.PollQueries, fold.Rounds, fold.Dispatched, fold.Misrouted, fold.Latency.P95)

	// ---- ③ 全テナント無条件は禁止（空担当でエラー）----
	if _, err := fanoutlab.FoldedPoll(ctx, db, nil, 50, 0); err == nil {
		t.Error("担当テナント空での畳み込みは拒否されるべき（全テナント無条件の防止）")
	} else {
		rec.Add(expkit.Variant{
			Name:  "担当テナント空の畳み込みは拒否（全テナント無条件の防止）",
			Notes: []string{"エラー: " + err.Error()},
		})
		t.Logf("空担当は拒否: %v", err)
	}

	// ---- 検証 ----
	if per.Dispatched != nTenants*perTenant || fold.Dispatched != nTenants*perTenant {
		t.Errorf("取りこぼし: per=%d fold=%d want %d", per.Dispatched, fold.Dispatched, nTenants*perTenant)
	}
	// ★分離: どちらも誤ルーティング 0
	if per.Misrouted != 0 || fold.Misrouted != 0 {
		t.Errorf("テナント越えの処理が起きた: per=%d fold=%d（0 のはず）", per.Misrouted, fold.Misrouted)
	}
	// fan-out が畳めている: 畳んだ方は問い合わせが桁違いに少ない
	if fold.PollQueries >= per.PollQueries {
		t.Errorf("畳んでも問い合わせが減っていない: per=%d fold=%d", per.PollQueries, fold.PollQueries)
	}
	// 具体的に: per はおよそ tenants × rounds、fold は rounds
	if per.PollQueries < int64(nTenants) {
		t.Errorf("per-tenant の問い合わせが少なすぎる（テナント数以上のはず）: %d", per.PollQueries)
	}

	rec.Scope(
		"MySQL 8.0 / 1コンテナが 100 テナントを担当 / 命令 500 件を 0.5 秒に散らす",
		"問い合わせ回数 = ポーリングの SELECT のみ（claim の UPDATE は両者同数なので除く）",
		"誤ルーティング = 別テナントのワーカーへ渡った行の数。0 が分離健全",
	)
	rec.Uncertain(
		"畳んだ IN のサイズが大きいと1クエリ自体が重くなる（担当テナント数の上限は別途）",
		"担当テナント集合は lease から得る前提。lease の実際の結線は本実験外",
		"複数コンテナが別々のテナント集合を担当する構成での相互作用は未測定",
	)
	rec.Artifact(
		"internal/fanoutlab: 担当テナント限定の畳み込みポーリングとテナント別切り分け",
		"誤ルーティング検査つき（畳んでも分離が保たれることをテストで担保）",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"テナントごとのポーリングは問い合わせがテナント数に比例する。",
		"担当テナントに限定した IN で1クエリに畳めば、テナント数に依らず問い合わせは一定。",
		"取得後に tenant_id で切り分けてテナント別ワーカーで処理すれば、",
		"『跨いで取って各ワーカーで処理』の怖さ（テナント越え処理）は誤ルーティング 0 で防げる。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

var _ = sql.ErrNoRows
