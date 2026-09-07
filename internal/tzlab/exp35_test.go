package tzlab_test

// EXP-35: タイムゾーン/DST のスケジューリング。
//
//	MYSQL_DSN=... go test ./internal/tzlab/ -run TestEXP35 -v
//
// (1) 現地時刻の予定は夏時間切替で「消える/二重の」時刻に当たる。(2) MySQL の DATETIME と
// TIMESTAMP はセッション tz で挙動が違う。両方を実際に見て、UTC 保存の指針を確かめる。

import (
	"context"
	"database/sql"
	"testing"
	"time"
	_ "time/tzdata" // tz DB を埋め込む（コンテナに zoneinfo が無くても LoadLocation が効く）

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/tzlab"
)

func TestEXP35_タイムゾーンとDST(t *testing.T) {
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
	if err := tzlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-35", "timezone-dst",
		"DST の現地時刻予定の罠と、DATETIME/TIMESTAMP のセッション tz 挙動")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 春の切替日、現地 02:30 は存在しない（02:00→03:00）。素朴に time.Date すると別の時刻にずれる" +
			"（Go は 01:30 に正規化した。方向は実装依存だが 02:30 は保てない）。 " +
			"2) 秋の切替日、現地 01:30 は二重に起きる。UTC で見ると1時間違う2つの瞬間になる。 " +
			"3) DATETIME はセッション tz を変えても値が変わらない。TIMESTAMP は変わる（UTC 保存のため）。 " +
			"4) だから予定・実績は UTC 保存し、現地の繰返し予定は tz 対応で gap/二重を明示的に解決する。")

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	// ---- ① 春: 02:30 は存在せず、素朴 time.Date は 03:30 にずれる（2026-03-08）----
	spring := time.Date(2026, 3, 8, 2, 30, 0, 0, ny)
	rec.Add(expkit.Variant{
		Name:     "春の切替: 現地 02:30 は存在せず、素朴 time.Date が別の時刻にずらす",
		Accident: true,
		Counters: map[string]int64{"wanted_hour": 2, "got_hour": int64(spring.Hour())},
		Notes:    []string{"time.Date(2026-03-08 02:30 NY) = " + spring.Format("15:04 MST") + "（02:30 の予定が別の時刻に。02:00–03:00 は存在しない）"},
	})
	t.Logf("春: 欲しい 02:30 → 実際 %s (hour=%d)", spring.Format("15:04 MST"), spring.Hour())

	// ---- ② 秋: 01:30 は二重（2026-11-01）。UTC で1時間違う2つの瞬間 ----
	fallEarly := time.Date(2026, 11, 1, 1, 30, 0, 0, ny)            // 最初の 01:30（EDT）
	fallLate := fallEarly.Add(time.Hour)                            // その1時間後も現地表示は 01:30（EST）
	sameWall := fallLate.In(ny).Format("15:04") == fallEarly.Format("15:04")
	utcGap := fallLate.UTC().Sub(fallEarly.UTC())
	rec.Add(expkit.Variant{
		Name:     "秋の切替: 現地 01:30 が二重（UTC では1時間違う2つ）",
		Accident: true,
		Counters: map[string]int64{"same_wallclock": b2i(sameWall), "utc_gap_hours": int64(utcGap.Hours())},
		Notes:    []string{"2つの 01:30 は UTC で " + fallEarly.UTC().Format("15:04") + " と " + fallLate.UTC().Format("15:04") + "（1時間差）"},
	})
	t.Logf("秋: 01:30 が二重 same_wall=%v utc_gap=%v", sameWall, utcGap)

	// ---- ③ DATETIME vs TIMESTAMP: セッション tz を変えて読む ----
	const lit = "2026-06-01 12:00:00"
	if err := tzlab.StoreAt(ctx, db, "+00:00", 1, lit); err != nil {
		t.Fatal(err)
	}
	dtJST, tsJST, err := tzlab.ReadAt(ctx, db, "+09:00", 1) // 別 tz で読む
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:    "DATETIME/TIMESTAMP: +00:00 で入れ +09:00 で読む",
		Metrics: map[string]float64{},
		Notes: []string{
			"入れた値: " + lit + "（tz +00:00）",
			"DATETIME 読み(+09:00): " + dtJST + "（変わらない＝tz 無視の literal）",
			"TIMESTAMP 読み(+09:00): " + tsJST + "（+9h ずれる＝UTC 保存で変換）",
		},
	})
	t.Logf("DATETIME=%s / TIMESTAMP=%s（+00:00 で保存, +09:00 で読み）", dtJST, tsJST)

	// ---- 検証 ----
	if spring.Hour() == 2 {
		t.Errorf("春の gap が再現していない: 02:30 がそのまま 02 時で返った（gap でずれるはず）")
	}
	if !sameWall || int(utcGap.Hours()) != 1 {
		t.Errorf("秋の二重が再現していない: same_wall=%v gap=%v", sameWall, utcGap)
	}
	if dtJST != lit {
		t.Errorf("DATETIME がセッション tz で変わった: %s（不変のはず）", dtJST)
	}
	if tsJST == lit {
		t.Errorf("TIMESTAMP がセッション tz で変わらない: %s（+9h ずれるはず）", tsJST)
	}

	rec.Scope(
		"Go time + 埋め込み tzdata（America/New_York）/ MySQL の DATETIME・TIMESTAMP をセッション tz +00:00/+09:00 で",
		"春 2026-03-08・秋 2026-11-01 の US 切替日を使用",
		"数値オフセット（+00:00/+09:00）を使い、名前付き tz テーブルの有無に依存しない",
	)
	rec.Uncertain(
		"実アプリの tz は運用地域による。ここは US で代表",
		"MySQL の named time zone（'America/New_York'）は tz テーブル投入が要る。数値オフセットは常に可",
		"go-sql-driver の loc/parseTime とセッション tz の相互作用は設定依存（ここは DATE_FORMAT で表示値を比較）",
	)
	rec.Artifact(
		"internal/tzlab: DST の gap/二重の再現と DATETIME/TIMESTAMP のセッション tz 挙動",
		"docs/timezone.md: 時刻の保存と DST スケジューリングの指針",
	)
	rec.Next("EXP-36 context キャンセルでクエリが止まるか")

	files, err := rec.Save(
		"予定・実績は UTC で保存し（epoch か、UTC 固定運用の DATETIME/TIMESTAMP を一貫して）、" +
			"スケジュール計算も基本 UTC で行う。『毎日 02:30 現地』のような現地繰返し予定だけ、tz 対応の計算で" +
			"存在しない時刻(春)・二重の時刻(秋)を明示的に解決する（素朴な time.Date 任せにしない）。" +
			"DATETIME はセッション tz で変わらず、TIMESTAMP は変わる——混在させず方針を1つに。")
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
