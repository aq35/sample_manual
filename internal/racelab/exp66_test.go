package racelab_test

// EXP-66: Web と Worker が同じ行を触るレースを、状態 CAS と version（楽観ロック）で止める。
//
//	MYSQL_DSN=... go test ./internal/racelab/ -run TestEXP66 -v
//
// 3つのレースを再現する:
//   A) 二重 claim（pending を複数 worker が掴む）
//   B) cancel 中の complete（Web の cancel を worker の完了が上書き）
//   C) 入力の途中編集（worker が古い input の結果を更新後の行に書く）

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/racelab"
)

func TestEXP66_WebとWorkerのレースをCASとversionで止める(t *testing.T) {
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
	if err := racelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-66", "web-worker-race",
		"Web と Worker のレースを状態 CAS と version（楽観ロック）で止める")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"状態遷移は遷移元を WHERE に入れた CAS(affected_rows=1 で勝者確定)で守る。claim も complete も同じ。",
		"入力の同時編集は version(楽観ロック)で守る。worker は読んだ version を WHERE に入れて書き、",
		"Web が先に編集していたら affected_rows=0 で結果を捨てて読み直す。",
		"CAS/version が無いと、二重 claim・cancel の消失・stale result が起きる。",
	}, " "))

	const tenant = "race-main"
	const n = 200

	// ---- A) 二重 claim ----
	claimNaive, err := racelab.ClaimRace(ctx, db, tenant, n, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	claimCAS, err := racelab.ClaimRace(ctx, db, tenant, n, 8, true)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "A claim: 無条件 UPDATE（読取り→書込みの隙間）",
		Accident: true,
		Metrics:  map[string]float64{"double_claims": float64(claimNaive.DoubleClaims), "total_claims": float64(claimNaive.TotalClaims)},
		Notes:    []string{"同じ pending を複数 worker が掴む＝二重処理。掴んだ総数が行数 " + strconv.Itoa(n) + " を超える"},
	})
	rec.Add(expkit.Variant{
		Name:    "A claim: 状態 CAS（WHERE status='pending'・affected_rows=1）",
		Metrics: map[string]float64{"double_claims": float64(claimCAS.DoubleClaims), "total_claims": float64(claimCAS.TotalClaims)},
		Notes:   []string{"1行は1人にしか渡らない。掴んだ総数=行数ちょうど・二重0"},
	})
	t.Logf("A 二重claim: 無条件 double=%d/total=%d ／ CAS double=%d/total=%d",
		claimNaive.DoubleClaims, claimNaive.TotalClaims, claimCAS.DoubleClaims, claimCAS.TotalClaims)

	// ---- B) cancel 中の complete ----
	lostNaive, err := racelab.CancelThenComplete(ctx, db, tenant, n, false)
	if err != nil {
		t.Fatal(err)
	}
	lostGuarded, err := racelab.CancelThenComplete(ctx, db, tenant, n, true)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "B complete: 状態を見ない UPDATE ... WHERE id",
		Accident: true,
		Metrics:  map[string]float64{"lost_cancels": float64(lostNaive)},
		Notes:    []string{"Web の cancel を worker の完了が上書き。" + strconv.FormatInt(lostNaive, 10) + " 件の cancel が消えて completed に"},
	})
	rec.Add(expkit.Variant{
		Name:    "B complete: 状態 CAS（WHERE status='in_progress' AND owner）",
		Metrics: map[string]float64{"lost_cancels": float64(lostGuarded)},
		Notes:   []string{"cancel 済みには一致しない(affected_rows=0)ので worker は結果を破棄。消えた cancel 0"},
	})
	t.Logf("B cancel消失: 無ガード=%d ／ CASガード=%d", lostNaive, lostGuarded)

	// ---- C) 入力の途中編集（stale result） ----
	staleNaive, err := racelab.StaleInputRace(ctx, db, tenant, n, false)
	if err != nil {
		t.Fatal(err)
	}
	staleGuarded, err := racelab.StaleInputRace(ctx, db, tenant, n, true)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "C 結果書込: version を見ない UPDATE ... WHERE id",
		Accident: true,
		Metrics:  map[string]float64{"stale_writes": float64(staleNaive)},
		Notes:    []string{"Web が編集した後の行に、古い input で計算した結果を書く。" + strconv.FormatInt(staleNaive, 10) + " 件が stale"},
	})
	rec.Add(expkit.Variant{
		Name:    "C 結果書込: version 楽観ロック（WHERE version=読んだ値）",
		Metrics: map[string]float64{"stale_writes": float64(staleGuarded)},
		Notes:   []string{"version 不一致で affected_rows=0 → 結果を捨てて読み直す。stale 0"},
	})
	t.Logf("C stale result: 無ガード=%d ／ versionガード=%d", staleNaive, staleGuarded)

	// ---- 検証 ----
	// A: CAS は二重0・掴んだ総数=行数ちょうど
	if claimCAS.DoubleClaims != 0 {
		t.Errorf("A CAS で二重 claim が起きた: %d（0 のはず）", claimCAS.DoubleClaims)
	}
	if claimCAS.TotalClaims != int64(n) {
		t.Errorf("A CAS の掴んだ総数が行数と一致しない: total=%d n=%d", claimCAS.TotalClaims, n)
	}
	// B: CAS ガードは cancel を消さない。無ガードは消す（事故の再現）
	if lostGuarded != 0 {
		t.Errorf("B CAS ガードで cancel が消えた: %d（0 のはず）", lostGuarded)
	}
	if lostNaive == 0 {
		t.Errorf("B 無ガードで cancel が消えない（事故が再現していない）: %d", lostNaive)
	}
	// C: version ガードは stale を書かない。無ガードは書く（事故の再現）
	if staleGuarded != 0 {
		t.Errorf("C version ガードで stale result を書いた: %d（0 のはず）", staleGuarded)
	}
	if staleNaive == 0 {
		t.Errorf("C 無ガードで stale result が出ない（事故が再現していない）: %d", staleNaive)
	}

	rec.Scope(
		"MySQL 8.0 / race_job "+strconv.Itoa(n)+"件 / claim は concurrency=8 で奪い合い",
		"A は並行(goroutine)、B・C は Web→Worker の順を決定的に起こして上書き/stale を測る",
		"状態 CAS=遷移元を WHERE に入れる、version=読んだ値を WHERE に入れる",
	)
	rec.Uncertain(
		"A の無条件版の二重数は並行タイミング依存（環境で変わる）。CAS の不変量(二重0・total=n)だけが確定的",
		"SELECT ... FOR UPDATE SKIP LOCKED でも claim は守れる（別解）。ここでは楽観 CAS を測った",
		"version は競合が激しいとやり直しが増える（repository-layer §2.3・楽観ロックはタダではない）",
	)
	rec.Artifact(
		"internal/racelab: ClaimRace(A) / CancelThenComplete(B) / StaleInputRace(C)",
		"docs/web-worker-race.md: Web と Worker のレースコンディション対策",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"Web と Worker が同じ行を触るなら、状態遷移は遷移元を WHERE に入れた CAS(affected_rows で勝者確定)、",
		"入力の同時編集は version(楽観ロック)で守る。claim も complete も『状態を見ずに id だけで UPDATE』は禁止。",
		"CAS/version が無いと、二重 claim・cancel の消失・stale result が起きる。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
