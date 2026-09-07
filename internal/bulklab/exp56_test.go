package bulklab_test

// EXP-56: bulk INSERT の正規化（単発 vs tx 包み vs prepared vs multi-row）。
//
//	MYSQL_DSN=... go test ./internal/bulklab/ -run TestEXP56 -v
//
// 同じ N 行でも、やり方で往復数・コミット数が桁で変わる。autocommit 単発×N は N 往復＋N コミット
// （fsync）で最悪。1 tx に包むとコミット1回。multi-row（chunk 件を1文）は往復が N/chunk に。
// 件/秒で正規化し、どれだけ縮むかを測る。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/bulklab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP56_bulkInsertの正規化(t *testing.T) {
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
	if err := bulklab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-56", "bulk-insert",
		"往復数とコミット数で INSERT の件/秒が桁で変わる")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) autocommit 単発×N は N 往復＋N コミット(fsync) で最も遅い。 " +
			"2) 1 tx に包むとコミットが1回になり大幅に速い（往復は N のまま）。 " +
			"3) prepared 再利用はパースを省き tx 単発より速い。 " +
			"4) multi-row（chunk 件を1文）は往復が N/chunk になり最速。件/秒で数倍〜桁。")

	const tenant = "bulk"
	const n = 20_000
	const chunk = 500

	dSingle, err := bulklab.SingleAutocommit(ctx, db, tenant, n)
	if err != nil {
		t.Fatal(err)
	}
	dTx, err := bulklab.SingleInTx(ctx, db, tenant, n)
	if err != nil {
		t.Fatal(err)
	}
	dPrep, err := bulklab.PreparedInTx(ctx, db, tenant, n)
	if err != nil {
		t.Fatal(err)
	}
	dMulti, err := bulklab.MultiRow(ctx, db, tenant, n, chunk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := bulklab.Count(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	rps := func(d interface{ Seconds() float64 }) float64 { return float64(n) / d.Seconds() }
	add := func(name string, d interface {
		Milliseconds() int64
		Seconds() float64
		String() string
	}, roundtrips int, acc bool) {
		rec.Add(expkit.Variant{
			Name:     name,
			Accident: acc,
			Counters: map[string]int64{"ms": d.Milliseconds(), "roundtrips": int64(roundtrips), "rows_per_sec": int64(rps(d))},
			Notes:    []string{d.String() + " / " + itoa(roundtrips) + " 往復 / " + itoa(int(rps(d))) + " 件/秒"},
		})
	}
	add("autocommit 単発×N（N 往復・N コミット）", dSingle, n, true)
	add("1 tx に包む単発×N（N 往復・1 コミット）", dTx, n, false)
	add("prepared 再利用＋1 tx（パース1回）", dPrep, n, false)
	add("multi-row chunk=500（N/chunk 往復）", dMulti, n/chunk, false)
	t.Logf("single=%v tx=%v prep=%v multi=%v (rows=%d)", dSingle, dTx, dPrep, dMulti, got)

	// ---- 検証 ----
	if got != n {
		t.Errorf("行数が N でない: %d（%d のはず）", got, n)
	}
	if dTx >= dSingle {
		t.Errorf("tx 包みが autocommit 単発より速くない: tx=%v single=%v", dTx, dSingle)
	}
	if dMulti >= dTx {
		t.Errorf("multi-row が tx 単発より速くない: multi=%v tx=%v", dMulti, dTx)
	}
	if dMulti >= dSingle {
		t.Errorf("multi-row が autocommit 単発より速くない: multi=%v single=%v", dMulti, dSingle)
	}

	rec.Scope(
		"MySQL 8.0 InnoDB / N=20000・chunk=500 / 同ホスト（往復 ~0.1ms）/ payload ~30B",
		"件/秒 = N ÷ 所要秒。往復数はやり方で決まる（単発=N、multi-row=N/chunk）",
		"autocommit はデフォルトで各文が即コミット＝毎回 fsync（durability 設定で強弱）",
	)
	rec.Uncertain(
		"絶対値は同ホスト。本番は往復 RTT が効くほど単発の不利が拡大（multi-row/バッチの価値が増す）",
		"chunk は大きいほど往復が減るが、1文が巨大だと max_allowed_packet・ロック時間・メモリに注意（数百〜数千が実務的）",
		"更に速くするなら LOAD DATA INFILE（本実験外）。ただし運用・権限・エラー処理が変わる",
		"innodb_flush_log_at_trx_commit=2 等でコミットコストは下がるが durability が緩む（別軸）",
	)
	rec.Artifact(
		"internal/bulklab: 4 方式の INSERT 速度（往復・コミットの効き）",
		"docs/bulk-insert.md: bulk INSERT の正規化（往復とコミットを減らす）",
	)
	rec.Next("EXP-57 utf8mb4 と index 長")

	files, err := rec.Save(
		"同じ N 行でも INSERT のやり方で件/秒が桁で変わる。最悪は autocommit 単発×N（N 往復＋N コミット" +
			"＝毎回 fsync）。1 tx に包むとコミットが1回になり大幅に速い。prepared 再利用でパースも省ける。" +
			"最速は multi-row（chunk 件を1文＝往復が N/chunk）。まとめられる書き込みは multi-row＋1 tx が基本。" +
			"chunk は max_allowed_packet とロック時間を見て数百〜数千に。往復 RTT が大きい本番ほど効く。")
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
