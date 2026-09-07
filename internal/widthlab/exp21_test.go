package widthlab_test

// EXP-21: 太い列は VARCHAR か TEXT か、で重さが決まるのではない。
//         「値が行の中にあるか（inline）、行の外にあるか（off-page）」で決まる。
//
//	MYSQL_DSN=... go test ./internal/widthlab/ -run TestEXP21 -v
//
// InnoDB(DYNAMIC) は、値が行に収まればその型が TEXT でも inline に置く。収まらないほど
// 大きいと本体を行の外へ追い出す。だから:
//   - 3KB は VARCHAR でも TEXT でも inline → 行が太る → 舐めが重い（TEXT にしても軽くならない）
//   - 24KB の TEXT は off-page → 行は細い → 舐めは narrow 並みに軽い（が SELECT * すると重い）

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/widthlab"
)

func TestEXP21_VARCHARとTEXTの重さ(t *testing.T) {
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
	if err := widthlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-21", "varchar-vs-text",
		"太い列の重さは型名でなく inline/off-page で決まる・SELECT * の効き")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 3KB の memo は VARCHAR でも TEXT でも行に収まり inline に置かれる。だから memo を取らない",
		"   全体走査でも narrow より大幅に重い。『TEXT にしただけ』では軽くならない。",
		"2) 24KB の TEXT は行に収まらず off-page になる。行(clustered index)は細いので、memo を取らない",
		"   走査は narrow 並みに軽い。→『軽さ』は型名でなくサイズ（inline か off-page か）で決まる。",
		"3) その大きい TEXT を SELECT *（本体込み）で読むと、1行ごとに外へ本体を取りに行き非常に重い。",
		"4) 別表に分ければサイズによらず舐める表は narrow 並みに軽い（EXP-19）。確実なのは分割。",
	}, " "))

	const tenant = "w-main"
	const rows = 30_000
	const rangeN = 5_000 // 範囲読みで舐める行数
	if err := widthlab.Seed(ctx, db, tenant, rows); err != nil {
		t.Fatal(err)
	}
	rec.Workload("rows", rows).Workload("memo_small", "約3KB(inline)").
		Workload("memo_big", "約24KB(off-page)").Workload("range_rows", rangeN)

	// ① 全体走査（memo 取らない）: varchar(3K) / text_small(3K) / text_big(24K) / narrow
	cv := widthlab.CountScan(ctx, db, "w_varchar", tenant, 20)
	cts := widthlab.CountScan(ctx, db, "w_text_small", tenant, 20)
	ctb := widthlab.CountScan(ctx, db, "w_text_big", tenant, 20)
	cn := widthlab.CountScan(ctx, db, "w_narrow", tenant, 20)
	rec.Add(expkit.Variant{
		Name:     "全体走査(COUNT・memo取らない): VARCHAR 3KB（inline・太い）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(cv.P50)},
	})
	rec.Add(expkit.Variant{
		Name:     "全体走査(COUNT・memo取らない): TEXT 3KB（これも inline・太い）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(cts.P50)},
		Notes:    []string{"VARCHAR 3KB " + dur(cv.P50) + " / TEXT 3KB " + dur(cts.P50) + "（どちらも inline で narrow より大幅に重い。TEXT にしても軽くならない）"},
	})
	rec.Add(expkit.Variant{
		Name:    "全体走査(COUNT・memo取らない): TEXT 24KB（off-page・行は細い）",
		Metrics: map[string]float64{"p50_ms": ms(ctb.P50)},
		Notes:   []string{"TEXT 3KB(inline) " + dur(cts.P50) + " → TEXT 24KB(off-page) " + dur(ctb.P50) + "（大きいほど外へ出て、舐めは軽い）"},
	})
	rec.Add(expkit.Variant{
		Name:    "全体走査(COUNT・memo取らない): narrow（memo 無し・基準）",
		Metrics: map[string]float64{"p50_ms": ms(cn.P50)},
	})
	t.Logf("全体走査: VARCHAR3K=%v / TEXT3K=%v / TEXT24K=%v / narrow=%v", cv.P50, cts.P50, ctb.P50, cn.P50)

	// ② 大きい TEXT（off-page）を SELECT * で読むと重い: SELECT * の罠
	bNo := widthlab.RangeReadNoMemo(ctx, db, "w_text_big", tenant, rangeN, 20)
	bWith := widthlab.RangeReadWithMemo(ctx, db, "w_text_big", tenant, rangeN, 20)
	rec.Add(expkit.Variant{
		Name:    "範囲読み TEXT 24KB: memo 取らない（SELECT id,status）",
		Metrics: map[string]float64{"p50_ms": ms(bNo.P50)},
		Notes:   []string{"off-page なので本体に触らず軽い"},
	})
	rec.Add(expkit.Variant{
		Name:     "範囲読み TEXT 24KB: memo も取る（SELECT * 相当）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(bWith.P50)},
		Notes:    []string{"取らない " + dur(bNo.P50) + " → 取る " + dur(bWith.P50) + "（1行ごとに 24KB を外へ取りに行く）"},
	})
	t.Logf("TEXT 24KB 範囲読み: memo無し=%v / memo有り(SELECT*)=%v", bNo.P50, bWith.P50)

	// ---- 検証 ----
	// (1) 3KB は VARCHAR でも TEXT でも inline: どちらも narrow より大幅に重い（TEXT で軽くならない）
	if cts.P50 <= cn.P50*3 {
		t.Errorf("TEXT 3KB が narrow 並みに軽い（inline なら重いはず）: TEXT3K=%v narrow=%v", cts.P50, cn.P50)
	}
	// (2) 24KB の TEXT は off-page: 3KB の inline TEXT より全体走査が軽い（型は同じ TEXT・差はサイズ）
	if ctb.P50 >= cts.P50 {
		t.Errorf("TEXT 24KB(off-page) が TEXT 3KB(inline) より軽くない: 24K=%v 3K=%v", ctb.P50, cts.P50)
	}
	// (3) off-page の TEXT を SELECT * で読むと、取らないより重い
	if bWith.P50 <= bNo.P50 {
		t.Errorf("TEXT 24KB の SELECT * が memo無しより重くない: 取る=%v 取らない=%v", bWith.P50, bNo.P50)
	}

	rec.Scope(
		"MySQL 8.0 / 3万行 / ROW_FORMAT=DYNAMIC / memo 3KB(inline)・24KB(off-page)",
		"全体走査は status で絞る COUNT（status 索引は張らない＝行を順に読む）",
		"範囲読みは id < 5000 で 5000 行を舐め、drain で全行 Scan する",
	)
	rec.Uncertain(
		"inline/off-page の境目は行フォーマットとページサイズで動く（DYNAMIC・16KB ページ前提）",
		"絶対値はバッファプールに載った状態のもの。ディスクから読むと差はさらに広がる",
		"VARCHAR も 8KB を超えるほど巨大だと off-page になりうる（今回の VARCHAR(2000)/3KB は inline）",
	)
	rec.Artifact(
		"internal/widthlab: サイズ違いの memo を inline/off-page に作り分けた走査と範囲読み",
		"docs/column-projection.md: VARCHAR/TEXT の使い分けと SELECT * の指針",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"『太い列が重い』の正体は VARCHAR か TEXT かではなく、値が行の中(inline)にあるか外(off-page)にあるか。",
		"3KB 程度なら TEXT でも行に載る（inline）ので、TEXT にしても舐めは軽くならない。",
		"十分大きい(24KB)TEXT は行の外に出て舐めは軽いが、SELECT * で読むと外を取りに行き重い。",
		"→ 確実なのは『別表に分ける（EXP-19）』。同居のまま軽くしたいなら、",
		"　 値を off-page になる大きさにでき、かつ普段の一覧で SELECT * しない場合に限る。まず SELECT * をやめる。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d interface{ Microseconds() int64 }) float64 { return float64(d.Microseconds()) / 1000.0 }
func dur(d interface{ String() string }) string      { return d.String() }
