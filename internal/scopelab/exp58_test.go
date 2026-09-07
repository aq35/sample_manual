package scopelab_test

// EXP-58: 共有ワーカーのテナントスコープ強制（境界を1本外すと越境する）。
//
//	MYSQL_DSN=... go test ./internal/scopelab/ -run TestEXP58 -v
//
// 共有ワーカー（1プロセスが全テナントを回す）で、テナント A の処理をするとき——
// スコープ強制ありなら A の行しか触らない（他テナント=0）。書き忘れ（status だけで絞る）だと
// B の行まで読み・書きしてしまう（越境＝情報漏洩・データ破壊）。cross-leak を数える。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/scopelab"
)

func TestEXP58_テナントスコープ強制(t *testing.T) {
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
	if err := scopelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-58", "tenant-scope-enforcement",
		"共有ワーカーでスコープ強制を外すと越境する。強制ありは cross-leak=0")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) スコープ強制あり（WHERE tenant_id=? を必ず付ける）: テナント A の処理は A の行だけ。 " +
			"他テナント B への読み漏れ=0・書き込み=0（cross-leak=0）。 " +
			"2) スコープ書き忘れ（status だけで絞る）: B の行まで読める（情報漏洩）し、" +
			"B の行まで done にしてしまう（データ破壊）。cross-leak>0。 " +
			"3) 共有ワーカーの安全は『プロセス分離』でなく『クエリのテナント境界強制』で決まる。")

	const tA, tB = "tenantA", "tenantB"
	const nEach = 100

	// ---- 読み: 強制あり ----
	if err := scopelab.Seed(ctx, db, []string{tA, tB}, nEach); err != nil {
		t.Fatal(err)
	}
	mineRS, otherRS, err := scopelab.ProcessRead(ctx, db, tA, true) // scoped
	if err != nil {
		t.Fatal(err)
	}
	// ---- 読み: 書き忘れ ----
	mineRU, otherRU, err := scopelab.ProcessRead(ctx, db, tA, false) // unscoped
	if err != nil {
		t.Fatal(err)
	}

	// ---- 書き: 強制あり（fresh seed）----
	if err := scopelab.Seed(ctx, db, []string{tA, tB}, nEach); err != nil {
		t.Fatal(err)
	}
	mineWS, otherWS, err := scopelab.ProcessWrite(ctx, db, tA, true) // scoped
	if err != nil {
		t.Fatal(err)
	}
	// ---- 書き: 書き忘れ（fresh seed）----
	if err := scopelab.Seed(ctx, db, []string{tA, tB}, nEach); err != nil {
		t.Fatal(err)
	}
	mineWU, otherWU, err := scopelab.ProcessWrite(ctx, db, tA, false) // unscoped
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "スコープ強制あり・読み: A だけ見える（cross-leak=0）",
		Counters: map[string]int64{"mine": int64(mineRS), "cross_leak": int64(otherRS)},
	})
	rec.Add(expkit.Variant{
		Name:     "スコープ書き忘れ・読み: B の行まで見える（情報漏洩）",
		Accident: true,
		Counters: map[string]int64{"mine": int64(mineRU), "cross_leak": int64(otherRU)},
		Notes:    []string{"A の処理なのに B の " + itoa(otherRU) + " 行が読めた（越境）"},
	})
	rec.Add(expkit.Variant{
		Name:     "スコープ強制あり・書き: A だけ done（他テナント破壊=0）",
		Counters: map[string]int64{"mine_done": int64(mineWS), "cross_write": int64(otherWS)},
	})
	rec.Add(expkit.Variant{
		Name:     "スコープ書き忘れ・書き: B の行まで done（データ破壊）",
		Accident: true,
		Counters: map[string]int64{"mine_done": int64(mineWU), "cross_write": int64(otherWU)},
		Notes:    []string{"A の処理なのに B の " + itoa(otherWU) + " 行を done にした（越境更新）"},
	})
	t.Logf("read scoped: mine=%d cross=%d / unscoped: mine=%d cross=%d",
		mineRS, otherRS, mineRU, otherRU)
	t.Logf("write scoped: mine=%d cross=%d / unscoped: mine=%d cross=%d",
		mineWS, otherWS, mineWU, otherWU)

	// ---- 検証 ----
	if otherRS != 0 {
		t.Errorf("強制ありの読みで越境した: cross=%d（0 のはず）", otherRS)
	}
	if otherRU == 0 {
		t.Errorf("書き忘れの読みで越境していない（するはず）: cross=%d", otherRU)
	}
	if otherWS != 0 {
		t.Errorf("強制ありの書きで他テナントを更新した: cross=%d（0 のはず）", otherWS)
	}
	if otherWU == 0 {
		t.Errorf("書き忘れの書きで他テナントを更新していない（するはず）: cross=%d", otherWU)
	}
	if mineRS != nEach || mineWS != nEach {
		t.Errorf("自テナントの処理が漏れた: read=%d write=%d（%d のはず）", mineRS, mineWS, nEach)
	}

	rec.Scope(
		"MySQL 8.0 / 共有表 scope_item に tenantA・tenantB 各 100 行が同居 / A の処理を実行",
		"cross_leak = 他テナント(B)の行を読んだ数、cross_write = 他テナント(B)の行を done にした数",
		"scoped = クエリに WHERE tenant_id=? を付ける（repo.Scope 相当）。unscoped = 書き忘れ",
	)
	rec.Uncertain(
		"実コードでは repo.Scope が :tenant を機械的に注入し、生 SQL は sqllint(rawdb/layerimport)で禁止する（EXP-8/9）",
		"テナントは ctx から取る（クライアント入力を信じない）。ここは processing を固定して境界の有無だけを見た",
		"共有ワーカーの分離はプロセス境界でなく『全クエリのスコープ強制』で決まる。だから機械強制が要る",
		"高感度テナントはさらにプロセス/資格情報を分けてブラスト半径を物理的に限定（docs/worker-tenancy.md）",
	)
	rec.Artifact(
		"internal/scopelab: ProcessRead/ProcessWrite（スコープ強制の有無で越境を測る）",
		"docs/tenant-scope.md: 共有ワーカーのテナントスコープ強制",
	)
	rec.Next("（セキュリティ実測の追加ぶん）")

	files, err := rec.Save(
		"共有ワーカーの安全は『プロセスを分けること』でなく『全クエリにテナント境界を強制すること』で決まる。" +
			"WHERE tenant_id=? を強制すればテナント A の処理は A の 100 行だけを触り、他テナント B への読み漏れ=0・" +
			"書き込み=0。境界を1本書き忘れると B の 100 行まで読め（情報漏洩）、done にしてしまう（データ破壊）。" +
			"だから境界は人手でなく repo.Scope で機械注入し、生 SQL は sqllint で禁止する。プロセス分離は最後の砦。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
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
