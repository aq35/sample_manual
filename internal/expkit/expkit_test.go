package expkit_test

// 測定器そのものの検査。
// 実験の数字を信じる前に、測る道具が正しいことを確かめる。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

func TestLatency_分位点(t *testing.T) {
	l := expkit.NewLatency()
	for i := 1; i <= 100; i++ {
		l.Record(time.Duration(i) * time.Millisecond)
	}
	s := l.Stats()
	if s.Count != 100 {
		t.Fatalf("件数が違う: %d", s.Count)
	}
	// 1..100ms を入れたので、p50≒50ms, p95≒95ms, p99≒99ms, max=100ms
	check := func(name string, got time.Duration, wantMs int) {
		if got < time.Duration(wantMs-2)*time.Millisecond || got > time.Duration(wantMs+2)*time.Millisecond {
			t.Errorf("%s が %v（期待 %dms 前後）", name, got, wantMs)
		}
	}
	check("p50", s.P50, 50)
	check("p95", s.P95, 95)
	check("p99", s.P99, 99)
	check("max", s.Max, 100)
	t.Logf("%s", s)
}

func TestRecorder_仮説を先に固定する(t *testing.T) {
	r := expkit.NewRecorder("EXP-0", "selftest", "測定器の自己検査")
	// 仮説を固定する前に結果を足そうとしたら止まる
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Freeze 前の Add が通ってしまった")
			}
		}()
		r.Add(expkit.Variant{Name: "x"})
	}()

	r.Freeze("結果を見る前に書いた仮説")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("2回目の Freeze が通ってしまった")
			}
		}()
		r.Freeze("あとから書き換えた仮説")
	}()
	t.Log("仮説は結果を見る前にしか書けない（Freeze 後の変更は panic）")
}

func TestRecorder_結果を保存する(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXP_RESULTS_DIR", dir)

	r := expkit.NewRecorder("EXP-0", "selftest", "測定器の自己検査")
	r.Env(expkit.CaptureEnv(context.Background(), nil))
	r.Freeze("保存した結果から、実行条件と SHA にたどり着ける")
	r.Workload("events", 100).Injection("kill_at", "step2")
	r.Add(expkit.Variant{
		Name:     "before",
		Accident: true,
		Counters: map[string]int64{"duplicate": 3},
		Notes:    []string{"防止策なしでは重複が出る"},
	})
	r.Add(expkit.Variant{
		Name:     "after",
		Counters: map[string]int64{"duplicate": 0},
	})
	r.Scope("MySQL 8.0 / 単一インスタンス")
	r.Uncertain("レプリカ構成では未検証")
	files, err := r.Save("防止策で重複が 0 になった")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("保存されたファイル数が違う: %v", files)
	}

	var jsonPath, mdPath string
	for _, f := range files {
		switch filepath.Ext(f) {
		case ".json":
			jsonPath = f
		case ".md":
			mdPath = f
		}
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var run expkit.Run
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	if run.Hypothesis == "" || run.Env.GoVersion == "" || len(run.Variants) != 2 {
		t.Fatalf("記録が欠けている: %+v", run)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Hypothesis (frozen before result)", "Failure injection", "適用範囲", "保証しない範囲"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("報告に %q が無い", want)
		}
	}
	t.Logf("結果を %s に保存した（JSON と Markdown）", filepath.Dir(jsonPath))
}

func TestSampler_goroutineとメモリを追う(t *testing.T) {
	s := expkit.NewSampler(nil, 10*time.Millisecond).Start()
	stop := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() { <-stop }()
	}
	time.Sleep(60 * time.Millisecond)
	close(stop)
	time.Sleep(60 * time.Millisecond)
	sum := expkit.Summarize(s.Stop())

	if sum.Count < 3 {
		t.Fatalf("観測点が少なすぎる: %d", sum.Count)
	}
	if sum.MaxGoroutines < 50 {
		t.Fatalf("goroutine の山を捉えられていない: %d", sum.MaxGoroutines)
	}
	t.Logf("観測 %d 点 / goroutine 最大 %d → 終了時 %d / RSS 最大 %.1f MB",
		sum.Count, sum.MaxGoroutines, sum.FinalGoroutines, float64(sum.MaxRSS)/1024/1024)
}

func TestChild_指定した地点で死ぬ(t *testing.T) {
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/expchild")
	ctx := context.Background()

	// ① 地点を指定しなければ最後まで走る
	c, err := expkit.StartChild(ctx, bin, []string{"-steps", "3"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Wait(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if info.Killed || info.Code != 0 {
		t.Fatalf("正常終了しなかった: %s", info)
	}
	if len(info.Points) < 4 { // step1..3 + DONE
		t.Fatalf("通過地点が記録されていない: %v", info.Points)
	}

	// ② 指定した地点で SIGKILL される（回復処理は走らない）
	c, err = expkit.StartChild(ctx, bin, []string{"-steps", "3"}, expkit.KillPointEnv+"=step2")
	if err != nil {
		t.Fatal(err)
	}
	info, err = c.Wait(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Killed || info.Signal != syscall.SIGKILL {
		t.Fatalf("SIGKILL で死んでいない: %s", info)
	}
	for _, p := range info.Points {
		if p == "step3" || p == "DONE" {
			t.Fatalf("kill 地点より先へ進んでいる: %v", info.Points)
		}
	}
	t.Logf("step2 で SIGKILL: 通過地点 %v", info.Points)
}

func TestChild_指定した地点で止めて信号を送る(t *testing.T) {
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/expchild")
	ctx := context.Background()

	c, err := expkit.StartChild(ctx, bin, []string{"-steps", "3", "-graceful"},
		expkit.PausePointEnv+"=step2")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitPoint("PAUSED at step2", 10*time.Second); err != nil {
		t.Fatalf("停止地点に達しない: %v", err)
	}
	if err := c.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	info, err := c.Wait(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if info.Killed {
		t.Fatalf("SIGTERM を捕まえられていない: %s", info)
	}
	if !strings.Contains(info.Stdout, "SIGNAL terminated") || !strings.Contains(info.Stdout, "CLEANUP") {
		t.Fatalf("片付けが行われていない: %s", info.Stdout)
	}
	t.Log("狙った地点で止めて、そこへ信号を送れる（graceful shutdown の実験に使う）")
}
