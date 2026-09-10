package lint

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Exhaustive: enum 的な named 型に対する switch が、全メンバを網羅しているか（default 無しで）。
//
// Go には sum type / enum が無く、`switch status { ... }` で case を1つ書き忘れても
// コンパイルは通る（pending→in_progress→completed に completed を足したのに、
// 既存の switch が古いまま黙って素通りする）。これを構文＋型情報で検出する。
//
// enum とみなすもの: あるパッケージに、同じ named 整数/文字列型の**定数が2つ以上**あるもの。
// 見ているもの: その型を tag に持つ switch。case に現れた定数と、型のメンバ集合を突き合わせる。
// 見ていないもの: default がある switch（意図的に「残りはまとめて」と書いたとみなし、対象外）。
//
//	tag が式（関数呼び出し結果など）でも型が named enum なら見る。case が定数でない（範囲・式）ものは追わない。
var Exhaustive = &analysis.Analyzer{
	Name: "exhaustive",
	Doc: "enum 的な named 型（同じ型の定数が2つ以上）に対する switch が全メンバを網羅しているかを見る。" +
		"Go は case の書き忘れをコンパイルで防げない（sum type が無い）。" +
		"default のある switch は対象外。case が定数でないものは追わない。",
	Run: runExhaustive,
}

func runExhaustive(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil {
				return true
			}
			tv, ok := pass.TypesInfo.Types[sw.Tag]
			if !ok {
				return true
			}
			named, ok := tv.Type.(*types.Named)
			if !ok || !isEnumUnderlying(named) {
				return true
			}
			members := enumMembers(named)
			if len(members) < 2 {
				return true // enum とみなさない
			}
			// default があれば「残りはまとめて」= 意図的とみなす
			covered := map[string]bool{}
			hasDefault := false
			for _, cl := range sw.Body.List {
				cc, ok := cl.(*ast.CaseClause)
				if !ok {
					return true
				}
				if cc.List == nil {
					hasDefault = true
					continue
				}
				for _, e := range cc.List {
					if obj := constObjOf(pass, e); obj != nil {
						covered[obj.Name()] = true
					}
				}
			}
			if hasDefault {
				return true
			}
			var missing []string
			for _, m := range members {
				if !covered[m] {
					missing = append(missing, m)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				report(pass, "exhaustive", sw.Pos(),
					"switch が網羅していない（未処理: %s）。default も無い。case を足すか default を書くこと",
					strings.Join(missing, ", "))
			}
			return true
		})
	}
	return nil, nil
}

// isEnumUnderlying は underlying が整数か文字列か（enum になりうる型か）。
func isEnumUnderlying(named *types.Named) bool {
	b, ok := named.Underlying().(*types.Basic)
	if !ok {
		return false
	}
	info := b.Info()
	return info&types.IsInteger != 0 || info&types.IsString != 0
}

// enumMembers は named 型の定義パッケージにある、その型の定数名を集める。
func enumMembers(named *types.Named) []string {
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return nil
	}
	scope := obj.Pkg().Scope()
	var names []string
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok {
			continue
		}
		if types.Identical(c.Type(), named.Obj().Type()) {
			names = append(names, name)
		}
	}
	return names
}

// constObjOf は case 式が指す定数（`Pending` / `pkg.Pending`）を返す。定数でなければ nil。
func constObjOf(pass *analysis.Pass, e ast.Expr) *types.Const {
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return nil
	}
	if obj, ok := pass.TypesInfo.ObjectOf(id).(*types.Const); ok {
		return obj
	}
	return nil
}
