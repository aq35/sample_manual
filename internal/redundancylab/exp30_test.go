package redundancylab_test

// EXP-30: 冗長化（複数レプリカ）でアプリの中はどうあるべきか。
//
//	MYSQL_DSN=... go test ./internal/redundancylab/ -run TestEXP30 -v
//
// 冗長化＝同じアプリを N 個並べる。安全に動かすには、アプリの中がこう出来ていること:
//   1. 仕事の取得は「原子的な claim」。同じ1件を N 個が見ても、処理するのは1つだけ（二重処理を防ぐ）。
//   2. fence（世代番号）で、遅れた/切り離された古い担当の書き込みを弾く（正しさ）。
//   3. 接続プールをレプリカ数で割る（N 台ぶんの合計が DB の上限を超えない）。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/poolbudget"
	"github.com/aq35/sample_manual/internal/redundancylab"
)

func TestEXP30_冗長化とアプリ構造(t *testing.T) {
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
	if err := redundancylab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-30", "redundancy-app-structure",
		"複数レプリカで安全に動かすためのアプリの作り（claim・fence・接続予算）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) N レプリカが同じキューを取り合っても、原子的 claim なら各件はちょうど1回だけ処理される" +
			"（取りこぼしも二重も無い）。 " +
			"2) 世代交代（handoff）後、古い担当（小さい fence）の書き込みは fence ガードで弾かれ、" +
			"新しい担当（大きい fence）だけが通る。 " +
			"3) 接続予算はレプリカ数で割る。N 台 × 1台の取り分 が DB 予算を超えないことを起動時に確かめる。")

	const tenant = "red-main"
	const items, replicas = 500, 4

	// ---- ① 原子的 claim: N レプリカで取り合っても各件ちょうど1回 ----
	if err := redundancylab.SeedQueue(ctx, db, tenant, items); err != nil {
		t.Fatal(err)
	}
	counts, err := redundancylab.ClaimRace(ctx, db, tenant, replicas)
	if err != nil {
		t.Fatal(err)
	}
	claimed, pending, err := redundancylab.QueueStats(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	owners := 0
	for _, c := range counts {
		total += c
		if c > 0 {
			owners++
		}
	}
	rec.Add(expkit.Variant{
		Name:     "原子的 claim: 4レプリカで500件を取り合う → 各件ちょうど1回",
		Counters: map[string]int64{"claimed": claimed, "pending": pending, "sum_by_owner": total, "owners_worked": int64(owners)},
		Notes:    []string{"owner ごとの取り分がばらけ、合計＝件数・取りこぼし0・二重0（条件つき UPDATE が保証）"},
	})
	t.Logf("claim: claimed=%d pending=%d sum=%d owners=%d 内訳=%v", claimed, pending, total, owners, counts)

	// ---- ② fence: 世代交代後、古い担当の書き込みを弾く ----
	if err := redundancylab.InitFence(ctx, db, tenant, "r1", 1, "init"); err != nil {
		t.Fatal(err)
	}
	// 新しい担当 B（fence=2）が書く → 通る
	okB, err := redundancylab.GuardedWrite(ctx, db, tenant, "r1", "by-B", 2)
	if err != nil {
		t.Fatal(err)
	}
	// 遅れた古い担当 A（fence=1）が後から書く → 弾かれる
	okA, err := redundancylab.GuardedWrite(ctx, db, tenant, "r1", "by-A-late", 1)
	if err != nil {
		t.Fatal(err)
	}
	val, fence, err := redundancylab.CurrentVal(ctx, db, tenant, "r1")
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "fence: 新担当(fence=2)は通り、古い担当(fence=1)の遅延書き込みは弾かれる",
		Counters: map[string]int64{"newer_accepted": b2i(okB), "stale_rejected": b2i(!okA)},
		Notes:    []string{"最終 val=" + val + " fence=" + itoa(fence) + "（古い担当の値で上書きされない）"},
	})
	t.Logf("fence: B(fence2)=%v A(fence1)=%v 最終 val=%s fence=%d", okB, okA, val, fence)

	// ---- ③ 接続予算をレプリカ数で割る ----
	plan := poolbudget.Plan{DBMaxConnections: 1000, Reserved: 100, Containers: replicas, PerContainer: 100}
	// web と worker を別プロセスで冗長化した合計が予算に収まるか（起動時 Guard）
	fitErr := poolbudget.Guard(1000, 100,
		poolbudget.Role{Name: "web", Containers: replicas, PerContainer: 100},
		poolbudget.Role{Name: "worker", Containers: replicas, PerContainer: 50})
	overErr := poolbudget.Guard(1000, 100,
		poolbudget.Role{Name: "web", Containers: replicas, PerContainer: 300}) // 4×300=1200 > 900
	rec.Add(expkit.Variant{
		Name:     "接続予算: レプリカ数で割る（起動時 Guard で fail-fast）",
		Counters: map[string]int64{"demand": int64(plan.Demand()), "budget": int64(plan.Budget()), "fits": b2i(plan.Fits())},
		Notes: []string{
			"web4×100 + worker4×50 = 600 ≤ 900 → OK: " + boolStr(fitErr == nil),
			"web4×300 = 1200 > 900 → 起動時に弾く: " + boolStr(overErr != nil),
		},
	})
	t.Logf("budget: demand=%d budget=%d fits=%v / fit=%v over=%v",
		plan.Demand(), plan.Budget(), plan.Fits(), fitErr, overErr)

	// ---- 検証 ----
	if claimed != items || pending != 0 || total != int64(items) {
		t.Errorf("claim が exactly-once でない: claimed=%d pending=%d sum=%d (期待 %d/0/%d)",
			claimed, pending, total, items, items)
	}
	if owners < 2 {
		t.Errorf("仕事が1レプリカに偏った（冗長化の意味が薄い）: owners=%d 内訳=%v", owners, counts)
	}
	if !okB || okA {
		t.Errorf("fence ガードが効いていない: newer=%v stale=%v", okB, okA)
	}
	if val != "by-B" || fence != 2 {
		t.Errorf("古い担当の書き込みで上書きされた: val=%s fence=%d", val, fence)
	}
	if fitErr != nil {
		t.Errorf("収まる構成が弾かれた: %v", fitErr)
	}
	if overErr == nil {
		t.Errorf("予算オーバーが弾かれていない")
	}

	rec.Scope(
		"MySQL 8.0 / キュー500件を4レプリカ / fence は redstate の条件つき UPDATE / 予算は poolbudget",
		"claim は『pending を1件見て、その id を条件つき UPDATE』。取れるのは1レプリカだけ",
		"レプリカ・レジストリやリーダー選出は使わず、DB の原子性と fence だけで安全にする",
	)
	rec.Uncertain(
		"lease による『テナント→担当レプリカ』の割り当ては EXP-2（ここは claim と fence の意味論に集中）",
		"二重の“効果”を完全に防ぐには冪等性（EXP-27）も要る。claim は二重“処理”を、fence は古い“書き込み”を防ぐ",
		"graceful shutdown（drain して lease を返す）で failover を速くする（EXP-3）",
		"メモリ上のキャッシュはどのレプリカでも成り立つ形に（テナント単位・共有しない）",
	)
	rec.Artifact(
		"internal/redundancylab: 原子的 claim・fence ガード・接続予算の確認",
		"docs/redundancy.md: 冗長化するときアプリの中はどうあるべきか",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"冗長化は『N 台並べれば動く』ではなく、アプリの中がそれ用に出来ていること: " +
			"仕事の取得は原子的 claim（二重処理を防ぐ）、書き込みは fence ガード（古い担当を弾く）、" +
			"書き込みは冪等（EXP-27）、接続予算はレプリカ数で割る（起動時に Guard）。" +
			"担当割り当ては lease（EXP-2）、速い failover は graceful shutdown（EXP-3）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
func boolStr(b bool) string {
	if b {
		return "はい"
	}
	return "いいえ"
}
func itoa(n int64) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
