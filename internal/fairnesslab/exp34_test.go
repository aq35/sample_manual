package fairnesslab_test

// EXP-34: ノイジーネイバー（テナント公平性）。1テナントの大量投入が他を巻き込むか。
//
//	MYSQL_DSN=... go test ./internal/fairnesslab/ -run TestEXP34 -v
//
// hog が先に大量投入したキューを、到着順(unfair) と テナント round-robin(fair) で捌き、
// victim の命令が「前に何件処理された後で」捌かれるか（＝待たされ具合）を比べる。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/fairnesslab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP34_テナント公平性(t *testing.T) {
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
	if err := fairnesslab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-34", "tenant-fairness",
		"1テナントの大量投入が他テナントの dispatch を待たせるか（公平スケジューリング）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 到着順に捌くと、先に大量投入した hog の後ろで victim が待たされる（待ち位置 ≒ hog の件数）。 " +
			"2) テナント round-robin で捌くと、victim は hog の件数に関係なく早い周で捌ける（待ち位置 ≒ テナント数）。 " +
			"3) 総処理量は同じ。変わるのは『順序』＝ victim の待ち。")

	const hogN, victims = 2000, 5
	if err := fairnesslab.Seed(ctx, db, hogN, victims); err != nil {
		t.Fatal(err)
	}
	items, err := fairnesslab.LoadOrdered(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	vnames := fairnesslab.VictimNames(victims)
	rec.Workload("hog_items", hogN).Workload("victim_tenants", victims).Workload("total_items", len(items))

	unfairWorst := fairnesslab.WorstVictimPosition(fairnesslab.Unfair(items), vnames)
	fairWorst := fairnesslab.WorstVictimPosition(fairnesslab.Fair(items), vnames)

	rec.Add(expkit.Variant{
		Name:     "到着順に捌く（unfair）: victim は hog の後ろで待つ",
		Accident: true,
		Counters: map[string]int64{"victim_worst_position": int64(unfairWorst)},
		Notes:    []string{"victim の最悪待ち位置 ≒ hog の件数（" + itoa(int64(unfairWorst)) + " 件処理後）"},
	})
	rec.Add(expkit.Variant{
		Name:     "テナント round-robin（fair）: victim は hog の量に関係なくすぐ",
		Counters: map[string]int64{"victim_worst_position": int64(fairWorst)},
		Notes:    []string{"unfair " + itoa(int64(unfairWorst)) + " → fair " + itoa(int64(fairWorst)) + "（テナント数ぶんで捌ける）"},
	})
	t.Logf("victim 最悪待ち位置: unfair=%d / fair=%d（hog=%d victims=%d）",
		unfairWorst, fairWorst, hogN, victims)

	// ---- 検証 ----
	if unfairWorst < hogN { // victim は hog 全部の後（到着が後なので）
		t.Errorf("unfair の待ちが小さすぎる: %d（hog=%d の後のはず）", unfairWorst, hogN)
	}
	if fairWorst > victims*2 { // 高々テナント数ぶん
		t.Errorf("fair でも victim が待たされている: %d（テナント数ぶんのはず）", fairWorst)
	}
	if fairWorst*10 > unfairWorst { // 桁違いに改善しているはず
		t.Errorf("fair が unfair を明確に改善していない: unfair=%d fair=%d", unfairWorst, fairWorst)
	}

	rec.Scope(
		"MySQL 8.0 / hog 2000件（先着）・victim 5テナント各1件（後着）/ キューは DB、順序を比較",
		"待ち位置 = その命令が捌かれる前に処理された件数。待ち時間 ≒ 位置 × 1件の処理時間",
		"round-robin は1周に各テナント1件。テナントの順は初出順",
	)
	rec.Uncertain(
		"実際の待ち時間は 1件の処理時間に依る（位置はその比例係数）",
		"round-robin は単純な公平化。重み付き（テナントのプラン別）や、hog に上限をかける方式もある",
		"複数ワーカーで担当を分ける（lease・EXP-2/14）と、テナントを別ワーカーに散らして更に緩和できる",
		"飢餓を完全に防ぐには、pending が古すぎる命令を優先する等の劣化対策も要る",
	)
	rec.Artifact(
		"internal/fairnesslab: 到着順 vs テナント round-robin の dispatch 順序",
		"docs/tenant-fairness.md: ノイジーネイバー対策（公平スケジューリング）",
	)
	rec.Next("なし（EXP-32..34 上位バッチ完了）")

	files, err := rec.Save(
		"到着順に捌くと、1テナントの大量投入で他テナントが hog の件数ぶん待たされる。" +
			"テナントを round-robin で回せば、victim は hog の量に関係なくテナント数ぶんで捌ける。" +
			"総処理量は同じで、変えるのは『順序』。担当をワーカーで分ける（lease）と更に緩和できる。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
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
