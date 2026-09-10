package ingestlab_test

// EXP-65: 通常動作では入らない不正データの「入口」を、DB 制約で塞ぐ。
//
//	MYSQL_DSN=... go test ./internal/ingestlab/ -run TestEXP65 -v
//
// 原因は「作成時バリデーションの不備」と「直接 DB 入力」。アプリのチェックはアプリ経路しか守れない。
// 同じ不正 INSERT を、ガード無し表と DB 制約(ENUM/CHECK/FK)有り表に打ち、landing するかを比較する。

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/ingestlab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP65_不正データの入口はDB制約で塞ぐ(t *testing.T) {
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
	if err := ingestlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-65", "ingest-guard",
		"不正データの入口を DB 制約(ENUM/CHECK/FK)で塞ぐ（アプリのバリデーションは経路しか守れない）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"アプリのバリデーションはアプリ経路しか守らない。直接 DB 入力(手動 SQL・別ツール・移行)はそれを迂回する。",
		"同じ不正 INSERT を、ガード無し表(ゆるい型・制約なし)には landing し、DB 制約(ENUM/CHECK/FK)有り表には",
		"入口で弾かれる。よって直接 DB 入力に対する最後の砦は DB 制約であり、アプリ側チェックだけでは足りない。",
		"※不正値を丸めずに弾くには STRICT sql_mode が要る(MySQL 8 の既定で入る)。",
	}, " "))

	const tenant = "ing-main"
	rec.Workload("bad_case_kinds", len(ingestlab.BadCases()))

	mode := ingestlab.SQLMode(ctx, db)
	strict := strings.Contains(mode, "STRICT")
	rec.Injection("sql_mode", mode).Injection("strict", strict)
	if !strict {
		// STRICT でないと ENUM/範囲が黙って丸められ、この実験の前提が崩れる。事実として記録する。
		t.Logf("警告: sql_mode に STRICT が無い（%s）。不正値が丸められる可能性がある", mode)
	}

	// ---- ① アプリ経路: AppValidate が不正を弾く（どちらの表にも届かない） ----
	appRejected := 0
	for _, c := range ingestlab.BadCases() {
		if ingestlab.AppValidate(c.Status, c.Amount, c.RefID) != nil {
			appRejected++
		}
	}
	rec.Add(expkit.Variant{
		Name:    "アプリ経路: 作成時バリデーションが不正を弾く",
		Metrics: map[string]float64{"rejected": float64(appRejected)},
		Notes:   []string{"アプリ経路では " + strconv.Itoa(appRejected) + "/" + strconv.Itoa(len(ingestlab.BadCases())) + " が弾かれる。ただしこれはアプリを通ったときだけ"},
	})

	// ---- ② 直接 DB 入力（アプリ迂回）: ガード無し表には landing する ----
	if err := ingestlab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}
	var openLanded int
	for i, c := range ingestlab.BadCases() {
		if err := ingestlab.DirectInsert(ctx, db, "ing_open", tenant, int64(i), c.Status, c.Amount, c.RefID); err == nil {
			openLanded++
		}
	}
	openTotal, err := ingestlab.Landed(ctx, db, "ing_open", tenant)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "直接DB入力: ガード無し表（poison が landing）",
		Accident: true,
		Metrics:  map[string]float64{"landed": float64(openLanded), "rows_in_table": float64(openTotal)},
		Notes:    []string{"アプリを迂回した不正 INSERT が " + strconv.Itoa(openLanded) + " 件そのまま残る＝後で worker を止める poison"},
	})

	// ---- ③ 直接 DB 入力（アプリ迂回）: DB 制約有り表は入口で弾く ----
	if err := ingestlab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}
	var guardedLanded int
	rejectedKinds := make([]string, 0, len(ingestlab.BadCases()))
	for i, c := range ingestlab.BadCases() {
		err := ingestlab.DirectInsert(ctx, db, "ing_guarded", tenant, int64(i), c.Status, c.Amount, c.RefID)
		if err == nil {
			guardedLanded++
		} else {
			rejectedKinds = append(rejectedKinds, c.Name)
		}
	}
	guardedTotal, err := ingestlab.Landed(ctx, db, "ing_guarded", tenant)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:    "直接DB入力: DB制約有り表（入口で弾く）",
		Metrics: map[string]float64{"landed": float64(guardedLanded), "rows_in_table": float64(guardedTotal)},
		Notes:   []string{"弾いた不正: " + strings.Join(rejectedKinds, " / ") + "（ENUM/CHECK/FK が経路に関係なく拒否）"},
	})
	t.Logf("直接DB入力: ガード無し landing=%d / ガード有り landing=%d（弾いた=%v）",
		openLanded, guardedLanded, rejectedKinds)

	// ---- 正しい行はガード有り表にもちゃんと入る（制約が正常系を壊さない） ----
	if err := ingestlab.DirectInsert(ctx, db, "ing_guarded", tenant, 100, "pending", 10, 1); err != nil {
		t.Errorf("正しい行がガード有り表に入らない（制約が厳しすぎる）: %v", err)
	}

	// ---- 検証 ----
	if openLanded == 0 {
		t.Errorf("ガード無し表に不正が1件も landing しない（実験前提が崩れている）: landed=%d", openLanded)
	}
	if strict && guardedLanded != 0 {
		t.Errorf("DB制約有り表に不正が landing した: landed=%d（0 のはず）弾いた=%v", guardedLanded, rejectedKinds)
	}
	if openLanded <= guardedLanded {
		t.Errorf("ガード有りがガード無しより多く弾けていない: open=%d guarded=%d", openLanded, guardedLanded)
	}

	rec.Scope(
		"MySQL 8.0 / 不正3種(未知状態・負値・存在しない参照)を直接 INSERT",
		"ing_open は VARCHAR/制約なし、ing_guarded は ENUM+CHECK(amount>=0)+FK(ref)",
		"STRICT sql_mode 前提（ENUM/範囲を丸めずに弾くため。MySQL 8 の既定）",
	)
	rec.Uncertain(
		"CHECK は MySQL 8.0.16+ で enforce（それ未満は無視される）",
		"STRICT が無い環境では ENUM が '' に、範囲外が丸められて landing しうる（この実験は STRICT 前提）",
		"入口をすり抜けた分（DB では表せない業務前提違反）は worker の読取り時防御＋EXP-45 の隔離で対処する（本実験外）",
		"FK はマルチテナントでは参照先もテナント込みで設計する（越境防止・本実験は単純化）",
	)
	rec.Artifact(
		"internal/ingestlab: AppValidate(アプリ経路) / DirectInsert(迂回) / ENUM+CHECK+FK の入口ガード",
		"docs/data-integrity-ingest.md: 不正データの入口を塞ぐ（作成時バリデーション＋DB制約＋防御読取り）",
	)
	rec.Next("EXP-45（すり抜けた poison を隔離してキューを止めない）と接続")

	files, err := rec.Save(strings.Join([]string{
		"不正データの入口は DB 制約(ENUM/CHECK/FK/NOT NULL)で塞ぐ。アプリのバリデーションはアプリ経路しか守らず、",
		"直接 DB 入力(手動 SQL・別ツール・移行)は迂回する。DB 制約だけが全経路の最後の砦。",
		"それでも DB で表せない業務前提違反は残るので、worker は読取り時にも検証し、EXP-45 で隔離してキューを止めない。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}
