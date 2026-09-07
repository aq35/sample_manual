package hubcachelab_test

// EXP-52: hub のキャッシュ破棄の粒度（テナント丸ごと vs 版で差分）。
//
//	MYSQL_DSN=... go test ./internal/hubcachelab/ -run TestEXP52 -v
//
// 1 hub に N 台。1回の poll で「テナント丸ごと引き直す」と毎回 N 行読む。「版(ver)で差分だけ」だと
// 変わった台数しか読まない。まばらな変更（毎 poll c 台）を M 回まわし、DB が読む総行数を比べる。
// 丸ごと ≒ N×M、差分 ≒ 変更総数。台数が増えるほど差が開く（メモリ・IO の実弾）。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/hubcachelab"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP52_hubキャッシュ破棄の粒度(t *testing.T) {
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
	if err := hubcachelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-52", "hub-cache-granularity",
		"1台変更時の再読を『丸ごと O(N)』と『版で差分 O(変更数)』で比べる")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) テナント丸ごと引き直すと、1回の poll で毎回 N 行読む → M 回で ≒ N×M 行。 " +
			"2) 版(ver)で差分だけ引くと、変わった台数しか読まない → M 回で ≒ 変更総数。 " +
			"3) 台数 N が増えるほど丸ごとの無駄が線形に増える。差分は N に依らず変更数で決まる。 " +
			"4) 購読者 S 人でも hub なら poll は共有（1本）。hub 無しだと S 倍に膨らむ（本実験は poll 側を測る）。")

	const tenant = "hubt"
	const n = 2000            // 1 hub の台数
	const cycles = 50         // poll 回数
	const changedPerCycle = 5 // 毎 poll で変わる台数（まばら）

	if _, err := hubcachelab.Seed(ctx, db, tenant, n); err != nil {
		t.Fatal(err)
	}

	// --- ① テナント丸ごと（粗い破棄）---
	wholeRows := 0
	for c := 0; c < cycles; c++ {
		got, err := hubcachelab.WholeSnapshot(ctx, db, tenant)
		if err != nil {
			t.Fatal(err)
		}
		wholeRows += got
	}

	// --- ② 版で差分（細かい破棄）---
	// 変更を起こしながら、差分だけ引く。lastVer は初期最大 ver(=n) から。
	var lastVer int64 = n
	deltaRows := 0
	totalChanges := 0
	nextRobot := 0
	for c := 0; c < cycles; c++ {
		ids := make([]int, 0, changedPerCycle)
		for k := 0; k < changedPerCycle; k++ {
			ids = append(ids, nextRobot%n)
			nextRobot++
		}
		newMax, err := hubcachelab.BumpRobots(ctx, db, tenant, ids, lastVer)
		if err != nil {
			t.Fatal(err)
		}
		lastVer0 := lastVer
		got, maxVer, err := hubcachelab.DeltaSince(ctx, db, tenant, lastVer0)
		if err != nil {
			t.Fatal(err)
		}
		deltaRows += got
		totalChanges += len(ids)
		lastVer = maxVer
		_ = newMax
	}

	amp := float64(wholeRows) / float64(deltaRows)
	// 読み込みバイトの目安（行幅 payload 200B 近辺）
	wholeBytesKB := float64(wholeRows) * hubcachelab.PayloadWidth / 1024.0
	deltaBytesKB := float64(deltaRows) * hubcachelab.PayloadWidth / 1024.0

	rec.Add(expkit.Variant{
		Name:     "テナント丸ごと引き直し（粗い破棄）: 毎 poll N 行",
		Accident: true,
		Counters: map[string]int64{"total_rows_read": int64(wholeRows), "per_poll": int64(n)},
		Metrics:  map[string]float64{"read_KB_目安": wholeBytesKB},
		Notes:    []string{"N=2000 × poll=50 = " + itoa(wholeRows) + " 行（変更が5台でも毎回2000行）"},
	})
	rec.Add(expkit.Variant{
		Name:     "版で差分だけ引く（細かい破棄）: 変更数しか読まない",
		Counters: map[string]int64{"total_rows_read": int64(deltaRows), "total_changes": int64(totalChanges)},
		Metrics:  map[string]float64{"read_KB_目安": deltaBytesKB, "reduction_x": amp},
		Notes:    []string{"読んだ行=変更総数 " + itoa(deltaRows) + "。丸ごと比 " + f1(amp) + "x 少ない"},
	})
	t.Logf("whole=%d rows (%.0fKB) / delta=%d rows (%.1fKB) / changes=%d / amp=%.1fx",
		wholeRows, wholeBytesKB, deltaRows, deltaBytesKB, totalChanges, amp)

	// ---- 検証 ----
	if wholeRows != n*cycles {
		t.Errorf("丸ごとが N×M でない: %d（%d のはず）", wholeRows, n*cycles)
	}
	if deltaRows != totalChanges {
		t.Errorf("差分が変更総数と一致しない: read=%d changes=%d", deltaRows, totalChanges)
	}
	if deltaRows >= wholeRows {
		t.Errorf("差分が丸ごとより少なくない: delta=%d whole=%d", deltaRows, wholeRows)
	}
	if amp < 10 { // N=2000, 変更 5/poll なら数百倍のはず
		t.Errorf("削減率が小さすぎる: %.1fx", amp)
	}

	rec.Scope(
		"MySQL 8.0 / 1テナント N=2000 台 / poll=50 回・変更 5 台/poll（まばら）/ payload ~200B",
		"読んだ行数 = 破棄の粗さの実弾。丸ごと=N×poll、差分=変更総数（版索引レンジ）",
		"バイトは行幅 200B の目安（実際はヘッダ・索引で上下）",
	)
	rec.Uncertain(
		"版(ver)はテナント内で単調増加する前提（書き込み時に採番・EXP-47 と同じ）",
		"差分は『変更が来た行だけ』。全台が毎 poll 変わるワークロードなら丸ごとと同じになる（まばらさが効く）",
		"購読者 S 人ぶんの膨張は hub で poll を共有すれば消える（EXP-38）。本実験は poll 1本ぶんの再読量",
		"payload が太い/off-page だと丸ごとの不利が更に増える（EXP-21）。差分は触る行が少なく有利",
	)
	rec.Artifact(
		"internal/hubcachelab: WholeSnapshot / DeltaSince（破棄粒度の測定）",
		"docs/cache.md / docs/subscription-design.md: hub のキャッシュ破棄",
	)
	rec.Next("EXP-53 長寿命接続の途中失権（re-authorization）")

	files, err := rec.Save(
		"1 hub に複数ロボットがいるとき、キャッシュ破棄は『テナント丸ごと再読 O(N)』でなく『版で差分 " +
			"O(変更数)』にする。まばらな変更では差分が数百倍少なく読む（N=2000・5台/poll で実測）。破棄の合図は" +
			"テナント単位の版 bump で粗くてよいが、再取得は ver>last の索引レンジで差分だけ。購読者が多くても" +
			"poll は hub で共有（S 倍にしない・EXP-38）。全台が毎回変わるなら丸ごとと同じ＝まばらさが効く。")
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
func f1(v float64) string {
	n := int64(v*10 + 0.5)
	return itoa(int(n/10)) + "." + itoa(int(n%10))
}
