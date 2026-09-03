package expkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Run は1つの実験の記録全体。docs/results/<unit>/ に JSON と Markdown で残す。
//
// ★Hypothesis は結果を見る前に固定する（Freeze）。
// 「出た数字に合わせて仮説を書く」ことを構造的に防ぐため、Freeze 後の変更を拒否する。
type Run struct {
	Unit       string    `json:"unit"`       // 例: "EXP-1"
	Name       string    `json:"name"`       // 例: "external-effect-crash"
	Title      string    `json:"title"`      // 人間向けの一行
	Hypothesis string    `json:"hypothesis"` // 結果を見る前に書く
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	Env        Env       `json:"env"`

	Workload  map[string]any `json:"workload"`  // 入力条件
	Injection map[string]any `json:"injection"` // 故障注入の内容

	Variants []Variant `json:"variants"`

	Verdict     string   `json:"verdict"`     // 何が言えたか
	Scope       []string `json:"scope"`       // この結果が当てはまる範囲
	Uncertainty []string `json:"uncertainty"` // 保証しない範囲・未検証
	Artifacts   []string `json:"artifacts"`   // 再利用できる成果物（テスト・型など）
	Next        []string `json:"next"`        // 次の実験
}

// Variant は比較する方式1つぶんの結果（before / after もこれで表す）。
type Variant struct {
	Name     string             `json:"name"`
	Desc     string             `json:"desc,omitempty"`
	Counters map[string]int64   `json:"counters,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	Latency  *LatencyStats      `json:"latency,omitempty"`
	Samples  *SampleSummary     `json:"samples,omitempty"`
	// Accident はこの方式で事故が起きたか。
	// 「防止策を外すと落ちる」ことを示すために、わざと true になる方式を必ず1つ置く。
	Accident bool     `json:"accident"`
	Notes    []string `json:"notes,omitempty"`
}

// Recorder は Run を組み立てて保存する。
type Recorder struct {
	run    Run
	frozen bool
	dir    string
}

// NewRecorder は実験の記録を開始する。unit は "EXP-1" のような単位名。
func NewRecorder(unit, name, title string) *Recorder {
	return &Recorder{
		run: Run{
			Unit:      unit,
			Name:      name,
			Title:     title,
			StartedAt: time.Now().UTC(),
			Workload:  map[string]any{},
			Injection: map[string]any{},
		},
		dir: resultsDir(unit),
	}
}

// Freeze は仮説を固定する。★結果を見る前に呼ぶこと。
func (r *Recorder) Freeze(hypothesis string) *Recorder {
	if r.frozen {
		panic("expkit: 仮説はすでに固定されている（結果を見てから書き換えないこと）")
	}
	r.run.Hypothesis = hypothesis
	r.frozen = true
	return r
}

func (r *Recorder) Env(e Env) *Recorder                 { r.run.Env = e; return r }
func (r *Recorder) Workload(k string, v any) *Recorder  { r.run.Workload[k] = v; return r }
func (r *Recorder) Injection(k string, v any) *Recorder { r.run.Injection[k] = v; return r }
func (r *Recorder) Scope(s ...string) *Recorder         { r.run.Scope = append(r.run.Scope, s...); return r }
func (r *Recorder) Uncertain(s ...string) *Recorder {
	r.run.Uncertainty = append(r.run.Uncertainty, s...)
	return r
}
func (r *Recorder) Artifact(s ...string) *Recorder {
	r.run.Artifacts = append(r.run.Artifacts, s...)
	return r
}
func (r *Recorder) Next(s ...string) *Recorder { r.run.Next = append(r.run.Next, s...); return r }

func (r *Recorder) Add(v Variant) *Recorder {
	if !r.frozen {
		panic("expkit: 仮説を Freeze する前に結果を足している")
	}
	r.run.Variants = append(r.run.Variants, v)
	return r
}

// Save は結果を docs/results/<unit>/ に書く。戻り値は書いたファイル。
func (r *Recorder) Save(verdict string) ([]string, error) {
	if !r.frozen {
		return nil, fmt.Errorf("expkit: 仮説が固定されていない")
	}
	r.run.Verdict = verdict
	r.run.EndedAt = time.Now().UTC()

	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return nil, err
	}
	stamp := r.run.StartedAt.Format("20060102-150405")
	base := filepath.Join(r.dir, fmt.Sprintf("%s-%s", stamp, r.run.Name))

	jsonPath := base + ".json"
	b, err := json.MarshalIndent(r.run, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(jsonPath, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}

	mdPath := base + ".md"
	if err := os.WriteFile(mdPath, []byte(r.Markdown()), 0o644); err != nil {
		return nil, err
	}
	return []string{jsonPath, mdPath}, nil
}

// Markdown は実験指示書の報告フォーマットに沿った本文を作る。
func (r *Recorder) Markdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# %s %s", r.run.Unit, r.run.Title)
	w("")
	w("| | |")
	w("| --- | --- |")
	w("| Experiment | %s / %s |", r.run.Unit, r.run.Name)
	w("| Starting SHA | `%.12s`%s |", r.run.Env.GitSHA, dirtyMark(r.run.Env.GitDirty))
	w("| Hypothesis (frozen before result) | %s |", r.run.Hypothesis)
	w("| Environment | %s |", r.run.Env.String())
	w("| Started / Ended | %s / %s |",
		r.run.StartedAt.Format(time.RFC3339), r.run.EndedAt.Format(time.RFC3339))
	w("")

	if len(r.run.Workload) > 0 {
		w("## Workload")
		w("")
		for _, k := range sortedKeys(r.run.Workload) {
			w("- `%s` = %v", k, r.run.Workload[k])
		}
		w("")
	}
	if len(r.run.Injection) > 0 {
		w("## Failure injection")
		w("")
		for _, k := range sortedKeys(r.run.Injection) {
			w("- `%s` = %v", k, r.run.Injection[k])
		}
		w("")
	}

	w("## Results")
	w("")
	for _, v := range r.run.Variants {
		mark := "OK"
		if v.Accident {
			mark = "**事故あり**"
		}
		w("### %s — %s", v.Name, mark)
		w("")
		if v.Desc != "" {
			w("%s", v.Desc)
			w("")
		}
		if len(v.Counters) > 0 {
			w("| 数えたもの | 値 |")
			w("| --- | --- |")
			for _, k := range sortedCounterKeys(v.Counters) {
				w("| %s | %d |", k, v.Counters[k])
			}
			w("")
		}
		if len(v.Metrics) > 0 {
			w("| 測ったもの | 値 |")
			w("| --- | --- |")
			for _, k := range sortedMetricKeys(v.Metrics) {
				w("| %s | %.3f |", k, v.Metrics[k])
			}
			w("")
		}
		if v.Latency != nil && v.Latency.Count > 0 {
			w("遅延: %s", v.Latency.String())
			w("")
		}
		if v.Samples != nil && v.Samples.Count > 0 {
			w("goroutine 最大 %d / 終了時 %d ・ヒープ最大 %.1f MB ・RSS 最大 %.1f MB ・DB 接続 最大 %d（待ち %d 回 / %v）",
				v.Samples.MaxGoroutines, v.Samples.FinalGoroutines,
				float64(v.Samples.MaxHeapAlloc)/1024/1024, float64(v.Samples.MaxRSS)/1024/1024,
				v.Samples.MaxOpenConns, v.Samples.TotalWaitCount,
				v.Samples.TotalWaitDuration.Round(time.Millisecond))
			w("")
		}
		for _, n := range v.Notes {
			w("- %s", n)
		}
		if len(v.Notes) > 0 {
			w("")
		}
	}

	w("## Verdict")
	w("")
	w("%s", r.run.Verdict)
	w("")
	if len(r.run.Scope) > 0 {
		w("## 適用範囲")
		w("")
		for _, s := range r.run.Scope {
			w("- %s", s)
		}
		w("")
	}
	if len(r.run.Uncertainty) > 0 {
		w("## 保証しない範囲・未検証")
		w("")
		for _, s := range r.run.Uncertainty {
			w("- %s", s)
		}
		w("")
	}
	if len(r.run.Artifacts) > 0 {
		w("## 再利用できる成果物")
		w("")
		for _, s := range r.run.Artifacts {
			w("- %s", s)
		}
		w("")
	}
	if len(r.run.Next) > 0 {
		w("## 次の実験")
		w("")
		for _, s := range r.run.Next {
			w("- %s", s)
		}
		w("")
	}
	if len(r.run.Env.MySQLVars) > 0 {
		w("## MySQL の設定（測定時）")
		w("")
		w("| 変数 | 値 |")
		w("| --- | --- |")
		keys := make([]string, 0, len(r.run.Env.MySQLVars))
		for k := range r.run.Env.MySQLVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w("| `%s` | %s |", k, r.run.Env.MySQLVars[k])
		}
		w("")
	}
	return b.String()
}

// Run は組み立て中の記録（テストからの検査用）。
func (r *Recorder) Run() Run { return r.run }

func dirtyMark(dirty bool) string {
	if dirty {
		return " (作業ツリーに未コミットの変更あり)"
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCounterKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedMetricKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resultsDir は結果の保存先。リポジトリのルートからの相対で docs/results/<unit>/。
func resultsDir(unit string) string {
	if dir := os.Getenv("EXP_RESULTS_DIR"); dir != "" {
		return filepath.Join(dir, strings.ToLower(unit))
	}
	root := repoRoot()
	return filepath.Join(root, "docs", "results", strings.ToLower(unit))
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}
