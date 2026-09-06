package fanoutlab_test

// EXP-16: 担当テナント数（IN サイズ）の上限。
//
//	MYSQL_DSN=... go test ./internal/fanoutlab/ -run TestEXP16 -v
//
// EXP-14 で fan-out を1クエリに畳んだ。担当テナント（IN の要素）が増えると、
// その1クエリ自体が重くなる。どこから重くなるか、畳む vs 分けるの分岐点を測る。

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

func TestEXP16_担当テナント数の上限(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-16", "owned-tenant-limit",
		"担当テナント数（IN サイズ）の上限: 畳んだ1クエリはどこから重くなるか")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 担当テナントを IN で畳んだ1クエリの所要は、IN の要素数（担当テナント数）とともに増える。",
		"2) テナント 30 程度なら1クエリは十分速い（畳んで良い）。",
		"3) IN が非常に大きい（数百〜千）と1クエリが重くなり、分けた方が速くなる領域が現れる。",
	}, " "))

	// 最大 1000 テナントぶんの命令を1件ずつ入れておく（due 済み）
	const maxT = 1000
	tenants := make([]string, maxT)
	for i := range tenants {
		tenants[i] = fmt.Sprintf("lim-t%04d", i)
	}
	if err := fanoutlab.SeedTenants(ctx, db, tenants, 1, 0, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	rec.Workload("max_tenants", maxT).Workload("samples_per_size", 50)

	sizes := []int{10, 30, 100, 300, 1000}
	var latMs []float64
	for _, sz := range sizes {
		st, err := fanoutlab.MeasureFoldQuery(ctx, db, tenants[:sz], 500, 50)
		if err != nil {
			t.Fatal(err)
		}
		latMs = append(latMs, msf(st.Latency.P95))
		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("IN サイズ = %d 担当テナント", sz),
			Counters: map[string]int64{"rows": st.Rows},
			Metrics:  map[string]float64{"p50_ms": msf(st.Latency.P50), "p95_ms": msf(st.Latency.P95), "p99_ms": msf(st.Latency.P99)},
		})
		t.Logf("IN=%-4d rows=%d p50=%v p95=%v", sz, st.Rows, st.Latency.P50, st.Latency.P95)
	}

	// 畳む vs 分ける: 300 テナントを 1クエリ IN(300) vs 3クエリ IN(100)×3
	one, err := fanoutlab.MeasureFoldQuery(ctx, db, tenants[:300], 500, 50)
	if err != nil {
		t.Fatal(err)
	}
	var splitTotal time.Duration
	for c := 0; c < 3; c++ {
		st, err := fanoutlab.MeasureFoldQuery(ctx, db, tenants[c*100:(c+1)*100], 500, 50)
		if err != nil {
			t.Fatal(err)
		}
		splitTotal += st.Latency.P50
	}
	rec.Add(expkit.Variant{
		Name: "畳む vs 分ける（300 テナント）",
		Metrics: map[string]float64{
			"one_query_in300_p50_ms":        msf(one.Latency.P50),
			"three_queries_in100_合計_p50_ms": msf(splitTotal),
		},
		Notes: []string{
			fmt.Sprintf("IN(300) 1クエリ p50=%v / IN(100)×3 合計 p50=%v", one.Latency.P50, splitTotal),
			"分けると往復が3回になる。IN が重くなる前は1クエリが有利、重くなると分割が効く",
		},
	})
	t.Logf("畳む IN(300) p50=%v / 分ける IN(100)x3 合計 p50=%v", one.Latency.P50, splitTotal)

	// ---- 検証 ----
	// IN サイズとともに p95 が増える傾向（10 < 1000）
	if latMs[0] >= latMs[len(latMs)-1] {
		t.Errorf("IN サイズに対して所要が単調でない: IN10=%.3fms IN1000=%.3fms", latMs[0], latMs[len(latMs)-1])
	}

	rec.Scope(
		"MySQL 8.0 / cmd_command の poll 索引 (tenant_id,state,scheduled_for) / 各サイズ 50 サンプル",
		"1テナント1命令（due 済み）。IN の要素数だけを変えて1クエリの所要を測る",
	)
	rec.Uncertain(
		"最適な上限は行数・索引・DB の忙しさで動く。この数字はこのホストのもの",
		"IN が数千を超えるとプランナやパケットサイズの別の効き方が出る（未測定）",
		"接続予算（poolbudget）との兼ね合いで、担当テナント数はコンテナ数とも連動する",
	)
	rec.Artifact("internal/fanoutlab: MeasureFoldQuery（IN サイズごとの1クエリ所要）")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"担当テナントを IN で畳んだ1クエリの所要は IN サイズとともに増える。",
		"テナント 30 程度なら1クエリで十分速い（畳んで良い）。",
		"IN が非常に大きくなると、コンテナあたりの担当テナント数に上限を設けて分割する方が良い領域が出る。",
		"担当テナント数の上限は、この所要と接続予算（poolbudget）の両方から決める。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
