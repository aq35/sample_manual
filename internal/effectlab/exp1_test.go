package effectlab_test

// EXP-1: 外部 effect の途中で SIGKILL されたとき、何が起きるか。
//
//	MYSQL_DSN=... go test ./internal/effectlab/ -run TestEXP1 -v -timeout 20m
//
// 見るのは DB の中身ではなく **外の世界で effect が何回起きたか**。
// 判定は外部サービス側の記録（プロセスを殺しても消えない）と突き合わせて行う。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/effectlab"
	"github.com/aq35/sample_manual/internal/effectsvc"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

const (
	requests = 5
	killOn   = 3 // 3件目の処理中に落とす（前後に正常な件を残すため）
)

type outcome struct {
	Strategy       string
	Point          string
	EffectsTotal   int
	Duplicates     int // 同じ要求に対して2回以上 effect が起きた
	Orphans        int // effect は起きたのに、アプリ側に記録が無い
	FalseCompleted int // アプリは完了と言っているが effect が無い
	Unknown        int // OUTCOME_UNKNOWN のまま残った
	Completed      int
	ByObservation  int // 送り直さず、相手への問い合わせで決着した件数
	ReDispatched   int // 再送した件数（attempts が 2 以上）
	RecoveryMS     float64
	KilledAt       string
}

