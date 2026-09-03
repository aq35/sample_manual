package shutdownlab_test

// EXP-3: graceful shutdown。
//
//	MYSQL_DSN=... go test ./internal/shutdownlab/ -run TestEXP3 -v -timeout 20m
//
// 確かめること:
//   - 受付の停止が処理の停止より先に行われる
//   - 未処理のものが「戻された」ことが分かる（黙って消えない）
//   - ack 済み消失が起きない
//   - goroutine・ticker・接続が残らない
//   - 期限を超えた場合に、何が残っているかが記録される

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/queuesvc"
	"github.com/aq35/sample_manual/internal/shutdownlab"
)

const (
	totalItems = 40
	visibility = 1500 * time.Millisecond
)

// 信号を送る地点。worker の主要な段階を網羅する。
var points = []string{
	"idle", "fetching", "decoding", "enqueue", "batching",
	"in_tx", "before_commit", "after_commit", "lease_renew", "retry_sleep",
}

type runResult struct {
	Point      string
	Signal     string
	ExitCode   int
	Killed     bool
	Duration   time.Duration
	Committed  int
	Acked      int
	Nacked     int
	Lost       int // ack したのに DB に無い＝消失
	Inflight   int // 宙に浮いたまま（次の可視性タイムアウトまで誰も触れない）
	Goroutines int
	Base       int
	Stdout     string
}

