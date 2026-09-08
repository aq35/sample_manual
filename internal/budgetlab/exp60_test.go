package budgetlab_test

// EXP-60: 接続予算とオートスケール・ストーム（台数を増やすと DB 接続が枯れる）。
//
//	MYSQL_DSN=... go test ./internal/budgetlab/ -run TestEXP60 -v
//
// オートスケールでコンテナを増やすと、DB への総接続 = 台数 × プール本数 が膨らむ。
// これが max_connections を超えると、MySQL は新規接続を 1040(Too many connections) で拒否する
// ＝台数を増やすほど悪化する（ストーム）。poolbudget で per-container を絞れば総接続が予算内に収まり、
// 拒否 0。さらに Guard は「接続を1本も張る前に」超過を検知して fail-fast する。

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/poolbudget"
)

// openContainers は「containers 台 × per 本」の接続を実際に張って保持する（オートスケールの再現）。
// 返り値: 成功本数 / 1040 拒否数 / その他エラー数 / 解放関数。
func openContainers(ctx context.Context, dsn string, containers, per int) (okN, rej1040, other int32, release func()) {
	var dbs []*sql.DB
	var mu sync.Mutex
	var held []*sql.Conn
	var wg sync.WaitGroup
	for c := 0; c < containers; c++ {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			atomic.AddInt32(&other, int32(per))
			continue
		}
		db.SetMaxOpenConns(per)
		db.SetMaxIdleConns(per)
		db.SetConnMaxLifetime(0)
		dbs = append(dbs, db)
		for j := 0; j < per; j++ {
			wg.Add(1)
			go func(db *sql.DB) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				conn, err := db.Conn(cctx) // 物理接続を1本開いて保持
				if err != nil {
					var me *mysql.MySQLError
					if ok := asMySQL(err, &me); ok && me.Number == 1040 {
						atomic.AddInt32(&rej1040, 1) // Too many connections
					} else {
						atomic.AddInt32(&other, 1)
					}
					return
				}
				atomic.AddInt32(&okN, 1)
				mu.Lock()
				held = append(held, conn)
				mu.Unlock()
			}(db)
		}
	}
	wg.Wait()
	release = func() {
		for _, c := range held {
			_ = c.Close()
		}
		for _, db := range dbs {
			_ = db.Close()
		}
	}
	return okN, rej1040, other, release
}

func asMySQL(err error, target **mysql.MySQLError) bool {
	for err != nil {
		if me, ok := err.(*mysql.MySQLError); ok {
			*target = me
			return true
		}
		type unwrap interface{ Unwrap() error }
		if u, ok := err.(unwrap); ok {
			err = u.Unwrap()
		} else {
			return false
		}
	}
	return false
}