func TestEXP1_外部effect途中のSIGKILL(t *testing.T) {
	dsn := mysqltest.DSN(t)
	db := openDB(t, dsn)
	ctx := context.Background()
	if err := effectlab.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/effectlab")

	rec := expkit.NewRecorder("EXP-1", "external-effect-crash",
		"外部 effect の途中で SIGKILL されたときの二重送信と『結果不明』の扱い")
	rec.Env(expkit.CaptureEnv(ctx, db))
	// ★結果を見る前に固定する仮説
	rec.Freeze(strings.Join([]string{
		"1) naive（呼んでから記録する・冪等キー無し）は、effect 成立後の SIGKILL で二重 effect を出す。",
		"2) 冪等キーを付け、送る前に意図を記録すれば、二重 effect は 0 になる。",
		"3) ただし 2 は相手が冪等キーを守ることに依存する。守らない相手では二重に戻る。",
		"4) 応答が得られなかった要求は、送り直す前に相手へ問い合わせれば、",
		"   『二重送信』も『不明のまま放置』も避けられる。",
	}, " "))
	rec.Workload("requests_per_run", requests).
		Workload("kill_on_request", killOn).
		Workload("strategies", []string{
			string(effectlab.StrategyNaive), string(effectlab.StrategyIdem),
			string(effectlab.StrategyOutbox), string(effectlab.StrategyObserve)}).
		Injection("kill_points", []string(effectlab.KillPoints)).
		Injection("method", "子プロセスが指定地点で自分に SIGKILL（effect 成立後の地点だけはサービス側 hook が子を kill）")

	strategies := []effectlab.Strategy{
		effectlab.StrategyNaive, effectlab.StrategyIdem,
		effectlab.StrategyOutbox, effectlab.StrategyObserve,
	}

	for _, st := range strategies {
		var (
			results                                    []outcome
			dup, orphan, falseDone, unknown, completed int
			observed, redis                            int
			recovery                                   = expkit.NewLatency()
		)
		for _, point := range effectlab.KillPoints {
			o := runOne(t, ctx, db, bin, dsn, st, point, true)
			results = append(results, o)
			dup += o.Duplicates
			orphan += o.Orphans
			falseDone += o.FalseCompleted
			unknown += o.Unknown
			completed += o.Completed
			observed += o.ByObservation
			redis += o.ReDispatched
			recovery.Record(time.Duration(o.RecoveryMS * float64(time.Millisecond)))
		}

		lat := recovery.Stats()
		v := expkit.Variant{
			Name: string(st),
			Desc: describe(st),
			Counters: map[string]int64{
				"duplicate_effects":       int64(dup),
				"orphan_effects":          int64(orphan),
				"false_completed":         int64(falseDone),
				"outcome_unknown_left":    int64(unknown),
				"completed":               int64(completed),
				"resolved_by_observation": int64(observed),
				"re_dispatched":           int64(redis),
				"kill_points_exercised":   int64(len(effectlab.KillPoints)),
			},
			Latency:  &lat,
			Accident: dup > 0 || orphan > 0 || falseDone > 0,
		}
		for _, o := range results {
			if o.Duplicates > 0 || o.Orphans > 0 || o.FalseCompleted > 0 {
				v.Notes = append(v.Notes, fmt.Sprintf(
					"%s で事故: 二重 %d / 記録なし effect %d / 実体の無い完了 %d",
					o.Point, o.Duplicates, o.Orphans, o.FalseCompleted))
			}
		}
		if len(v.Notes) == 0 {
			v.Notes = append(v.Notes, "全 7 地点で、二重 effect も記録漏れも実体の無い完了も 0")
		}
		rec.Add(v)
		t.Logf("[%s] 二重=%d 記録なし=%d 実体なし完了=%d 不明のまま=%d 完了=%d 観測で決着=%d 再送=%d（%d 地点）",
			st, dup, orphan, falseDone, unknown, completed, observed, redis, len(effectlab.KillPoints))

		switch st {
		case effectlab.StrategyNaive:
			if dup == 0 && orphan == 0 {
				t.Errorf("naive で事故を再現できていない（実験の前提が崩れている）")
			}
		default:
			if dup > 0 {
				t.Errorf("%s で二重 effect が %d 件出た", st, dup)
			}
			if falseDone > 0 {
				t.Errorf("%s で『effect が無いのに完了』が %d 件出た", st, falseDone)
			}
		}
	}

	// ---- 反証: 冪等キーは「相手が守る」ことに依存している ----
	o := runOneWithBehavior(t, ctx, db, bin, dsn, effectlab.StrategyIdem,
		effectlab.PointAfterEffectBeforeResponse, false)
	rec.Add(expkit.Variant{
		Name: "idempotency_key / 相手がキーを守らない場合",
		Desc: "同じ方式のまま、外部サービスの冪等キー対応だけを外した反証",
		Counters: map[string]int64{
			"duplicate_effects": int64(o.Duplicates),
			"effects_total":     int64(o.EffectsTotal),
		},
		Accident: o.Duplicates > 0,
		Notes: []string{
			"冪等キーは **こちら側の防御ではない**。相手が重複を弾いて初めて効く",
			"相手の実装が不明なら、送り直す前に問い合わせる（observe）ほうが確実",
		},
	})
	if o.Duplicates == 0 {
		t.Error("反証にならなかった: 相手がキーを守らないのに二重 effect が出ていない")
	}
	t.Logf("[反証] 相手が冪等キーを守らない場合: 二重 effect %d 件", o.Duplicates)

	// ---- 決定的な比較: 相手がキーを守らない場合、observe だけが二重を避けられる ----
	o2 := runOneWithBehavior(t, ctx, db, bin, dsn, effectlab.StrategyObserve,
		effectlab.PointAfterEffectBeforeResponse, false)
	rec.Add(expkit.Variant{
		Name: "outbox_then_observe / 相手がキーを守らない場合",
		Desc: "同じ条件（相手は冪等キーを見ない）で、送り直す前に問い合わせる方式",
		Counters: map[string]int64{
			"duplicate_effects":       int64(o2.Duplicates),
			"effects_total":           int64(o2.EffectsTotal),
			"resolved_by_observation": int64(o2.ByObservation),
			"re_dispatched":           int64(o2.ReDispatched),
		},
		Accident: o2.Duplicates > 0,
		Notes: []string{
			"冪等キーが効かない相手でも、二重 effect は発生しなかった",
			"『結果が分からないものは、送り直す前に相手に聞く』が、相手の実装に依存しない唯一の防御",
		},
	})
	if o2.Duplicates > 0 {
		t.Errorf("観測してから決める方式でも二重 effect が %d 件出た", o2.Duplicates)
	}
	t.Logf("[比較] 相手がキーを守らない × 観測してから決める: 二重 %d 件 / 観測で決着 %d 件",
		o2.Duplicates, o2.ByObservation)

	rec.Scope(
		"MySQL 8.0 / 同一ホスト / 外部サービスは同一マシンの HTTP サーバ",
		"1プロセス・逐次処理。並行実行時の競合は EXP-2 で扱う",
		"effect の記録は外部サービス側のファイルに fsync して保存（プロセス kill では消えない）",
	)
	rec.Uncertain(
		"ネットワーク分断（応答が返らないまま接続だけ生きる）は未再現",
		"外部サービス側が effect 記録の前に落ちる場合は未検証（相手の耐久性は仮定）",
		"観測 API が古い値を返す（レプリカ遅延）場合の扱いは未検証",
	)
	rec.Artifact(
		"internal/effectsvc: 外部サービスの代役（effect をファイルに永続化・冪等キー対応を切替可能）",
		"internal/effectlab: 4方式の実装と OUTCOME_UNKNOWN を含む状態機械",
		"cmd/effectlab + expkit: 指定地点で SIGKILL して再起動する実験の型",
	)
	rec.Next("EXP-2 lease/fencing: 複数プロセスが同じ対象を処理する場合の重複")

	files, err := rec.Save(strings.Join([]string{
		"『呼んでから記録する』方式は、effect 成立後に落ちると記録が残らず、再試行で二重 effect になる。",
		"送る前に意図を記録し、要求ごとに固定した冪等キーを付ければ二重 effect は 0 になるが、",
		"それは相手がキーを守る場合に限る（反証で二重に戻ることを確認）。",
		"応答が得られなかったものを『送り直す前に問い合わせる』方式は、",
		"相手の冪等性に依存せず、二重送信も『不明のまま放置』も避けられた。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

func describe(st effectlab.Strategy) string {
	switch st {
	case effectlab.StrategyNaive:
		return "外部を呼んでから記録する。冪等キー無し。再起動後は記録が無いものを最初から処理する"
	case effectlab.StrategyIdem:
		return "送る前に意図を記録し、要求ごとに固定した冪等キーを付けて送る"
	case effectlab.StrategyOutbox:
		return "業務状態と『送るつもり』を同じトランザクションで確定してから送る"
	case effectlab.StrategyObserve:
		return "outbox に加え、結果不明のものは **送り直す前に相手へ問い合わせる**"
	}
	return ""
}

// runOne は1つの（方式, 故障地点）の組を実行する。
func runOne(t *testing.T, ctx context.Context, db *sql.DB, bin, dsn string,
	st effectlab.Strategy, point string, honorIdem bool) outcome {
	t.Helper()
	return runOneWithBehavior(t, ctx, db, bin, dsn, st, point, honorIdem)
}

func runOneWithBehavior(t *testing.T, ctx context.Context, db *sql.DB, bin, dsn string,
	st effectlab.Strategy, point string, honorIdem bool) outcome {
	t.Helper()

	tenant := fmt.Sprintf("t-%s-%s", shortName(string(st)), shortName(point))
	if !honorIdem {
		tenant += "-nokey"
	}
	if len(tenant) > 32 {
		tenant = tenant[:32]
	}
	if err := effectlab.Reset(ctx, db, tenant); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "effects.jsonl")
	var (
		mu       sync.Mutex
		childPID int
		killed   bool
	)
	behavior := effectsvc.Behavior{HonorIdempotencyKey: honorIdem}

	// 「effect 成立後・応答前」に落とす地点だけは、サービス側の hook から子を殺す。
	// 子プロセス自身のタイマーで殺すと、effect の前か後かが実行ごとに変わってしまう。
	if point == effectlab.PointAfterEffectBeforeResponse {
		behavior.HangAfterEffect = true
		behavior.OnEffect = func(e effectsvc.Effect) {
			if e.RequestID != effectlab.RequestID(killOn) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if killed || childPID == 0 {
				return
			}
			killed = true
			_ = syscallKill(childPID)
		}
	}
	svc := effectsvc.New(logPath, behavior)
	defer svc.Close()

	// ---- 1回目: 指定地点で落ちる ----
	args := []string{
		"-dsn", dsn, "-service", svc.URL(), "-tenant", tenant,
		"-strategy", string(st), "-requests", fmt.Sprint(requests),
		"-kill-on", fmt.Sprint(killOn), "-http-timeout", "1s",
	}
	env := []string{}
	if point != effectlab.PointAfterEffectBeforeResponse {
		env = append(env, expkit.KillPointEnv+"="+point)
	}
	child, err := expkit.StartChild(ctx, bin, args, env...)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	childPID = child.PID()
	mu.Unlock()

	info, err := child.Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("子プロセスの待機に失敗: %v", err)
	}
	if !info.Killed {
		t.Fatalf("[%s/%s] 子プロセスが SIGKILL されていない: %s\n%s", st, point, info, info.Stdout)
	}

	// ---- 2回目: 再起動して回復処理 ----
	svc.SetBehavior(effectsvc.Behavior{HonorIdempotencyKey: honorIdem}) // hang を解除
	start := time.Now()
	rec, err := expkit.StartChild(ctx, bin, append(append([]string{}, args...), "-recover"))
	if err != nil {
		t.Fatal(err)
	}
	rinfo, err := rec.Wait(60 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	recovery := time.Since(start)
	if rinfo.Killed {
		t.Fatalf("[%s/%s] 回復処理が異常終了した: %s\n%s", st, point, rinfo, rinfo.Stderr)
	}

	// ---- 突き合わせ ----
	report, err := effectlab.Load(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	effects := svc.CountByRequest()

	o := outcome{
		Strategy: string(st), Point: point,
		RecoveryMS: float64(recovery.Microseconds()) / 1000,
		KilledAt:   point,
	}
	for _, n := range effects {
		o.EffectsTotal += n
		if n > 1 {
			o.Duplicates += n - 1
		}
	}
	for i := 1; i <= requests; i++ {
		id := effectlab.RequestID(i)
		n := effects[id]
		receipt, known := report.Receipts[id]
		state := stateOf(report, id)

		if n > 0 && !known {
			o.Orphans++ // 外では起きたのに、アプリは何も知らない
		}
		if n == 0 && state == effectlab.StateCompleted {
			o.FalseCompleted++ // 完了と言っているのに effect が無い
		}
		if state == effectlab.StateOutcomeUnknown {
			o.Unknown++
		}
		if state == effectlab.StateCompleted {
			o.Completed++
		}
		if report.Sources[id] == "observation" {
			o.ByObservation++
		}
		if report.Attempts[id] > 1 {
			o.ReDispatched++
		}
		// 禁止事項の検査: 受領書には相手が発行した ID しか入らない
		if receipt != "" && !strings.HasPrefix(receipt, "rcpt-") {
			t.Errorf("[%s/%s] %s の受領書が相手の発行物でない: %q", st, point, id, receipt)
		}
		// 禁止事項の検査: 受領書なしで完了にしていない
		if state == effectlab.StateCompleted && receipt == "" {
			t.Errorf("[%s/%s] %s が受領書なしで COMPLETED になっている", st, point, id)
		}
	}
	return o
}

func stateOf(rep effectlab.Report, id string) string {
	// Report.States は集計なので、個別の状態は Receipts/Sources から引けない。
	// ここでは states を数えるために再取得する代わりに、attempts の有無で存在を判定する。
	if _, ok := rep.Receipts[id]; !ok {
		return ""
	}
	return rep.StateOf(id)
}

func shortName(s string) string {
	s = strings.ReplaceAll(s, "_", "")
	if len(s) > 10 {
		s = s[:10]
	}
	return s
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := effectlab.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	return db
}

func syscallKill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Kill)
}
