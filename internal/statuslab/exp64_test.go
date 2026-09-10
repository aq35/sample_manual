package statuslab_test

// EXP-64: in_progress → pending の「回収」を ①時間なし で書くと二重実行、②heartbeat+CAS で止まる。
//
//	MYSQL_DSN=... go test ./internal/statuslab/ -run TestEXP64 -v
//
// docs/worker-state-time.md の §3（回収を①で書くと二重実行）と §5 の検証(3) を数字で裏付ける。
// あわせて②の抽出クエリの索引効果（索引末尾を heartbeat_at にする）も測る。

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/statuslab"
)

func TestEXP64_回収は時間指定とCASで書く(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-64", "reclaim-time-and-cas",
		"in_progress→pending の回収は時間指定(heartbeat)とCASで書く（①時間なしは二重実行）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"回収を『status=in_progress を全部戻す』(①時間なし)で書くと、heartbeat が新しい＝生きている担当の行まで",
		"奪い、二重実行になる。heartbeat が lease より古い行だけを CAS(WHERE に status と heartbeat)で戻すと、",
		"生きている担当は奪わず(奪取0)、落ちた担当ぶんだけを affected_rows として回収できる。",
		"また回収候補の抽出は、索引末尾を heartbeat_at にすると completed の山を舐めずに済む。",
	}, " "))

	const tenant = "st-job"
	const alive = 500     // 生きている担当（heartbeat 新しい・奪ってはいけない）
	const stale = 500     // 落ちた担当（heartbeat 古い・回収すべき）
	const noise = 100_000 // completed の山（索引が無いと②の抽出がこれを舐める）
	const leaseSec = 30
	const staleAge = 120 * time.Second // lease(30s) より十分古い
	rec.Workload("alive", alive).Workload("stale", stale).
		Workload("completed_noise", noise).Workload("lease_sec", leaseSec)

	// ---- ① 危険: 時間を見ない回収 → 生きている担当を奪う（二重実行） ----
	if err := statuslab.SeedJobs(ctx, db, "st_job", tenant, alive, stale, noise, staleAge); err != nil {
		t.Fatal(err)
	}
	aliveBefore, err := statuslab.AliveInProgress(ctx, db, "st_job", tenant, leaseSec)
	if err != nil {
		t.Fatal(err)
	}
	reclaimedAll, err := statuslab.ReclaimAll(ctx, db, "st_job", tenant)
	if err != nil {
		t.Fatal(err)
	}
	aliveAfterBad, err := statuslab.AliveInProgress(ctx, db, "st_job", tenant, leaseSec)
	if err != nil {
		t.Fatal(err)
	}
	stolenBad := aliveBefore - aliveAfterBad // 生きている担当から奪った数 = 二重実行の危険
	rec.Add(expkit.Variant{
		Name:     "回収①: 時間を見ない（status だけで全部戻す）",
		Accident: true,
		Metrics:  map[string]float64{"reclaimed": float64(reclaimedAll), "stolen_alive": float64(stolenBad)},
		Notes:    []string{"生きている担当から stolen_alive 件を奪う＝そのぶん二重実行になる"},
	})
	t.Logf("①時間なし: 戻した=%d / うち生存担当から奪取=%d（二重実行の危険）", reclaimedAll, stolenBad)

	// ---- ② 正しい: heartbeat + CAS → 落ちた担当だけ回収、生存は奪わない ----
	if err := statuslab.SeedJobs(ctx, db, "st_job", tenant, alive, stale, noise, staleAge); err != nil {
		t.Fatal(err)
	}
	reclaimedStale, err := statuslab.ReclaimStale(ctx, db, "st_job", tenant, leaseSec)
	if err != nil {
		t.Fatal(err)
	}
	aliveAfterGood, err := statuslab.AliveInProgress(ctx, db, "st_job", tenant, leaseSec)
	if err != nil {
		t.Fatal(err)
	}
	stolenGood := aliveBefore - aliveAfterGood
	rec.Add(expkit.Variant{
		Name:    "回収②: heartbeat が古い行だけを CAS で戻す",
		Metrics: map[string]float64{"reclaimed": float64(reclaimedStale), "stolen_alive": float64(stolenGood)},
		Notes:   []string{"affected_rows=" + strconv.FormatInt(reclaimedStale, 10) + " が『実際に奪えた件数』。生存担当は奪取0"},
	})
	t.Logf("②heartbeat+CAS: 回収=%d（=stale %d）/ 生存担当から奪取=%d", reclaimedStale, stale, stolenGood)

	// ---- 回収候補の抽出: 索引末尾 heartbeat_at あり / なし ----
	if err := statuslab.SeedJobs(ctx, db, "st_job_noidx", tenant, alive, stale, noise, staleAge); err != nil {
		t.Fatal(err)
	}
	idx := statuslab.FindStale(ctx, db, "st_job", tenant, leaseSec, 100, 30)
	noidx := statuslab.FindStale(ctx, db, "st_job_noidx", tenant, leaseSec, 100, 30)
	rec.Add(expkit.Variant{
		Name:    "回収候補の抽出: 索引 (tenant,status,heartbeat_at) あり",
		Metrics: map[string]float64{"p50_ms": ms(idx.P50)},
	})
	rec.Add(expkit.Variant{
		Name:     "回収候補の抽出: 索引なし（completed の山を舐める）",
		Accident: true,
		Metrics:  map[string]float64{"p50_ms": ms(noidx.P50)},
		Notes:    []string{"索引あり " + dur(idx.P50) + " → なし " + dur(noidx.P50)},
	})
	t.Logf("回収候補抽出: 索引あり=%v / なし=%v", idx.P50, noidx.P50)

	// ---- 検証 ----
	// ① 時間なし回収は生きている担当を奪う（二重実行が起きる）
	if stolenBad <= 0 {
		t.Errorf("①時間なし回収が生存担当を奪っていない（実験前提が崩れている）: stolen=%d", stolenBad)
	}
	// ② heartbeat+CAS は生存担当を1件も奪わない
	if stolenGood != 0 {
		t.Errorf("②heartbeat+CAS が生存担当を奪った: stolen=%d（0 のはず）", stolenGood)
	}
	// ② の回収数は stale 件ちょうど（affected_rows が正しく効いている）
	if reclaimedStale != int64(stale) {
		t.Errorf("②の回収数が stale と一致しない: reclaimed=%d stale=%d", reclaimedStale, stale)
	}
	// 抽出は索引ありの方が速い
	if idx.P50 >= noidx.P50 {
		t.Errorf("回収候補抽出で索引ありが速くない: あり=%v なし=%v", idx.P50, noidx.P50)
	}

	rec.Scope(
		"MySQL 8.0 / in_progress 1000(生存500・落ち500) + completed 10万 / lease 30s・stale 120s",
		"回収①は status=in_progress を全 UPDATE。②は status=in_progress AND heartbeat_at < NOW(3)-INTERVAL 30 SECOND",
		"基準時刻はすべて DB の NOW(3)。生存/落ちの区別は heartbeat_at の新旧のみ",
	)
	rec.Uncertain(
		"二重実行の『実害』はここでは奪取件数で代理測定（実際の重複処理は worker 側の話）",
		"stale の絶対レイテンシは completed 量とバッファプール状態に依存。桁の関係だけが要点",
		"lease 値そのものの決め方は業務要件（何秒気づかなくて許されるか）で技術では決まらない（調査 §2.5）",
	)
	rec.Artifact(
		"internal/statuslab/exp64.go: SeedJobs / ReclaimAll(①) / ReclaimStale(②) / FindStale",
		"docs/worker-state-time.md: 欲しい状態は時間指定が要るか要らないか",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"回収(in_progress→pending)は『時間指定(heartbeat が lease より古い)』＋『CAS(affected_rows で奪取確認)』で書く。",
		"status だけで全部戻す①は、生きている担当を奪って二重実行になる。",
		"基準時刻は必ず DB の NOW(3)。回収候補の抽出索引は末尾を heartbeat_at にする(id ではない)。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