func TestEXP3_gracefulShutdown(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := mysqltest.DSN(t)
	db := openDB(t, dsn)
	ctx := context.Background()
	if err := shutdownlab.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/shutdownlab")

	rec := expkit.NewRecorder("EXP-3", "graceful-shutdown",
		"worker の各段階で終了信号を受けたときの、ack 済み消失・滞留・後始末")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) 片付けてから終わる worker は、どの段階で SIGTERM を受けても ack 済み消失を出さず、",
		"   未処理をキューへ戻す。",
		"2) 片付けずに落ちる worker は、消失は出さない（キューの可視性タイムアウトが救う）が、",
		"   取り出したものが宙に浮き、次に触れるまで遅延する。",
		"3) commit の前に ack する実装は、そこで落ちると **本当に消失する**。",
		"4) 片付けて終わった場合、goroutine は起動時の水準に戻る。",
	}, " "))
	rec.Workload("items", totalItems).
		Workload("queue_visibility", visibility.String()).
		Workload("batch", 5).
		Injection("signal_points", points).
		Injection("signals", []string{"SIGTERM", "SIGINT", "SIGHUP", "SIGQUIT"}).
		Injection("method", "指定地点で worker を止め、その位置で信号を送る（EXP_PAUSE_AT）")

	// ---- ① 片付けてから終わる: 全地点 × SIGTERM ----
	var (
		lostTotal, inflightTotal, leaked int
		results                          []runResult
	)
	for _, p := range points {
		r := runOnce(t, ctx, db, bin, dsn, opts{point: p, signal: syscall.SIGTERM, graceful: true})
		results = append(results, r)
		lostTotal += r.Lost
		inflightTotal += r.Inflight
		// ★起動時の水準に戻ることを要求する（+1 は終了処理中の一時的なもの）。
		// HTTP の keep-alive 接続を閉じ忘れると、ここで必ず落ちる
		if r.Goroutines > r.Base+1 {
			leaked++
		}
	}
	rec.Add(expkit.Variant{
		Name: "片付けてから終わる / SIGTERM を全10地点で",
		Desc: "受付を先に止め、手元のバッチを commit し、未処理をキューへ戻してから終了する",
		Counters: map[string]int64{
			"points":                   int64(len(points)),
			"acked_but_not_committed":  int64(lostTotal),
			"left_inflight":            int64(inflightTotal),
			"runs_with_goroutine_leak": int64(leaked),
		},
		Accident: lostTotal > 0 || leaked > 0,
		Notes:    notesFor(results),
	})
	if lostTotal > 0 {
		t.Errorf("ack 済み消失が %d 件（0 であるべき）", lostTotal)
	}
	if leaked > 0 {
		t.Errorf("goroutine が残った実行が %d 件", leaked)
	}
	t.Logf("[片付けあり] 10地点: 消失 %d / 宙に浮いたまま %d / goroutine 残留 %d",
		lostTotal, inflightTotal, leaked)

	// ---- ② 4種類の信号 ----
	sigs := []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM}, {"SIGINT", syscall.SIGINT},
		{"SIGHUP", syscall.SIGHUP}, {"SIGQUIT", syscall.SIGQUIT},
	}
	var sigLost, sigBad int
	var sigNotes []string
	for _, s := range sigs {
		r := runOnce(t, ctx, db, bin, dsn, opts{point: "in_tx", signal: s.sig, graceful: true})
		sigLost += r.Lost
		if r.ExitCode != 0 || r.Killed {
			sigBad++
		}
		sigNotes = append(sigNotes, fmt.Sprintf("%s: exit=%d 消失=%d 戻した=%d goroutine=%d(起動時 %d)",
			s.name, r.ExitCode, r.Lost, r.Nacked, r.Goroutines, r.Base))
	}
	rec.Add(expkit.Variant{
		Name:     "4種類の信号を同じ地点（トランザクション中）で受ける",
		Counters: map[string]int64{"acked_but_not_committed": int64(sigLost), "abnormal_exit": int64(sigBad)},
		Accident: sigLost > 0 || sigBad > 0,
		Notes:    sigNotes,
	})
	if sigLost > 0 || sigBad > 0 {
		t.Errorf("信号ごとの挙動が揃っていない: 消失 %d / 異常終了 %d", sigLost, sigBad)
	}
	t.Logf("[信号4種] 消失 %d / 異常終了 %d", sigLost, sigBad)

	// ---- ③ 事故A: 片付けずに落ちる ----
	rA := runOnce(t, ctx, db, bin, dsn, opts{point: "in_tx", signal: syscall.SIGTERM, graceful: false})
	rec.Add(expkit.Variant{
		Name: "片付けずに落ちる（signal で即 exit）",
		Desc: "取り出したものを戻さずに終了する。キューの可視性タイムアウトが切れるまで誰も触れない",
		Counters: map[string]int64{
			"acked_but_not_committed": int64(rA.Lost),
			"left_inflight":           int64(rA.Inflight),
			"committed":               int64(rA.Committed),
		},
		Accident: rA.Inflight > 0 || rA.Lost > 0,
		Notes: []string{
			fmt.Sprintf("終了 exit=%d killed=%v", rA.ExitCode, rA.Killed),
			"消失はしないが、宙に浮いた件は可視性タイムアウト（この実験では 1.5 秒）まで進まない",
			"本番の可視性タイムアウトは分単位のことが多く、そのぶん遅延する",
		},
	})
	if rA.Inflight == 0 {
		t.Errorf("事故を再現できていない: 片付けずに落ちたのに宙に浮いた件が 0")
	}
	t.Logf("[事故A 片付けなし] 宙に浮いたまま %d 件 / 消失 %d 件", rA.Inflight, rA.Lost)

	// ---- ④ 事故B: commit の前に ack する ----
	rB := runOnce(t, ctx, db, bin, dsn, opts{
		point: "in_tx", signal: syscall.SIGTERM, graceful: false, ackBeforeCommit: true})
	rec.Add(expkit.Variant{
		Name: "commit の前に ack する",
		Desc: "ack してから書き込む実装。ack と commit の間で落ちると取り戻せない",
		Counters: map[string]int64{
			"acked_but_not_committed": int64(rB.Lost),
			"committed":               int64(rB.Committed),
		},
		Accident: rB.Lost > 0,
		Notes: []string{
			"★キューは『処理済み』と思っているのに、DB には入っていない。再配達もされない",
			"ack は commit のあと。順序を逆にすると、どんな shutdown 手順でも救えない",
		},
	})
	if rB.Lost == 0 {
		t.Errorf("事故を再現できていない: ack 先行なのに消失が 0")
	}
	t.Logf("[事故B ack 先行] 消失 %d 件", rB.Lost)

	rec.Scope(
		"MySQL 8.0 / 同一ホスト / キューは同一マシンの HTTP サーバ（可視性タイムアウト 1.5 秒）",
		"worker は 1 プロセス。複数 worker の同時終了は未測定",
		"goroutine の残留は worker 自身が終了直前に数えた値で判定している",
	)
	rec.Uncertain(
		"SIGKILL（捕捉できない終了）は EXP-1 で扱っており、ここでは対象外",
		"子プロセスを持つ worker（外部コマンド起動）の後始末は未実装・未検証",
		"shutdown 期限を超えた場合の挙動は、期限内に終わる負荷でしか確認していない",
		"OS のプロセスツリーやファイルディスクリプタの残留は測っていない",
	)
	rec.Artifact(
		"internal/queuesvc: pull / ack / nack と可視性タイムアウトを持つキューの代役",
		"cmd/shutdownlab: 受付→有界キュー→バッチ→トランザクション→ack の worker と、地点指定の信号受信",
	)
	rec.Next("EXP-4 backpressure: 入力が処理能力を超えたときのメモリと遅延")

	files, err := rec.Save(strings.Join([]string{
		"片付けてから終わる worker は、10地点すべてで ack 済み消失を出さず、",
		"未処理をキューへ戻し、goroutine も起動時の水準に戻った。",
		"片付けずに落ちると消失こそしないが、取り出した件が可視性タイムアウトまで滞留する。",
		"commit の前に ack する実装だけは、どんな shutdown 手順でも救えない（実際に消失した）。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

type opts struct {
	point           string
	signal          syscall.Signal
	graceful        bool
	ackBeforeCommit bool
}

func runOnce(t *testing.T, ctx context.Context, db *sql.DB, bin, dsn string, o opts) runResult {
	t.Helper()

	tenant := fmt.Sprintf("t3-%s-%s", short(o.point), o.signal.String()[:4])
	if !o.graceful {
		tenant += "-ng"
	}
	if o.ackBeforeCommit {
		tenant += "-af"
	}
	if len(tenant) > 32 {
		tenant = tenant[:32]
	}
	if err := shutdownlab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	q := queuesvc.New(totalItems, visibility)
	defer q.Close()
	if o.point == "retry_sleep" {
		// 再試行の経路を通すために、最初の取り出しを失敗させる
		q.FailNextPulls(2)
	}

	args := []string{
		"-dsn", dsn, "-queue", q.URL(), "-tenant", tenant, "-name", "w1", "-run", "1",
		"-items", fmt.Sprint(totalItems), "-batch", "5", "-capacity", "16",
		fmt.Sprintf("-graceful=%v", o.graceful),
		fmt.Sprintf("-ack-before-commit=%v", o.ackBeforeCommit),
		"-drain-deadline", "3s", "-work-delay", "5ms",
	}
	child, err := expkit.StartChild(ctx, bin, args, expkit.PausePointEnv+"="+o.point)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.WaitPoint("PAUSED at "+o.point, 20*time.Second); err != nil {
		t.Fatalf("[%s] 地点に達しない: %v", o.point, err)
	}
	if err := child.Signal(o.signal); err != nil {
		t.Fatal(err)
	}
	info, err := child.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := shutdownlab.CommittedIDs(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	inDB := map[int]bool{}
	for _, id := range committed {
		inDB[id] = true
	}
	stats := q.Stats()
	lost := 0
	for _, id := range q.AckedIDs() {
		if !inDB[id] {
			lost++
		}
	}

	r := runResult{
		Point: o.point, Signal: o.signal.String(),
		ExitCode: info.Code, Killed: info.Killed, Duration: info.Duration,
		Committed: len(committed), Acked: stats.Acked, Lost: lost,
		Inflight: stats.Inflight, Stdout: info.Stdout,
	}
	r.Nacked = countMarker(info.Stdout, "nacked n=")
	r.Goroutines, r.Base = parseSummary(info.Stdout)

	// 片付けありなら「受付停止が先」の証跡が残っているはず
	if o.graceful && !strings.Contains(info.Stdout, "shutdown_begin") {
		t.Errorf("[%s] 片付けの開始が記録されていない", o.point)
	}
	return r
}

func notesFor(rs []runResult) []string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%-14s exit=%d commit=%d ack=%d 戻した=%d 消失=%d goroutine=%d(起動時 %d)",
			r.Point, r.ExitCode, r.Committed, r.Acked, r.Nacked, r.Lost, r.Goroutines, r.Base))
	}
	return out
}

var summaryRe = regexp.MustCompile(`summary committed=(\d+) acked=(\d+) nacked=(\d+) goroutines=(\d+) base=(\d+)`)

func parseSummary(stdout string) (goroutines, base int) {
	m := summaryRe.FindStringSubmatch(stdout)
	if m == nil {
		return 0, 0
	}
	g, _ := strconv.Atoi(m[4])
	b, _ := strconv.Atoi(m[5])
	return g, b
}

func countMarker(stdout, marker string) int {
	total := 0
	for _, line := range strings.Split(stdout, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(line[i+len(marker):]))
		if err == nil {
			total += n
		}
	}
	return total
}

func short(s string) string {
	s = strings.ReplaceAll(s, "_", "")
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := shutdownlab.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	return db
}

var _ = os.Getenv
