// コマンド sqllint は、このリポジトリの実験で分かった事故のうち
// 構文で見つかるものを検出する（EXP-9）。
//
//	go run ./cmd/sqllint ./...
//
// 各検査が「何を見ていて、何を見ていないか」は internal/lint の Doc を読むこと。
// 逃げ道は理由つきのコメントで、使うと監査に残る:
//
//	//smlint:allow rawdb 理由: 移行中。2026-12 までに repo.Scope へ置き換える
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/aq35/sample_manual/internal/lint"
)

func main() {
	multichecker.Main(lint.Analyzers()...)
}
