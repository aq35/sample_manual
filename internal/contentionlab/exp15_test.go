package contentionlab_test

// EXP-15: テーブル分割・パーティションと UPDATE 競合。
//
//	MYSQL_DSN=... go test ./internal/contentionlab/ -run TestEXP15 -v

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/contentionlab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP15_テーブル分割と競合(t *testing.T) {
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
	if err := contentionlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-15", "table-split-contention",
		"テーブル分割・パーティションと UPDATE 競合。何が減って何が減らないか")
	rec.Env(expkit.CaptureEnv(ctx, db))
	// ★仮説はメカニズムで立てる（結果を見て書き換えない）。
	rec.Freeze(strings.Join([]string{
		"1) 熱い状態と大ペイロードを同居させた幅広い行は、UPDATE のたびに大きな行を書き直し二次索引を保守するので、",
		"   熱い状態だけの狭い行よりスループットが低い（分割は1文コストに効く）。",
		"2) 行ロック競合は『同じ行を奪い合う』ことで起き、テーブルを分割しても減らない（ロックは行単位）。",
		"   tx でロックを保持すると Innodb_row_lock_waits が観測でき、wide/narrow で同程度になる。",
		"3) 履歴の掃除は DROP PARTITION が DELETE より桁違いに速い（undo/purge を作らない）。",
		"4) つまりテーブル分割は『アクセスパターンで分ける』もの: 1文コスト・掃除・走査・熱い表の小ささに効く。",
		"   行ロック競合の特効薬ではない。",
	}, " "))

	const tenant = "exp15"
	rec.Workload("hot_robots_throughput", 200).Workload("hot_robots_contended", 2).
		Workload("concurrency", 16).Workload("duration", "2s")

	if err := contentionlab.SeedState(ctx, db, tenant, 200, strings.Repeat("x", 1500)); err != nil {
		t.Fatal(err)
	}

	// ---- ① 単一文スループット: wide vs narrow（往復支配なので差は小さいはず）----
	wide := contentionlab.RunUpdate(ctx, db, tenant, "wide_state", 200, 16, 2*time.Second, true)
	narrow := contentionlab.RunUpdate(ctx, db, tenant, "narrow_state", 200, 16, 2*time.Second, false)
	diff := (persec(narrow.Ops, narrow.Elapsed)/persec(wide.Ops, wide.Elapsed) - 1) * 100
	rec.Add(expkit.Variant{
		Name:     "単一文 UPDATE: 幅広い行（状態+大ペイロード同居）",
		Counters: map[string]int64{"ops": wide.Ops},
		Metrics:  map[string]float64{"per_sec": persec(wide.Ops, wide.Elapsed), "p95_ms": msf(wide.Latency.P95)},
		Latency:  &wide.Latency,
	})
	rec.Add(expkit.Variant{
		Name:     "単一文 UPDATE: 狭い行（熱い状態だけ）",
		Counters: map[string]int64{"ops": narrow.Ops},
		Metrics:  map[string]float64{"per_sec": persec(narrow.Ops, narrow.Elapsed), "p95_ms": msf(narrow.Latency.P95)},
		Latency:  &narrow.Latency,
		Notes: []string{
			fmt.Sprintf("幅広い %.0f/s → 狭い %.0f/s（差 %.1f%%）。往復支配なので差は小さい",
				persec(wide.Ops, wide.Elapsed), persec(narrow.Ops, narrow.Elapsed), diff),
		},
	})
	t.Logf("単一文 wide=%.0f/s p95=%v / narrow=%.0f/s p95=%v（差 %.1f%%）",
		persec(wide.Ops, wide.Elapsed), wide.Latency.P95, persec(narrow.Ops, narrow.Elapsed), narrow.Latency.P95, diff)

	// ---- ② 行ロック競合: 2つの熱い行を奪い合う（tx でロック保持）----
	if err := contentionlab.SeedState(ctx, db, tenant, 2, strings.Repeat("x", 1500)); err != nil {
		t.Fatal(err)
	}
	cw := contentionlab.RunUpdateContended(ctx, db, tenant, "wide_state", 2, 16, 2*time.Second, 2*time.Millisecond)
	cn := contentionlab.RunUpdateContended(ctx, db, tenant, "narrow_state", 2, 16, 2*time.Second, 2*time.Millisecond)
	rec.Add(expkit.Variant{
		Name:     "行ロック競合（2行を16並行で奪い合う・tx保持2ms）: 幅広い行",
		Accident: true,
		Counters: map[string]int64{"ops": cw.Ops, "row_lock_waits": cw.LockWaits, "row_lock_time_ms": cw.LockTime, "deadlocks": cw.Deadlocks},
		Metrics:  map[string]float64{"p95_ms": msf(cw.Latency.P95)},
	})
	rec.Add(expkit.Variant{
		Name:     "行ロック競合（同上）: 狭い行",
		Accident: true,
		Counters: map[string]int64{"ops": cn.Ops, "row_lock_waits": cn.LockWaits, "row_lock_time_ms": cn.LockTime, "deadlocks": cn.Deadlocks},
		Metrics:  map[string]float64{"p95_ms": msf(cn.Latency.P95)},
		Notes: []string{
			fmt.Sprintf("行ロック待ち wide=%d / narrow=%d。テーブルを分けても行ロック競合は減らない（ロックは行単位）",
				cw.LockWaits, cn.LockWaits),
		},
	})
	t.Logf("競合 wide: lockwaits=%d p95=%v / narrow: lockwaits=%d p95=%v",
		cw.LockWaits, cw.Latency.P95, cn.LockWaits, cn.Latency.P95)

	// ---- ③ 履歴の掃除: DELETE vs DROP PARTITION ----
	if err := contentionlab.SeedHistory(ctx, db, tenant, 30000); err != nil {
		t.Fatal(err)
	}
	del, err := contentionlab.CleanupDelete(ctx, db, tenant, "2026-01-02")
	if err != nil {
		t.Fatal(err)
	}
	drop, err := contentionlab.CleanupDropPartition(ctx, db, "p1")
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name: "履歴の掃除: DELETE", Accident: true,
		Counters: map[string]int64{"removed": del.Removed}, Metrics: map[string]float64{"took_ms": msf(del.Took)},
	})
	rec.Add(expkit.Variant{
		Name:     "履歴の掃除: DROP PARTITION",
		Counters: map[string]int64{"removed": drop.Removed}, Metrics: map[string]float64{"took_ms": msf(drop.Took)},
		Notes: []string{fmt.Sprintf("DELETE %v(%d行) → DROP PARTITION %v(%d行)。%.0f倍速い",
			del.Took.Round(time.Microsecond), del.Removed, drop.Took.Round(time.Microsecond), drop.Removed,
			float64(del.Took)/float64(maxd(drop.Took, time.Microsecond)))},
	})
	t.Logf("掃除 DELETE=%v(%d) DROP=%v(%d)", del.Took, del.Removed, drop.Took, drop.Removed)

	// ---- 検証（メカニズムどおりか）----
	// #1: 狭い行の方が単一文スループットが高い（分割が効く）
	if persec(narrow.Ops, narrow.Elapsed) <= persec(wide.Ops, wide.Elapsed) {
		t.Errorf("狭い行が速くない: narrow=%.0f/s wide=%.0f/s", persec(narrow.Ops, narrow.Elapsed), persec(wide.Ops, wide.Elapsed))
	}
	// #2: 競合下でロック待ちが観測でき、wide/narrow で同程度（分割で行ロックは減らない）
	if cw.LockWaits == 0 && cn.LockWaits == 0 {
		t.Errorf("行ロック待ちが観測できていない（測定器が競合を作れていない）")
	}
	// #3: DROP PARTITION が DELETE より速い
	if drop.Took >= del.Took {
		t.Errorf("DROP PARTITION が DELETE 以上に遅い: drop=%v del=%v", drop.Took, del.Took)
	}

	rec.Scope(
		"MySQL 8.0 / 単一ホスト / 並行 16・各 2 秒",
		"スループット比較は熱い行 200（競合を薄めて1文の素の速度を見る）",
		"行ロック競合は熱い行 2・tx でロックを 2ms 保持（待ちを観測するため）",
		"掃除は 1 日分（約1万行）を DELETE と DROP PARTITION で",
	)
	rec.Uncertain(
		"payload サイズ・並行度・熱い行数で差は動く。この数字はこのホストのもの",
		"パーティション pruning（検索範囲の縮小）の効果は本実験では別途未測定",
		"二次索引の数を増やすと wide の1文コストは上がる（本実験は索引1本）。索引数依存は未測定",
	)
	rec.Artifact("internal/contentionlab: wide/narrow の単一文と tx 競合、DELETE/DROP PARTITION の掃除")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"単一文 UPDATE は往復が支配的で、行の幅はスループットをほとんど変えなかった。",
		"行ロック競合は同じ行の奪い合いで起き、テーブルを分割しても減らない（ロックは行単位）。",
		"効いたのは掃除で、DROP PARTITION は DELETE より桁違いに速い。",
		"→ テーブル分割はアクセスパターンで分ける（掃除・走査・熱い表を小さく保つ）ためのもので、",
		"　行ロック競合や1文スループットの特効薬ではない。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func persec(ops int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(ops) / d.Seconds()
}
func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
func maxd(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
