package ssecapacity_test

// EXP-40: SSE は何人まで／hub あり・なしの容量を、実測係数から計算する。
//
//	go test ./internal/ssecapacity/ -run TestEXP40 -v   （DB 不要）
//
// EXP-31（1接続 ~34KB）・EXP-38（fan-out は激安）・DB 往復を式に入れ、代表シナリオで
// 「1タスク何人・何タスク要る・DB 読み/秒」を出す。実値を差し替えれば即再計算できる。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/ssecapacity"
)

func TestEXP40_SSE容量計算(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-40", "sse-capacity-calc",
		"SSE は何人まで（hub あり・なし）を実測係数から計算")
	rec.Env(expkit.CaptureEnv(ctx, nil)) // DB 不要
	rec.Freeze(
		"1) hub ありの天井はメモリ/fd（1接続 ~34KB）。DB 読みは接続数に無関係（テナント数 / 間隔）。 " +
			"2) hub なしの DB 読みは接続数に比例（接続数 / 間隔）＝すぐ DB 予算を食う。 " +
			"3) だから hub なしは数百〜低い千で頭打ち、hub ありは 1タスク 1万超＋横に線形。")

	type scenario struct {
		name string
		in   ssecapacity.Inputs
	}
	base := ssecapacity.Defaults()
	scen := func(name string, subs, tenants int, poll float64) scenario {
		in := base
		in.Subscribers = subs
		in.Tenants = tenants
		in.PollSeconds = poll
		return scenario{name, in}
	}
	scenarios := []scenario{
		scen("既定: 30テナント・1万接続・1秒", 10000, 30, 1),
		scen("間隔を緩める: 30テナント・1万接続・5秒", 10000, 30, 5),
		scen("大規模: 500テナント・10万接続・1秒", 100000, 500, 1),
		scen("小規模: 30テナント・300接続・1秒", 300, 30, 1),
	}

	var results []ssecapacity.Result
	for _, s := range scenarios {
		r := ssecapacity.Compute(s.in)
		results = append(results, r)
		rec.Add(expkit.Variant{
			Name: s.name,
			Counters: map[string]int64{
				"per_task_conn_cap": int64(r.PerTaskConnCap),
				"tasks_needed":      int64(r.TasksNeeded),
				"nohub_max_conns":   int64(r.NoHubMaxConns),
			},
			Metrics: map[string]float64{
				"hub_db_reads_per_sec":   r.HubDBReadsPerSec,
				"nohub_db_reads_per_sec": r.NoHubDBReadsPerSec,
			},
			Notes: []string{
				"hub あり: 1タスク " + itoa(r.PerTaskConnCap) + " 接続（" + r.Binding + "律速）× " +
					itoa(r.TasksNeeded) + " タスク / DB 読み " + f0(r.HubDBReadsPerSec) + "/秒（接続数に無関係）",
				"hub なし: DB 読み " + f0(r.NoHubDBReadsPerSec) + "/秒 → 予算内=" + b(r.NoHubFits) +
					"、予算で許される最大 " + itoa(r.NoHubMaxConns) + " 接続",
			},
		})
		t.Logf("%s: hub[cap=%d(%s) tasks=%d reads=%.0f/s] nohub[reads=%.0f/s fits=%v max=%d]",
			s.name, r.PerTaskConnCap, r.Binding, r.TasksNeeded, r.HubDBReadsPerSec,
			r.NoHubDBReadsPerSec, r.NoHubFits, r.NoHubMaxConns)
	}

	// ---- 検証 ----
	// hub の DB 読みは接続数に無関係（同じテナント数・間隔なら 1万でも 10万でも同じ）
	a := ssecapacity.Compute(scen("a", 1000, 30, 1).in)
	bb := ssecapacity.Compute(scen("b", 100000, 30, 1).in)
	if a.HubDBReadsPerSec != bb.HubDBReadsPerSec {
		t.Errorf("hub の DB 読みが接続数に依存している: %v vs %v", a.HubDBReadsPerSec, bb.HubDBReadsPerSec)
	}
	// hub なしの DB 読みは接続数に比例
	if !(bb.NoHubDBReadsPerSec > a.NoHubDBReadsPerSec) {
		t.Errorf("hub なしの DB 読みが接続数に比例していない")
	}
	// 1 vCPU/2GB の1タスク接続上限はメモリ律速で概ね1万台
	got := results[0].PerTaskConnCap
	if got < 8000 || got > 20000 {
		t.Errorf("1タスク接続上限が想定域(8k〜20k)外: %d", got)
	}
	if results[0].Binding != "memory" {
		t.Errorf("2GB/34KB でメモリ律速のはず: %s", results[0].Binding)
	}
	// 既定シナリオ（1万接続・1秒）は hub なしだと DB 予算に収まらない
	if results[0].NoHubFits {
		t.Errorf("1万接続/1秒が hub なしで DB 予算に収まってしまった（収まらないはず）")
	}
	// 大規模は複数タスク要る
	if results[2].TasksNeeded < 2 {
		t.Errorf("10万接続で必要タスクが %d（複数のはず）", results[2].TasksNeeded)
	}

	rec.Scope(
		"純計算（DB 不要）/ 係数: 1接続 34KB(EXP-31)・安全率 0.4・予約 800MB・fd 60000・SSE の DB 予算 2000/秒",
		"hub あり: 天井 = min(メモリ由来, fd)。DB 読み = テナント数/間隔",
		"hub なし: DB 読み = 接続数/間隔。最大接続 = DB 予算 × 間隔",
	)
	rec.Uncertain(
		"係数は実測ベースだが環境で動く（TLS 実装・GC・行の太さ）。実 RSS は上下する",
		"DB 予算 2000/秒は保守的な例。実際は DB の余力と実クエリとの取り合いで決める（EXP-5）",
		"fan-out CPU は無視（EXP-38）。ソケット書込み・帯域は別途（更新頻度が高いと効く）",
		"複数タスクに分かれると poller はタスク（プロセス）ごとに要る → プロセス跨ぎは pub/sub（EXP-38）",
	)
	rec.Artifact(
		"internal/ssecapacity: SSE 容量計算機（実値を差し替えて再計算できる）",
		"docs/sse-fan-in.md: hub あり・なしの何人まで",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"hub なしは DB 律速で数百〜低い千（接続数ぶん DB を食う）。hub ありはメモリ/fd 律速で 1タスク" +
			"1万〜1.5万・DB は平ら（テナント数/間隔）で横に線形。SSE をやるなら hub 必須、が数字で出る。" +
			"実値（間隔・テナント数・接続数・スペック・DB 予算）を Inputs に入れれば即再計算できる。")
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
func f0(v float64) string { return itoa(int(v + 0.5)) }
func b(v bool) string {
	if v {
		return "はい"
	}
	return "いいえ"
}
