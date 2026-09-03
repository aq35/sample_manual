package planlab_test

// EXP-7: 実行計画とデータの偏り。
//
//	MYSQL_DSN=... go test ./internal/planlab/ -run TestEXP7 -v -timeout 20m
//
// 少量・均等なテストデータで計画を固定しても、本番の計画とは別物になる。
// 件数と偏りを変えて、同じ問い合わせがどう変わるかを測る。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/planlab"
)

func TestEXP7_実行計画とデータの偏り(t *testing.T) {
	mysqltest.Serialize(t)
	db := mysqltest.Raw(t)
	ctx := context.Background()

	rec := expkit.NewRecorder("EXP-7", "query-plan-skew",
		"件数と偏りを変えたときの実行計画・走査行数・遅延")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 同じ問い合わせでも、行数が変われば選ばれる索引と Extra（filesort など）が変わる。",
		"2) OFFSET は深くなるほど走査行数が増える。キーセット法は増えない。",
		"3) 選択性の低い列（status）での絞り込みは、索引があっても走査行数が減らない。",
		"4) 1テナントが 90% を占めると、そのテナントの一覧は他テナントより遅くなる。",
		"5) N+1 は、まとめて引くより桁で遅い。",
	}, " "))

	datasets := []planlab.Dataset{
		{Name: "50行・均等", Tenants: 5, Rows: 50, HotKeys: 3},
		{Name: "5,000行・均等", Tenants: 5, Rows: 5000, HotKeys: 3},
		{Name: "100,000行・均等", Tenants: 5, Rows: 100000, HotKeys: 3},
		{Name: "100,000行・1テナントが90%", Tenants: 5, Rows: 100000, Skew: true, HotKeys: 3},
	}
	rec.Workload("datasets", []string{datasets[0].Name, datasets[1].Name, datasets[2].Name, datasets[3].Name}).
		Workload("indexes", "PRIMARY(tenant_id,id) / idx_tenant_created / idx_tenant_status").
		Injection("none", "故障注入なし。データの形だけを変える")

	for _, ds := range datasets {
		if err := planlab.Build(ctx, db, ds); err != nil {
			t.Fatal(err)
		}
		counts, err := planlab.Counts(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		big := "t-000"
		perTenant := counts[big]
		t.Logf("=== %s（%s は %d 行） ===", ds.Name, big, perTenant)

		deep := perTenant / 2
		if deep < 1 {
			deep = 1
		}
		queries := []planlab.Query{
			{Name: "一覧: キーセット法", SQL: `SELECT id, status, payload FROM plan_item
				WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT 50`, Args: []any{big, deep}},
			{Name: "一覧: OFFSET", SQL: `SELECT id, status, payload FROM plan_item
				WHERE tenant_id = ? ORDER BY id LIMIT 50 OFFSET ?`, Args: []any{big, deep}},
			{Name: "一覧: 索引だけで足りる", SQL: `SELECT id FROM plan_item
				WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT 50`, Args: []any{big, deep}},
			{Name: "絞り込み: status（選択性が低い）", SQL: `SELECT id, payload FROM plan_item
				WHERE tenant_id = ? AND status = 1 ORDER BY id LIMIT 50`, Args: []any{big}},
			{Name: "絞り込み: 作成日時の範囲", SQL: `SELECT id, payload FROM plan_item
				WHERE tenant_id = ? AND created_at >= ? ORDER BY created_at LIMIT 50`,
				Args: []any{big, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}},
			{Name: "1件: 主キー", SQL: `SELECT status, payload FROM plan_item
				WHERE tenant_id = ? AND id = ?`, Args: []any{big, deep}},
		}

		var notes []string
		for _, q := range queries {
			m, err := planlab.Measure(ctx, db, q)
			if err != nil {
				t.Fatalf("[%s/%s] %v", ds.Name, q.Name, err)
			}
			notes = append(notes, fmt.Sprintf(
				"%-28s type=%-6s key=%-18s 見積 %8d 行 / 実際に読んだ %8d 行 / p50 %-9v p99 %-9v %s",
				m.Name, m.PlanType, orDash(m.PlanKey), m.PlanRows,
				m.HandlerReadNext+m.HandlerReadKey+m.HandlerReadRnd,
				m.Latency.P50.Round(time.Microsecond), m.Latency.P99.Round(time.Microsecond),
				m.PlanExtra))
			t.Logf("  %-28s type=%-6s key=%-18s 見積 %8d / 実読 %8d / p50 %-9v %s",
				m.Name, m.PlanType, orDash(m.PlanKey), m.PlanRows,
				m.HandlerReadNext+m.HandlerReadKey+m.HandlerReadRnd,
				m.Latency.P50.Round(time.Microsecond), m.PlanExtra)
		}

		// N+1 とまとめ引きの比較
		ids := make([]int, 0, 200)
		for i := 0; i < 200 && i < perTenant; i++ {
			ids = append(ids, i)
		}
		n1, gotN1, err := planlab.NPlusOne(ctx, db, big, ids)
		if err != nil {
			t.Fatal(err)
		}
		inq, gotIn, err := planlab.InClause(ctx, db, big, ids)
		if err != nil {
			t.Fatal(err)
		}
		ratio := float64(n1) / float64(inq)
		notes = append(notes, fmt.Sprintf(
			"%d 件の取得: 1件ずつ %v / まとめて %v（%.0f 倍）", len(ids), n1.Round(time.Microsecond), inq.Round(time.Microsecond), ratio))
		t.Logf("  %d 件: 1件ずつ %v / まとめて %v（%.0f 倍）", len(ids),
			n1.Round(time.Microsecond), inq.Round(time.Microsecond), ratio)
		if gotN1 != gotIn {
			t.Errorf("取得件数が違う: %d vs %d", gotN1, gotIn)
		}

		rec.Add(expkit.Variant{
			Name: ds.Name,
			Desc: fmt.Sprintf("%s の行数: %d（全 %d 行）", big, perTenant, ds.Rows),
			Counters: map[string]int64{
				"rows_total":         int64(ds.Rows),
				"rows_in_big_tenant": int64(perTenant),
			},
			Metrics: map[string]float64{"n_plus_one_ratio": ratio},
			Notes:   notes,
		})
	}

	// ---- 偏りの影響: 大きいテナント vs 小さいテナント（同じ問い合わせ） ----
	skewed := planlab.Dataset{Name: "100,000行・1テナントが90%", Tenants: 5, Rows: 100000, Skew: true, HotKeys: 3}
	if err := planlab.Build(ctx, db, skewed); err != nil {
		t.Fatal(err)
	}
	counts, err := planlab.Counts(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var skewNotes []string
	var bigP50, smallP50 time.Duration
	for _, tenant := range []string{"t-000", "t-001"} {
		q := planlab.Query{
			Name: "status での絞り込み",
			SQL: `SELECT id, payload FROM plan_item
				  WHERE tenant_id = ? AND status = 1 ORDER BY id LIMIT 50`,
			Args: []any{tenant},
		}
		m, err := planlab.Measure(ctx, db, q)
		if err != nil {
			t.Fatal(err)
		}
		if tenant == "t-000" {
			bigP50 = m.Latency.P50
		} else {
			smallP50 = m.Latency.P50
		}
		skewNotes = append(skewNotes, fmt.Sprintf(
			"%s（%d 行）: type=%s key=%s 見積 %d 行 / 実読 %d 行 / p50 %v",
			tenant, counts[tenant], m.PlanType, orDash(m.PlanKey), m.PlanRows,
			m.HandlerReadNext+m.HandlerReadKey+m.HandlerReadRnd, m.Latency.P50.Round(time.Microsecond)))
		t.Logf("[偏り] %s（%d 行）type=%s key=%s 実読 %d 行 p50 %v",
			tenant, counts[tenant], m.PlanType, orDash(m.PlanKey),
			m.HandlerReadNext+m.HandlerReadKey+m.HandlerReadRnd, m.Latency.P50.Round(time.Microsecond))
	}
	rec.Add(expkit.Variant{
		Name: "同じ問い合わせ・大きいテナント vs 小さいテナント",
		Desc: "テナントごとに行数が違えば、同じ SQL でも選ばれる索引・走査行数・遅延が変わる",
		Notes: append(skewNotes,
			"★仮説4（大きいテナントのほうが遅くなる）は **外れた**。",
			"　小さいテナントのほうが index_merge を選ばれ、テナント全行を読んで遅くなった。",
			"　オプティマイザはテナントごとの選択性の見積もりで判断するので、"+
				"『行数が少ない = 速い』とは限らない。",
			"　『平均の応答時間』はこの差を隠す。テナント別に見る必要がある"),
		Metrics: map[string]float64{
			"big_over_small_p50": float64(bigP50) / float64(smallP50),
		},
		Accident: false,
	})

	rec.Scope(
		"MySQL 8.0 / 同一ホスト / 1表・3索引（主キー + 作成日時 + status）",
		"遅延はバッファプールが温まった状態での値。1回目（初回アクセス）は別に記録している",
		"1件あたり payload 180 バイト",
	)
	rec.Uncertain(
		"**本当のコールドキャッシュは未検証**（バッファプールを空にするには再起動か縮小が要る）。"+
			"ここでの『初回』は、直前の投入でページが載っている可能性がある",
		"索引の追加・削除が既存の計画に与える影響は EXP-6 の 0002/0003 で部分的に見ただけ",
		"結合（JOIN）を含む問い合わせは測っていない",
		"統計情報の更新タイミング（ANALYZE TABLE の有無）による揺れは1回しか確認していない",
	)
	rec.Artifact(
		"internal/planlab: 件数・偏りを変えてデータを作り、EXPLAIN と Handler_* の差分（実際に読んだ行数）と"+
			"遅延を1つの接続で測る道具",
		"repotest.RequireIndexed と組み合わせると、『本番に近い件数で計画を固定する』テストが書ける",
	)
	rec.Next("EXP-8 SQL guard の fuzz")

	files, err := rec.Save(strings.Join([]string{
		"同じ問い合わせでも、行数が変われば選ばれる索引も Extra も変わる",
		"（5,000行では idx_tenant_created + filesort、100,000行では PRIMARY の range）。",
		"OFFSET は深さに比例して走査行数が増え（11 → 1,001 → 10,050 → 45,050 行）、キーセット法は 50 行のまま。",
		"N+1 は 200 件で 32〜37 倍遅い。",
		"★仮説4は外れた: 偏りがあるとき遅くなったのは **小さいほうのテナント** で、",
		"index_merge を選ばれてテナント全行を読んでいた。『行数が少ない = 速い』は成り立たない。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ = sql.ErrNoRows
