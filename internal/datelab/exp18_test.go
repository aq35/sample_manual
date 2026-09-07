package datelab_test

// EXP-18: 日付範囲検索は何件で重くなるか。
//
//	MYSQL_DSN=... go test ./internal/datelab/ -run TestEXP18 -v

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/datelab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP18_日付範囲検索(t *testing.T) {
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
	if err := datelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-18", "date-range-search",
		"日付範囲検索は何件で重くなるか。総行数ではなく走査行数で決まる")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) (tenant_id, observed_date) 索引があれば、日付範囲はテナント局所の range scan。",
		"   重さは『テーブルの総行数』ではなく『範囲がヒットして走査/返す行数』で決まる。",
		"2) 同じ日付幅なら、総行数を 100K→1M に増やしても所要はほぼ変わらない。",
		"3) 索引だけで完結（covering, COUNT や索引列）なら速い。payload を取ると本体行へ",
		"   ランダムに引きに行くので、返す行数が増えるほど重くなる。",
		"4) 範囲がテーブルの大部分を占めると、オプティマイザが full scan(type=ALL) に切り替える。",
	}, " "))

	const days = 365
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tenant := "ds-main"

	// ---- A) 総行数を変えて、同じ「1日」検索の所要を見る（総行数に依らないはず）----
	for _, total := range []int{100_000, 1_000_000} {
		if err := datelab.Seed(ctx, db, tenant, total, days, base, 2, 50_000); err != nil {
			t.Fatal(err)
		}
		// 1日 = 総行数/365 件くらいがヒット
		from := base.AddDate(0, 0, 100)
		to := from.AddDate(0, 0, 1)
		lat, matched, plan, err := datelab.Measure(ctx, db, tenant, from, to, true, 30)
		if err != nil {
			t.Fatal(err)
		}
		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("総行数 %d / 1日検索（covering COUNT）", total),
			Counters: map[string]int64{"matched": matched, "explain_rows": plan.Rows},
			Metrics:  map[string]float64{"p50_ms": ms(lat.P50), "p95_ms": ms(lat.P95)},
			Notes:    []string{fmt.Sprintf("EXPLAIN type=%s key=%s rows=%d %s", plan.Type, plan.Key, plan.Rows, plan.Extra)},
		})
		t.Logf("総行数=%-8d 1日: matched=%d p50=%v type=%s rows=%d", total, matched, lat.P50, plan.Type, plan.Rows)
	}

	// ここからは 1M 行で範囲幅・covering・full scan を見る
	const total = 1_000_000
	if err := datelab.Seed(ctx, db, tenant, total, days, base, 2, 50_000); err != nil {
		t.Fatal(err)
	}

	// ---- B) 範囲幅を変える（走査行数が増えると重くなる）----
	type rangeCase struct {
		name string
		dur  int // 日数
	}
	var covLat []float64
	for _, rc := range []rangeCase{{"1日", 1}, {"7日", 7}, {"30日", 30}, {"180日", 180}} {
		from := base.AddDate(0, 0, 100)
		to := from.AddDate(0, 0, rc.dur)
		lat, matched, plan, err := datelab.Measure(ctx, db, tenant, from, to, true, 20)
		if err != nil {
			t.Fatal(err)
		}
		covLat = append(covLat, ms(lat.P50))
		rec.Add(expkit.Variant{
			Name:     "範囲 " + rc.name + "（covering COUNT / 1M行）",
			Counters: map[string]int64{"matched": matched, "explain_rows": plan.Rows},
			Metrics:  map[string]float64{"p50_ms": ms(lat.P50), "p95_ms": ms(lat.P95)},
			Notes:    []string{fmt.Sprintf("type=%s rows=%d %s", plan.Type, plan.Rows, plan.Extra)},
			Accident: plan.Type == "ALL",
		})
		t.Logf("範囲=%-5s matched=%d p50=%v type=%s rows=%d %s", rc.name, matched, lat.P50, plan.Type, plan.Rows, plan.Extra)
	}

	// ---- C) covering vs 本体行へ引きに行く（payload）----
	from := base.AddDate(0, 0, 100)
	to := from.AddDate(0, 0, 30) // 30日
	covLatS, covMatched, _, err := datelab.Measure(ctx, db, tenant, from, to, true, 20)
	if err != nil {
		t.Fatal(err)
	}
	payLat, payMatched, payPlan, err := datelab.Measure(ctx, db, tenant, from, to, false, 20)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "30日: covering(COUNT) vs payload取得（本体行ランダムアクセス）",
		Counters: map[string]int64{"matched": covMatched, "matched_payload": payMatched},
		Metrics:  map[string]float64{"covering_p50_ms": ms(covLatS.P50), "payload_p50_ms": ms(payLat.P50)},
		Notes: []string{
			fmt.Sprintf("covering %v → payload取得 %v（%d行の本体アクセス）", covLatS.P50, payLat.P50, payMatched),
			"payload は索引に無い → 行ごとに本体（clustered index）へ引きに行く。返す行数が効く",
			"EXPLAIN(payload): type=" + payPlan.Type + " " + payPlan.Extra,
		},
	})
	t.Logf("30日 covering p50=%v / payload p50=%v (matched=%d)", covLatS.P50, payLat.P50, payMatched)

	// ---- D) 全期間（テーブルの大部分）→ full scan に切り替わるか ----
	full := base.AddDate(1, 0, 0)
	_, fmatched, fplan, err := datelab.Measure(ctx, db, tenant, base, full, true, 5)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "全期間（365日 ≒ 全行）",
		Counters: map[string]int64{"matched": fmatched, "explain_rows": fplan.Rows},
		Notes:    []string{fmt.Sprintf("EXPLAIN type=%s key=%q rows=%d %s", fplan.Type, fplan.Key, fplan.Rows, fplan.Extra), "範囲が広すぎると索引を使わず full scan になりうる"},
	})
	t.Logf("全期間: matched=%d type=%s key=%q rows=%d", fmatched, fplan.Type, fplan.Key, fplan.Rows)

	// ---- 検証 ----
	// B: 範囲が広いほど遅い（1日 < 180日）
	if covLat[0] >= covLat[len(covLat)-1] {
		t.Errorf("範囲幅と所要が単調でない: 1日=%.3fms 180日=%.3fms", covLat[0], covLat[len(covLat)-1])
	}

	rec.Scope(
		"MySQL 8.0 / date_search(tenant_id,id) PK + (tenant_id,observed_date,id) 索引",
		"1テナントを 365 日に均等散布 + 別テナントの noise 10万行（テナント局所性の確認）",
		"covering = COUNT/索引列のみ。payload取得 = 本体行へのランダムアクセスを含む",
	)
	rec.Uncertain(
		"絶対値はこのホスト・バッファプールに載った状態のもの。ディスクから読む状況は別",
		"full scan への切り替え閾値はオプティマイザの推定次第（データ分布で動く。EXP-7 参照）",
		"OFFSET 深いページ・複合条件（status との AND）は本実験では別途未測定",
	)
	rec.Artifact("internal/datelab: 日付範囲検索の所要 vs 総行数/範囲幅/covering/full scan")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"日付範囲検索の重さは、テーブルの総行数ではなく『範囲がヒットして走査/返す行数』で決まる。",
		"(tenant_id, observed_date) 索引があれば、同じ日付幅なら 100K でも 1M でも所要はほぼ同じ。",
		"重くなるのは、(a) 範囲が広くヒット行数が多い、(b) payload など索引外の列を取り本体行へ",
		"ランダムアクセスする、(c) 範囲がテーブルの大部分で full scan に切り替わる、のいずれか。",
		"対策: 索引に tenant_id を先頭で含める／必要な列を索引に載せて covering にする／",
		"範囲を絞る・keyset でページングする／巨大な追記表は日付パーティション（EXP-15）。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
