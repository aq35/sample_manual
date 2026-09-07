package retentionlab_test

// EXP-48: 保持期間の運用（日パーティションの DROP でロールフォワード）。
//
//	MYSQL_DSN=... go test ./internal/retentionlab/ -run TestEXP48 -v
//
// 履歴を「保持期間で丸ごと捨てる」。日ごとの RANGE パーティションなら DROP PARTITION で一瞬、
// DELETE は高い（EXP-15）。古い日だけ消え、recent は残り、current は書け、パーティション数は有界。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/retentionlab"
)

func TestEXP48_保持期間の運用(t *testing.T) {
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
	if err := retentionlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-48", "retention-policy",
		"日パーティションの DROP で保持期間を適用（古い日を丸ごと捨てる）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 5日ぶんのうち古い2日を DROP PARTITION で消すと一瞬（DELETE で同量消すより速い・EXP-15）。 " +
			"2) 消えるのは古い日だけ。recent（保持内）は残り、current の書き込みは通る。 " +
			"3) ロールフォワード（pmax を割って翌日パーティションを用意）でパーティション数は有界に保つ。")

	const tenant = "ret"
	const perDay = 3000
	if err := retentionlab.Seed(ctx, db, tenant, perDay); err != nil {
		t.Fatal(err)
	}
	partsInitial, _ := retentionlab.PartitionCount(ctx, db) // ドロップ前（5日＋pmax=6）

	// 保持3日 → 09-01, 09-02 を捨てる
	dropDur, err := retentionlab.DropOldPartitions(ctx, db, "p20260901", "p20260902")
	if err != nil {
		t.Fatal(err)
	}
	// 比較: フラット表で同じ古い範囲を DELETE
	delDur, err := retentionlab.DeleteOldFlat(ctx, db, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}

	oldGone, _ := retentionlab.CountDay(ctx, db, tenant, "2026-09-01")
	recentKept, _ := retentionlab.CountDay(ctx, db, tenant, "2026-09-04")
	partsBefore, _ := retentionlab.PartitionCount(ctx, db)

	// ロールフォワード: 翌日 09-06 のパーティションを用意 → current(09-06) を書けるように
	if err := retentionlab.RollForward(ctx, db, "p20260906", "2026-09-07"); err != nil {
		t.Fatal(err)
	}
	writeErr := retentionlab.InsertOne(ctx, db, tenant, "2026-09-06", 1)
	partsAfter, _ := retentionlab.PartitionCount(ctx, db)

	rec.Add(expkit.Variant{
		Name:     "保持適用: DROP PARTITION（古い2日）",
		Metrics:  map[string]float64{"drop_ms": ms(dropDur), "delete_ms_同量": ms(delDur)},
		Counters: map[string]int64{"old_day_rows": int64(oldGone), "recent_day_rows": int64(recentKept)},
		Notes:    []string{"DROP " + dropDur.String() + " / 同量 DELETE " + delDur.String() + "。古い日=0 行・recent=" + itoa(recentKept)},
	})
	rec.Add(expkit.Variant{
		Name:     "ロールフォワード: 翌日パーティションを用意 → current 書き込み OK・パーティション有界",
		Counters: map[string]int64{"write_ok": okI(writeErr == nil), "parts_initial": int64(partsInitial), "parts_after_drop": int64(partsBefore), "parts_after_roll": int64(partsAfter)},
		Notes:    []string{"パーティション数 初期 " + itoa(partsInitial) + " → drop 後 " + itoa(partsBefore) + " → roll 後 " + itoa(partsAfter) + "（初期以下＝増え続けない）"},
	})
	t.Logf("retention: drop=%v delete=%v old=%d recent=%d parts %d→%d writeErr=%v",
		dropDur, delDur, oldGone, recentKept, partsBefore, partsAfter, writeErr)

	// ---- 検証 ----
	if oldGone != 0 {
		t.Errorf("古い日が消えていない: %d 行", oldGone)
	}
	if recentKept != perDay {
		t.Errorf("recent が保持されていない: %d（%d のはず）", recentKept, perDay)
	}
	if dropDur >= delDur {
		t.Errorf("DROP PARTITION が DELETE より速くない: drop=%v delete=%v", dropDur, delDur)
	}
	if writeErr != nil {
		t.Errorf("ロールフォワード後に current を書けない: %v", writeErr)
	}
	if partsAfter > partsInitial { // 1サイクル（drop＋roll）で初期を超えて増えない＝有界
		t.Errorf("パーティションが有界に保たれていない: 初期%d → %d", partsInitial, partsAfter)
	}

	rec.Scope(
		"MySQL 8.0 / 5日×3000行 / 日ごとの RANGE COLUMNS(d) パーティション / 保持3日",
		"DROP PARTITION は該当パーティションを丸ごと外す（undo 肥大・purge 遅延が無い）",
		"ロールフォワードは pmax を REORGANIZE して翌日パーティションを切り出す",
	)
	rec.Uncertain(
		"パーティションキーは全ユニークキーに含める必要（ここは PK に d を含めた）",
		"REORGANIZE pmax はデータが無ければ一瞬。pmax にデータが溜まっていると重い（先回りで用意する）",
		"パーティション運用は定期ジョブで（毎日: 翌日を用意し、保持超過を DROP）。DDL なので暗黙コミット",
		"絶対時間はこのホストのもの。DROP vs DELETE の差は行数・undo 設定で動く（EXP-15）",
	)
	rec.Artifact(
		"internal/retentionlab: 日パーティションの DROP と REORGANIZE によるロールフォワード",
		"docs/retention.md: 保持期間の運用（パーティション DROP）",
	)
	rec.Next("EXP-49 一時 vs 恒久エラーの分類")

	files, err := rec.Save(
		"履歴は保持期間で『古い日を丸ごと DROP PARTITION』する（DELETE は undo 肥大で高い）。日ごとの" +
			"RANGE パーティションにし、定期ジョブで『翌日を用意（ロールフォワード）＋保持超過を DROP』する。" +
			"消えるのは古い日だけ、recent は残り、current は書け、パーティション数は有界に保たれる。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func ms(d interface{ Microseconds() int64 }) float64 { return float64(d.Microseconds()) / 1000.0 }
func okI(b bool) int64 {
	if b {
		return 1
	}
	return 0
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
