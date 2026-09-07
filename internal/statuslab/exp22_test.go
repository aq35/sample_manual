package statuslab_test

// EXP-22: ステータス（pending/queued/進行中/完了/失敗/キャンセル）でテーブルを分けるべきか。
//
//	MYSQL_DSN=... go test ./internal/statuslab/ -run TestEXP22 -v
//
// 直感では「状態ごとに表を分ければ速そう」。だが状態は遷移で動く。3つを実測する:
//   1) status で絞る一覧は、1テーブル＋status索引で足りるか（分割不要か）
//   2) 状態を別テーブルに分けると、遷移が UPDATE→(DELETE+INSERT) の引っ越しになって重いか
//   3) 終端行が溜まると、テーブル全体を舐める操作（全体COUNT）は重くなるか（=hot/cold の動機）

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/statuslab"
)

func TestEXP22_ステータスで表を分けるか(t *testing.T) {
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
	if err := statuslab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-22", "status-table-split",
		"ステータスでテーブルを分けるべきか（索引 vs 分割 vs hot/cold）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) pending を古い順に取る worker クエリは、1テーブル＋status索引で狙い撃てる。終端行が大量でも、",
		"   索引が効くので速い（分割は不要）。索引が無いと、古い terminal を大量に舐めて遅い。",
		"2) 状態を別テーブルに分けると、遷移が UPDATE 一発から DELETE+INSERT の引っ越し（要トランザクション）",
		"   になり、明確に重い。状態は『分ける鍵』でなく『索引で絞るもの』。",
		"3) 終端行が溜まると、テーブル全体を舐める操作（全体 COUNT など）は重くなる。",
		"   → pending 抽出そのものは索引で速いまま。hot/cold 分離が効くのは全体走査と掃除（EXP-15 の DROP 10x）。",
	}, " "))

	const tenant = "st-main"
	const total = 200_000
	const active = 2_000 // 直近 1% だけが active、99% は溜まった terminal
	const limit = 100
	if err := statuslab.Seed(ctx, db, tenant, total, active); err != nil {
		t.Fatal(err)
	}
	activeStart := statuslab.ActiveStart(total, active)
	rec.Workload("total_rows", total).Workload("active_rows", active).Workload("page_limit", limit)

	// ① pending 抽出（古い順）: 1テーブル索引あり / 索引なし / hot（active だけの小さい表）
	one := statuslab.FindPending(ctx, db, "st_one", tenant, limit, 30)
	noidx := statuslab.FindPending(ctx, db, "st_noidx", tenant, limit, 30)
	hot := statuslab.FindPending(ctx, db, "st_hot", tenant, limit, 30)
	rec.Add(expkit.Variant{
		Name:    "pending 抽出: 1テーブル＋status索引（終端が大量でも）",
		Metrics: map[string]float64{"p50_ms": ms(one.P50)},
		Notes:   []string{"索引で pending だけを狙い撃つので、terminal が 99% でも速い"},
	})
	rec.Add(expkit.Variant{
		Name:     "pending 抽出: 1テーブル・status索引なし（全表走査）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(noidx.P50)},
		Notes:    []string{"索引あり " + dur(one.P50) + " → 索引なし " + dur(noidx.P50) + "（索引が無いと古い terminal を大量に舐める）"},
	})
	rec.Add(expkit.Variant{
		Name:    "pending 抽出: hot 表（active だけを隔離した小表）",
		Metrics: map[string]float64{"p50_ms": ms(hot.P50)},
		Notes:   []string{"1テーブル索引 " + dur(one.P50) + " ≒ hot " + dur(hot.P50) + "（索引が効くなら分割しても大差ない）"},
	})
	t.Logf("pending抽出: 1表索引=%v / 索引なし=%v / hot=%v", one.P50, noidx.P50, hot.P50)

	// ② 状態遷移: 1テーブル UPDATE 一発 vs 別テーブルへ引っ越し（DELETE+INSERT）
	const moves = 500
	up := statuslab.TransitionOneTable(ctx, db, tenant, activeStart, moves)
	mv := statuslab.TransitionSplit(ctx, db, tenant, activeStart, moves)
	rec.Add(expkit.Variant{
		Name:    "状態遷移: 1テーブル UPDATE 一発",
		Metrics: map[string]float64{"per_move_p50_ms": ms(up.P50), "per_move_p95_ms": ms(up.P95)},
	})
	rec.Add(expkit.Variant{
		Name:     "状態遷移: 別テーブルへ引っ越し（DELETE+INSERT・要トランザクション）",
		Accident: true,
		Metrics:  map[string]float64{"per_move_p50_ms": ms(mv.P50), "per_move_p95_ms": ms(mv.P95)},
		Notes:    []string{"UPDATE " + dur(up.P50) + " → 引っ越し " + dur(mv.P50) + "（2表への書き込み＋2索引更新＋tx）"},
	})
	t.Logf("遷移/1件: UPDATE=%v / 引っ越し=%v", up.P50, mv.P50)

	// ③ 終端が溜まると全体走査は重い: 全体 COUNT（terminal 込み）を 1表 vs hot で
	countOne := statuslab.CountAll(ctx, db, "st_one", tenant, 20)
	countHot := statuslab.CountAll(ctx, db, "st_hot", tenant, 20)
	rec.Add(expkit.Variant{
		Name:    "全体 COUNT（terminal 込み・1テーブル）",
		Metrics: map[string]float64{"p50_ms": ms(countOne.P50)},
	})
	rec.Add(expkit.Variant{
		Name:    "全体 COUNT（active だけの hot 表）",
		Metrics: map[string]float64{"p50_ms": ms(countHot.P50)},
		Notes:   []string{"1表(20万) " + dur(countOne.P50) + " → hot(2千) " + dur(countHot.P50) + "（全体を舐める操作は溜まった terminal に比例して重い）"},
	})
	t.Logf("全体COUNT: 1表=%v / hot=%v", countOne.P50, countHot.P50)

	// ---- 検証 ----
	// (1) 索引があれば active 抽出は速い。索引なし（全表走査）より明確に速い
	if one.P50 >= noidx.P50 {
		t.Errorf("索引ありが索引なしより速くない: 索引あり=%v なし=%v", one.P50, noidx.P50)
	}
	// (2) 別テーブルへの引っ越しは UPDATE 一発より重い
	if mv.P50 <= up.P50 {
		t.Errorf("引っ越しが UPDATE より重くない: UPDATE=%v 引っ越し=%v", up.P50, mv.P50)
	}
	// (3) 終端が溜まると全体走査は重い（hot の方が軽い）
	if countHot.P50 >= countOne.P50 {
		t.Errorf("hot の全体COUNT が 1表より軽くない: 1表=%v hot=%v", countOne.P50, countHot.P50)
	}

	rec.Scope(
		"MySQL 8.0 / 20万行・うち active 1%（2千）/ status索引 (tenant_id,status,id)",
		"active 抽出は status IN (0,1,2) を id 順に LIMIT 100",
		"遷移は active→done。UPDATE 版は st_one、引っ越し版は st_active→st_terminal を tx で",
	)
	rec.Uncertain(
		"索引ありの active 抽出は terminal 量にほぼ依らない（索引シーク）。この結論は索引前提",
		"掃除（terminal の削除）は本実験外。DELETE より DROP PARTITION が速いのは EXP-15（10x）",
		"分割の別の代償（結合・二重書き込みの整合・移行）は本実験の対象外",
		"絶対値はバッファプールに載った状態のもの",
	)
	rec.Artifact(
		"internal/statuslab: 索引あり/なし・分割・hot での active 抽出、遷移コスト、全体走査",
		"docs/status-table-split.md: 状態でテーブルを分けるべきかの指針",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"ステータスは遷移で動くので『状態ごとに表を分ける』のは基本しない（遷移が引っ越しになって重い）。",
		"status に索引を張れば、終端行が大量でも 1 テーブルのまま active 抽出は速い。",
		"分けるなら状態でなく hot/cold（動く行 vs 終わった行）。全体走査・掃除が重くなってきたら、",
		"終端をアーカイブ表へ移すか、時間でパーティションして DROP PARTITION で捨てる（EXP-15）。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d interface{ Microseconds() int64 }) float64 { return float64(d.Microseconds()) / 1000.0 }
func dur(d interface{ String() string }) string      { return d.String() }
