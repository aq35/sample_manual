package cancellab_test

// EXP-36: context キャンセルでクエリが本当に止まるか。
//
//	MYSQL_DSN=... go test ./internal/cancellab/ -run TestEXP36 -v
//
// タイムアウトつき context でクエリを投げたとき、(1) 呼び出しは timeout で即戻るか、
// (2) DB 側のクエリも止まって接続を握り続けないか、を実測。サーバ側の保険 MAX_EXECUTION_TIME も。

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/cancellab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP36_contextキャンセル(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)
	admin, err := cancellab.Open(dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	db, err := cancellab.Open(dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	rec := expkit.NewRecorder("EXP-36", "context-cancel-kills-query",
		"context タイムアウトで呼び出しが即戻り、DB 側のクエリも止まるか")
	rec.Env(expkit.CaptureEnv(ctx, admin))
	rec.Freeze(
		"1) timeout つき context で SELECT SLEEP(3) を投げると、呼び出しは約 timeout で戻る（3秒待たない）。 " +
			"2) その後、DB 側に SLEEP を実行中の幽霊スレッドが残らない（接続を握り続けない）。 " +
			"3) timeout 無しだとフルに待つ（対照）。 " +
			"4) サーバ側の MAX_EXECUTION_TIME でも上限をかけられる（クライアント任せにしない保険）。")

	// ---- ① timeout つき: 即戻る ----
	elapsed, errTO := cancellab.SleepWithTimeout(db, 3, 300*time.Millisecond)
	time.Sleep(600 * time.Millisecond) // driver が接続を閉じ、サーバがクエリを打ち切る猶予
	linger, _ := cancellab.LingeringSleeps(ctx, admin)
	rec.Add(expkit.Variant{
		Name:     "timeout つき SELECT SLEEP(3)（300ms）→ 即戻る・幽霊なし",
		Metrics:  map[string]float64{"elapsed_ms": msf(elapsed)},
		Counters: map[string]int64{"timed_out": okI(errTO != nil), "lingering_sleeps": int64(linger)},
		Notes:    []string{"所要 " + elapsed.String() + " / error: " + errStr(errTO) + " / 幽霊 SLEEP=" + itoa(linger)},
	})
	t.Logf("timeout: elapsed=%v err=%v linger=%d", elapsed, errTO, linger)

	// ---- ② 対照: timeout 無し → フルに待つ ----
	full, errFull := cancellab.SleepNoTimeout(ctx, db, 1)
	rec.Add(expkit.Variant{
		Name:    "対照: timeout 無し SELECT SLEEP(1) → フルに待つ",
		Metrics: map[string]float64{"elapsed_ms": msf(full)},
		Notes:   []string{"所要 " + full.String() + "（1秒待つ）"},
	})
	t.Logf("no-timeout: elapsed=%v err=%v", full, errFull)

	// ---- ③ サーバ側 MAX_EXECUTION_TIME ----
	me, errME := cancellab.SleepMaxExecTime(ctx, db, 3, 300)
	rec.Add(expkit.Variant{
		Name:     "サーバ側 MAX_EXECUTION_TIME(300ms) → 実行時間が上限で頭打ち",
		Accident: true,
		Metrics:  map[string]float64{"elapsed_ms": msf(me)},
		Notes: []string{"所要 " + me.String() + "（3秒 SLEEP が ~300ms で打ち切られた）",
			"※SLEEP は打ち切り時 error を返さず値を返す MySQL の癖。実データを走査する SELECT なら 3024 で error になる"},
	})
	t.Logf("max-exec: elapsed=%v err=%v", me, errME)

	// ---- 検証 ----
	if errTO == nil {
		t.Errorf("timeout つきなのにエラーが出ない")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("timeout で即戻っていない（3秒待った？）: %v", elapsed)
	}
	if linger != 0 {
		t.Errorf("キャンセル後も DB 側で SLEEP が残っている（接続を握り続ける）: %d", linger)
	}
	if full < 900*time.Millisecond {
		t.Errorf("対照(timeout無し)がフルに待っていない: %v", full)
	}
	if me > 1500*time.Millisecond { // 3秒 SLEEP が ~300ms で頭打ちになっていること（SLEEP は error を返さない癖あり）
		t.Errorf("MAX_EXECUTION_TIME で実行時間が頭打ちになっていない: elapsed=%v", me)
	}

	rec.Scope(
		"MySQL 8.0 / go-sql-driver は ctx キャンセルで実行中クエリを打ち切る / 幽霊は processlist で確認",
		"MAX_EXECUTION_TIME は読み取り専用 SELECT にだけ効く（書き込みには別の仕組み）",
		"所要のしきい値は緩め（CI の揺れを許容）。要点は『フルに待たない・幽霊が残らない』",
	)
	rec.Uncertain(
		"driver がキャンセル時に接続を閉じるか KILL するかは版依存。結果（即戻る・幽霊なし）は同じ",
		"書き込みクエリのタイムアウトは MAX_EXECUTION_TIME では止まらない（lock_wait_timeout 等で）",
		"ネットワーク断とアプリのタイムアウトは別。ここはアプリ起点のキャンセル",
	)
	rec.Artifact(
		"internal/cancellab: ctx タイムアウトの即戻り・幽霊クエリ検査・MAX_EXECUTION_TIME",
		"docs/query-timeout.md: クエリのタイムアウトとキャンセル伝播",
	)
	rec.Next("EXP-37 可観測性（メトリクス/カーディナリティ）")

	files, err := rec.Save(
		"DB 呼び出しには必ず timeout つき context を渡す。go-sql-driver は ctx キャンセルで実行中クエリを" +
			"打ち切り、呼び出しは即戻り、DB 側に幽霊クエリを残さない（接続を握り続けない＝プール枯れを防ぐ）。" +
			"クライアント任せにしない保険として、読み取りには MAX_EXECUTION_TIME をサーバ側にも掛ける。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
func okI(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
func errStr(err error) string {
	if err == nil {
		return "(なし)"
	}
	return err.Error()
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
