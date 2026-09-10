package lint_test

// EXP-67: 状態機械の網羅を静的解析で強制する（Go に sum type / enum が無い弱点を仕組みで補う）。
//
//	go test ./internal/lint/ -run TestEXP67 -v
//
// Go は `switch status { ... }` で case を書き忘れてもコンパイルが通る。
// pending→in_progress→completed に completed を足したのに古い switch が素通りする事故を、
// go/analysis（型情報つき）で検出する。analysistest で検出の正しさを固定し、
// このリポジトリ全体へ当てた結果（本物1件・限界1件）を記録する。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/lint"
)

func TestEXP67_状態機械の網羅を静的検査(t *testing.T) {
	// 検出の正しさ（未網羅は検出・全網羅とdefaultは検出しない・理由つき逃げ道は通す）
	analysistest.Run(t, analysistest.TestData(), lint.Exhaustive, "exhaustive/data")

	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-67", "exhaustive-switch",
		"enum 的 named 型の switch 網羅を go/analysis で強制する（Go に sum type が無い弱点を補う）")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(strings.Join([]string{
		"Go は case の書き忘れをコンパイルで防げない（sum type / enum が無い）。",
		"『同じ named 型の定数が2つ以上』を enum とみなし、その型を tag に持つ switch が全メンバを",
		"網羅しているか（default 無しで）を型情報つきで検出できる。default のある switch は対象外。",
		"ネストした switch の絞り込み（外側 case で除外済み）は追えないので、そこは理由つき逃げ道で通す。",
	}, " "))

	rec.Add(expkit.Variant{
		Name: "検出の正しさ（analysistest）",
		Notes: []string{
			"未網羅（Completed 忘れ・default 無し）→ 検出",
			"全メンバ網羅 → 検出しない",
			"default あり → 検出しない（意図的に『残りはまとめて』）",
			"//smlint:allow exhaustive 理由: ... → 通す",
		},
	})

	rec.Add(expkit.Variant{
		Name:     "このリポジトリ全体に当てた結果（対応前）",
		Accident: true,
		Counters: map[string]int64{"findings": 2},
		Notes: []string{
			"internal/repo/guard.go: SQL 種別(kind)の switch が kindInsert/kindOther を明示していなかった。" +
				"挙動は正しい（テナントの目印は switch より前で全 kind に要求済み）が、意図が暗黙だった。" +
				"→ 空の case kindInsert, kindOther を足して明示。新しい kind を足すと再び検出される形に。",
			"internal/pklab/lab.go: ネストした switch が BigintAuto を欠いていた。だが BigintAuto は" +
				"外側 switch の case で処理済みで、この default 側には到達しない（構文では追えない絞り込み）。" +
				"→ 理由つき逃げ道で通す（検出の限界を正直に残す）。",
		},
	})
	rec.Add(expkit.Variant{
		Name:     "対応後",
		Counters: map[string]int64{"findings": int64(len(exhaustiveFindings(t)))},
		Notes:    []string{"repo 全体で exhaustive の指摘 0 件（本物は明示化・限界は理由つき逃げ道）"},
	})

	rec.Scope(
		"Go の構文＋型情報だけ（go/analysis）。実行時は見ない",
		"enum の判定＝定義パッケージに同じ named 整数/文字列型の定数が2つ以上",
		"default のある switch は対象外。case が定数でない（範囲・式）ものは追わない",
	)
	rec.Uncertain(
		"ネストした switch の絞り込み（外側 case で除外済みの値）は追えない（pklab がその例・逃げ道で対応）",
		"別パッケージの enum でも、その型の定数がエクスポートされていれば集められる。未エクスポートは同一パッケージ内のみ",
		"iota で歯抜けの値や、型変換で作った値（Status(99)）は case では追えない",
	)
	rec.Artifact(
		"internal/lint/exhaustive.go: 網羅性の検査。Doc に何を見て何を見ないかを明記",
		"cmd/sqllint に自動的に組み込まれる（Analyzers() に追加済み）",
	)
	rec.Next("EXP-68 エラー分類（一時/恒久）の型付けと取りこぼし検出")

	files, err := rec.Save(strings.Join([]string{
		"switch の網羅は Go の型システムでは強制できないが、go/analysis で検出できる。",
		"このリポジトリに当てて本物の暗黙 case 1件を明示化した。ネスト絞り込みの限界は逃げ道で正直に残す。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

// exhaustiveFindings は sqllint を repo 全体に当てて exhaustive の指摘だけ数える。
func exhaustiveFindings(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sqllint")
	build := exec.Command("go", "build", "-o", bin, "github.com/aq35/sample_manual/cmd/sqllint")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("sqllint をビルドできない: %v\n%s", err, out)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "./...")
	cmd.Dir = root
	out, _ := cmd.CombinedOutput()
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "網羅していない") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}