func TestEXP60_接続予算とオートスケール(t *testing.T) {
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
	var maxConns int
	if err := db.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&maxConns); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-60", "connection-budget-autoscale",
		"台数×プールが max_connections を超えると 1040 拒否。予算で絞れば拒否0・Guard は事前に止める")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 素朴オートスケール（per-container 固定 × 台数増）は、総接続が max_connections を超えると " +
			"新規接続が 1040(Too many connections) で拒否される＝台数を増やすほど悪化。 " +
			"2) poolbudget.RecommendPerContainer で per-container を絞れば総接続が予算内 → 拒否 0。 " +
			"3) Guard は接続を1本も張る前に超過を検知して fail-fast（起動時に止める）。")

	const reserved = 10 // 管理/移行/監視ぶん
	budget := maxConns - reserved
	const targetContainers = 15

	// ---- ① 素朴オートスケール: per-container 固定 15 本 × 15 台 = 225 本要求 ----
	const naivePer = 15
	naivePlan := poolbudget.Plan{DBMaxConnections: maxConns, Reserved: reserved, Containers: targetContainers, PerContainer: naivePer}
	naiveGuard := poolbudget.Guard(maxConns, reserved, poolbudget.Role{Name: "app", Containers: targetContainers, PerContainer: naivePer})
	nOK, nRej, nOther, nRel := openContainers(ctx, dsn, targetContainers, naivePer)
	nRel() // すぐ解放（次の計測のため）

	rec.Add(expkit.Variant{
		Name:     "素朴オートスケール（固定15本×15台=225要求）: DB が 1040 で拒否（ストーム）",
		Accident: true,
		Counters: map[string]int64{
			"demand": int64(naivePlan.Demand()), "budget": int64(budget), "max_connections": int64(maxConns),
			"opened_ok": int64(nOK), "rejected_1040": int64(nRej), "other_err": int64(nOther),
			"guard_blocked": b2i(naiveGuard != nil),
		},
		Notes: []string{"要求 " + itoa(naivePlan.Demand()) + " > 予算 " + itoa(budget) +
			" / 実際に " + itoa64(int64(nRej)) + " 本が 1040 拒否。Guard は事前に " + guardStr(naiveGuard)},
	})

	// ---- ② 予算オートスケール: RecommendPerContainer で絞る ----
	budgetPer := poolbudget.RecommendPerContainer(budget, targetContainers, 0.2) // 20% 余白
	budgetPlan := poolbudget.Plan{DBMaxConnections: maxConns, Reserved: reserved, Containers: targetContainers, PerContainer: budgetPer}
	budgetGuard := poolbudget.Guard(maxConns, reserved, poolbudget.Role{Name: "app", Containers: targetContainers, PerContainer: budgetPer})
	bOK, bRej, bOther, bRel := openContainers(ctx, dsn, targetContainers, budgetPer)
	bRel()

	rec.Add(expkit.Variant{
		Name:     "予算オートスケール（per-container を予算内に絞る）: 拒否 0",
		Counters: map[string]int64{
			"per_container": int64(budgetPer), "demand": int64(budgetPlan.Demand()), "budget": int64(budget),
			"opened_ok": int64(bOK), "rejected_1040": int64(bRej), "other_err": int64(bOther),
			"guard_ok": b2i(budgetGuard == nil), "fits": b2i(budgetPlan.Fits()),
		},
		Notes: []string{"per-container " + itoa(naivePer) + "→" + itoa(budgetPer) +
			" / 要求 " + itoa(budgetPlan.Demand()) + " ≤ 予算 " + itoa(budget) + " → 1040 拒否 " + itoa64(int64(bRej))},
	})
	t.Logf("naive: demand=%d ok=%d rej1040=%d guard=%v / budget: per=%d demand=%d ok=%d rej1040=%d guard=%v",
		naivePlan.Demand(), nOK, nRej, naiveGuard != nil, budgetPer, budgetPlan.Demand(), bOK, bRej, budgetGuard != nil)

	// ---- 検証 ----
	if nRej == 0 {
		t.Errorf("素朴オートスケールで 1040 拒否が起きていない（起きるはず）: rej=%d（demand=%d, max=%d）", nRej, naivePlan.Demand(), maxConns)
	}
	if naiveGuard == nil {
		t.Errorf("Guard が素朴構成を止めていない（止めるはず）: demand=%d budget=%d", naivePlan.Demand(), budget)
	}
	if bRej != 0 {
		t.Errorf("予算構成で 1040 拒否が出た（0 のはず）: rej=%d（demand=%d）", bRej, budgetPlan.Demand())
	}
	if budgetGuard != nil {
		t.Errorf("Guard が予算構成を誤って止めた: %v", budgetGuard)
	}
	if !budgetPlan.Fits() {
		t.Errorf("予算構成が Fits でない: demand=%d budget=%d", budgetPlan.Demand(), budget)
	}
	if int(bOK) != budgetPlan.Demand() {
		t.Errorf("予算構成で開けた本数が要求と一致しない: ok=%d demand=%d", bOK, budgetPlan.Demand())
	}

	rec.Scope(
		"MySQL 8.0 / max_connections="+itoa(maxConns)+" / 予約 "+itoa(reserved)+" / 目標 "+itoa(targetContainers)+" 台",
		"1040 拒否 = DB が『これ以上つなげない』と新規接続を断った回数（実測）",
		"poolbudget.Demand=台数×per、Budget=max-予約、RecommendPerContainer=余白20%で per を算出",
	)
	rec.Uncertain(
		"max_connections はこのホストの既定(151)。本番の値・予約・レプリカ読み取りプールで数字は動く",
		"『予算に収まる』は接続が枯れないだけで、スループットが良い保証ではない（膝は EXP-5）",
		"RDS Proxy を挟むと DB 側接続はコンテナ数に比例しなくなる（ProxyBackend で頭打ち・rds-proxy.md）",
		"実運用は Guard を起動時に呼び、超過なら起動を失敗させる（1本も張らずに止める）のが安い",
	)
	rec.Artifact(
		"internal/budgetlab: 実接続で 1040 拒否を再現し、poolbudget で防げることを実証",
		"docs/connection-budget.md: 接続予算とオートスケール・ストーム",
	)
	rec.Next("（アーキテクチャ実測）")

	files, err := rec.Save(
		"オートスケールで台数を増やすと DB への総接続 = 台数 × プール が膨らみ、max_connections を超えると " +
			"MySQL が 1040(Too many connections) で新規接続を拒否する＝増やすほど悪化するストーム（実測: 固定15本×15台=" +
			"225要求で多数が 1040 拒否）。poolbudget.RecommendPerContainer で per-container を予算内に絞れば総接続が " +
			"収まり拒否 0。さらに Guard は接続を1本も張る前に超過を検知して起動を止める（fail-fast）。" +
			"『縦より横』だが、横に割るときは合計接続が DB 予算を超えないことを Guard で担保する。")
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
func guardStr(err error) string {
	if err != nil {
		return "止めた（fail-fast）"
	}
	return "通した"
}
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
func itoa64(n int64) string { return itoa(int(n)) }
