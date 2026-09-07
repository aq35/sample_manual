package splitlab_test

// EXP-19: メモ列の縦分割（一覧の行に大きなメモを同居させるか、別表へ分けるか）。
//
//	MYSQL_DSN=... go test ./internal/splitlab/ -run TestEXP19 -v

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/splitlab"
)

func TestEXP19_メモ列の縦分割(t *testing.T) {
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
	if err := splitlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-19", "column-split",
		"メモ列（失敗理由）を一覧行に同居させるか別表に分けるか")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 同居のコストは『走査する行数 × 行の太さ』。1ページ(LIMIT 100)だけなら走査が少ないので差は小さい。",
		"2) テナント全体を走査する集計では、太い行(メモ同居)の clustered index はページ数が多く、",
		"   狭い表より明確に遅い（メモを取らなくても）。",
		"3) メモが要る画面では、分割版は『一覧＋id群のメモをバッチ取得』（DataLoader 相当）で足りる",
		"   （往復は増えるが1ページぶんだけ）。",
	}, " "))

	const tenant = "sp-main"
	const rows = 200_000
	const limit = 100
	if err := splitlab.Seed(ctx, db, tenant, rows); err != nil {
		t.Fatal(err)
	}
	rec.Workload("rows", rows).Workload("page_limit", limit).Workload("detail_bytes", "約3KB")

	// ① 1ページ（LIMIT 100）: 走査が少ないので差は小さいはず
	inline := splitlab.ListInline(ctx, db, tenant, limit, 30)
	split := splitlab.ListSplit(ctx, db, tenant, limit, 30)
	rec.Add(expkit.Variant{
		Name:    "1ページ一覧(LIMIT 100・メモ取らない): 同居 vs 分割",
		Metrics: map[string]float64{"inline_p50_ms": ms(inline.P50), "split_p50_ms": ms(split.P50)},
		Notes:   []string{"同居 " + dur(inline.P50) + " → 分割 " + dur(split.P50) + "（走査が少ないので差は小さい）"},
	})
	t.Logf("1ページ: 同居 p50=%v / 分割 p50=%v", inline.P50, split.P50)

	// ② テナント全体を走査する集計: 太い行が効く
	scanInline := splitlab.ScanInline(ctx, db, tenant, 20)
	scanSplit := splitlab.ScanSplit(ctx, db, tenant, 20)
	ratio := ms(scanInline.P50) / ms(scanSplit.P50)
	rec.Add(expkit.Variant{
		Name:     "全体走査の集計(COUNT・メモ取らない): 同居版（太い行）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(scanInline.P50)},
	})
	rec.Add(expkit.Variant{
		Name:    "全体走査の集計(COUNT・メモ取らない): 分割版（狭い表）",
		Metrics: map[string]float64{"p50_ms": ms(scanSplit.P50)},
		Notes:   []string{"同居 " + dur(scanInline.P50) + " → 分割 " + dur(scanSplit.P50) + "（約 " + ratioStr(ratio) + " 倍。太い clustered index はページ数が多い）"},
	})
	t.Logf("全体走査: 同居 p50=%v / 分割 p50=%v（%.1f倍）", scanInline.P50, scanSplit.P50, ratio)

	// メモが要る画面
	withInline := splitlab.PageWithDetailInline(ctx, db, tenant, limit, 30)
	pageThenDetail := splitlab.PageThenDetailSplit(ctx, db, tenant, limit, 30)
	rec.Add(expkit.Variant{
		Name:    "メモが要る画面: 同居版（1クエリでメモも取る）",
		Metrics: map[string]float64{"p50_ms": ms(withInline.P50)},
	})
	rec.Add(expkit.Variant{
		Name:    "メモが要る画面: 分割版（一覧＋id群のメモをバッチ取得, DataLoader 相当）",
		Metrics: map[string]float64{"p50_ms": ms(pageThenDetail.P50)},
		Notes:   []string{"メモが要るときは分割版でも " + dur(pageThenDetail.P50) + " で足りる（2クエリでも1ページぶんだけ）"},
	})
	t.Logf("メモ要: 同居1クエリ p50=%v / 分割(一覧+バッチ) p50=%v", withInline.P50, pageThenDetail.P50)

	// ---- 検証 ----
	// 全体走査では分割（狭い表）が明確に速い
	if scanSplit.P50 >= scanInline.P50 {
		t.Errorf("全体走査で分割が同居より速くない: 分割 %v 同居 %v", scanSplit.P50, scanInline.P50)
	}

	rec.Scope(
		"MySQL 8.0 / 20万行 / 1ページ100件 / メモ約3KB",
		"一覧は status で絞って id 順に LIMIT。メモ列は VARCHAR(2000)",
		"分割版の一覧は狭い表、メモは別表（同じ (tenant_id, id)）",
	)
	rec.Uncertain(
		"絶対値はバッファプールに載った状態のもの。ディスクから読む状況では差が広がりうる",
		"メモのサイズ・1ページ件数で差は動く。TEXT/BLOB ではさらに差が出る（別ページ格納）",
		"分割は書き込みが2表になる（一覧行とメモを両方入れる）。整合はトランザクションで取る",
	)
	rec.Artifact("internal/splitlab: 同居/分割の一覧・詳細取得（DataLoader 相当のバッチ取得）")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"失敗理由のような大きなメモを一覧行に同居させると、メモを取らない一覧でも行が太って遅くなる。",
		"メモを別表に分ければ一覧は狭い行だけを読むので速く、メモが要る画面では",
		"『一覧＋id群のメモをバッチ取得』（GraphQL の DataLoader 相当）で足りる。",
		"→ GraphQL でも、要求スカラーは投影し、メモ/理由は別表＋別リゾルバのバッチ取得にする。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d interface{ Microseconds() int64 }) float64 { return float64(d.Microseconds()) / 1000.0 }
func dur(d interface{ String() string }) string      { return d.String() }

func ratioStr(r float64) string { return fmt.Sprintf("%.1f", r) }
