package lint

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// RawDB: 生の *sql.DB を、DB を知らないはずの層へ渡していないか。
//
// 見ているもの: 関数の引数・構造体のフィールドの型
// 見ていないもの: interface 経由で渡している場合、any に包んだ場合
var RawDB = &analysis.Analyzer{
	Name: "rawdb",
	Doc: "生の *sql.DB を domain/service/handler へ渡していないかを見る。" +
		"テナントに束縛したハンドル（repo.Scope）を配るのが前提。" +
		"interface に包んで渡した場合は検出できない。",
	Run: runRawDB,
}

func runRawDB(pass *analysis.Pass) (any, error) {
	if !isDomainPackage(pass.Pkg.Path()) {
		return nil, nil
	}
	isSQLDB := func(t types.Type) bool {
		p, ok := t.(*types.Pointer)
		if !ok {
			return false
		}
		named, ok := p.Elem().(*types.Named)
		if !ok {
			return false
		}
		obj := named.Obj()
		return obj != nil && obj.Pkg() != nil &&
			obj.Pkg().Path() == "database/sql" && obj.Name() == "DB"
	}

	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Type.Params == nil {
					return true
				}
				for _, p := range x.Type.Params.List {
					if tv, ok := pass.TypesInfo.Types[p.Type]; ok && isSQLDB(tv.Type) {
						report(pass, "rawdb", p.Pos(),
							"この層に生の *sql.DB を渡している（テナントに束縛したハンドルを渡すこと）")
					}
				}
			case *ast.StructType:
				for _, fld := range x.Fields.List {
					if tv, ok := pass.TypesInfo.Types[fld.Type]; ok && isSQLDB(tv.Type) {
						report(pass, "rawdb", fld.Pos(),
							"この層の構造体が生の *sql.DB を持っている")
					}
				}
			}
			return true
		})
	}
	return nil, nil
}

// TxExternalCall: トランザクションの中で外部を呼んでいないか。
//
// 見ているもの: Tx / WithTx / Transaction / InTx に渡した関数リテラルの中の
//
//	net/http・外部 API らしき呼び出し
//
// 見ていないもの: 別関数に切り出された呼び出し（関数をまたぐ追跡はしない）
var TxExternalCall = &analysis.Analyzer{
	Name: "txhttp",
	Doc: "トランザクションの中で HTTP など外部の呼び出しをしていないかを見る。" +
		"やり直しが二重送信になり、接続も占有する。" +
		"別関数に切り出された呼び出しは検出できない。",
	Run: runTxExternal,
}

var txFuncNames = map[string]bool{
	"Tx": true, "WithTx": true, "Transaction": true, "InTx": true, "RunInTx": true,
}

func runTxExternal(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !txFuncNames[selectorName(call.Fun)] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				ast.Inspect(lit, func(in ast.Node) bool {
					inner, ok := in.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isExternalCall(pass, inner) {
						report(pass, "txhttp", inner.Pos(),
							"トランザクションの中で外部を呼んでいる（やり直しが二重送信になる。接続も占有する）")
					}
					return true
				})
			}
			return true
		})
	}
	return nil, nil
}

func isExternalCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	// net/http のパッケージ関数（http.Get など）
	if id, ok := sel.X.(*ast.Ident); ok {
		if obj := pass.TypesInfo.Uses[id]; obj != nil {
			if pkg, ok := obj.(*types.PkgName); ok && pkg.Imported().Path() == "net/http" {
				return true
			}
		}
	}
	// クライアントのメソッド（c.Do / client.Get / client.Post ...）
	switch sel.Sel.Name {
	case "Do", "Get", "Post", "PostForm", "Head", "Put", "Patch", "Delete", "Call", "Invoke":
		if tv, ok := pass.TypesInfo.Types[sel.X]; ok {
			if strings.Contains(tv.Type.String(), "net/http") ||
				strings.Contains(tv.Type.String(), "grpc") {
				return true
			}
		}
	}
	return false
}

// RowsAffected: UPDATE / DELETE の影響行数を捨てていないか。
//
// 見ているもの: Exec / ExecContext の戻り値を `_` で捨てている呼び出しで、
//
//	引数の文字列が UPDATE / DELETE で始まるもの
//
// 見ていないもの: SQL を変数で組み立てている場合（定数へ辿れるものだけ見る）
var RowsAffected = &analysis.Analyzer{
	Name: "rowsaffected",
	Doc: "UPDATE / DELETE の影響行数を捨てていないかを見る。" +
		"WHERE を間違えても MySQL はエラーを返さないので、宣言と突き合わせるしかない。" +
		"SQL を動的に組み立てている場合は検出できない。",
	Run: runRowsAffected,
}

func runRowsAffected(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			if len(assign.Rhs) != 1 {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			name := selectorName(call.Fun)
			if name != "Exec" && name != "ExecContext" {
				return true
			}
			sql, ok := firstStringLit(call.Args)
			if !ok {
				return true
			}
			up := strings.ToUpper(strings.TrimSpace(sql))
			if !strings.HasPrefix(up, "UPDATE") && !strings.HasPrefix(up, "DELETE") {
				return true
			}
			// 結果を捨てているか（_, err = db.Exec(...)）
			if len(assign.Lhs) > 0 {
				if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "_" {
					report(pass, "rowsaffected", call.Pos(),
						"UPDATE/DELETE の影響行数を捨てている（何行に当たるはずかを宣言して確かめること）")
				}
			}
			return true
		})
		// db.Exec(...) を式文として書き捨てている場合
		ast.Inspect(f, func(n ast.Node) bool {
			stmt, ok := n.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := selectorName(call.Fun)
			if name != "Exec" && name != "ExecContext" {
				return true
			}
			if sql, ok := firstStringLit(call.Args); ok {
				up := strings.ToUpper(strings.TrimSpace(sql))
				if strings.HasPrefix(up, "UPDATE") || strings.HasPrefix(up, "DELETE") {
					report(pass, "rowsaffected", call.Pos(),
						"UPDATE/DELETE の結果をまったく見ていない")
				}
			}
			return true
		})
	}
	return nil, nil
}

// MissingContext: context を渡さない DB 呼び出し。
//
// 見ているもの: Exec / Query / QueryRow / Begin / Prepare（Context の付かない版）
// 見ていないもの: 独自のラッパー越しの呼び出し
var MissingContext = &analysis.Analyzer{
	Name: "nocontext",
	Doc: "context を渡さない DB 呼び出しを見る。" +
		"取り消しも締め切りも効かず、shutdown のときに終われない。",
	Run: runMissingContext,
}

var nonCtxDBMethods = map[string]string{
	"Exec": "ExecContext", "Query": "QueryContext", "QueryRow": "QueryRowContext",
	"Begin": "BeginTx", "Prepare": "PrepareContext", "Ping": "PingContext",
}

func runMissingContext(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := selectorName(call.Fun)
			want, ok := nonCtxDBMethods[name]
			if !ok {
				return true
			}
			sel := call.Fun.(*ast.SelectorExpr)
			tv, ok := pass.TypesInfo.Types[sel.X]
			if !ok {
				return true
			}
			typ := tv.Type.String()
			if !strings.Contains(typ, "database/sql") {
				return true
			}
			report(pass, "nocontext", call.Pos(),
				"context を渡していない DB 呼び出し（%s ではなく %s を使う）", name, want)
			return true
		})
	}
	return nil, nil
}

// LoopQuery: ループの中の単発クエリ（N+1）。
//
// 見ているもの: for / range の本体にある Query / QueryRow / Exec / Get
// 見ていないもの: 関数呼び出しの奥にあるクエリ（呼び出し先までは追わない）
var LoopQuery = &analysis.Analyzer{
	Name: "loopquery",
	Doc: "ループの中で1件ずつ問い合わせていないかを見る（N+1）。" +
		"実測では 200 件で 30 倍以上遅い。呼び出し先の中にあるクエリは検出できない。",
	Run: runLoopQuery,
}

func runLoopQuery(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			var (
				body *ast.BlockStmt
				vars []string
			)
			switch x := n.(type) {
			case *ast.ForStmt:
				body = x.Body
			case *ast.RangeStmt:
				body = x.Body
				vars = identNames(x.Key, x.Value)
			default:
				return true
			}
			ast.Inspect(body, func(in ast.Node) bool {
				call, ok := in.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch selectorName(call.Fun) {
				case "Query", "QueryContext", "QueryRow", "QueryRowContext", "Get", "Exec", "ExecContext":
				default:
					return true
				}
				sel := call.Fun.(*ast.SelectorExpr)
				if tv, ok := pass.TypesInfo.Types[sel.X]; ok {
					t := tv.Type.String()
					if !strings.Contains(t, "database/sql") && !strings.Contains(t, "repo.") {
						return true
					}
				}
				// ★ループ変数が「引数（束縛値）」として使われている場合だけ N+1 とみなす。
				//
				// 実測（このリポジトリ）で分かったこと:
				// 「ループ変数が SQL 文そのもの」という形が大量にある
				//   for _, stmt := range statements { db.ExecContext(ctx, stmt) }
				// これはマイグレーションや後片付けで、行を1件ずつ引いているのではない。
				// 区別しないと、指摘の大半が誤検出になる（43 件中の多数がこれだった）。
				if len(vars) > 0 && !usesLoopVarAsBindArg(call, vars) {
					return true
				}
				report(pass, "loopquery", call.Pos(),
					"ループの中で1件ずつ問い合わせている（まとめて引くこと。N+1 はスロークエリログに出ない）")
				return true
			})
			return true
		})
	}
	return nil, nil
}

func identNames(exprs ...ast.Expr) []string {
	var out []string
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			out = append(out, id.Name)
		}
	}
	return out
}

// usesLoopVarAsBindArg は、ループ変数が「SQL 文以外の引数」に現れるか。
// 第1引数（または context の次）が SQL 文なので、そこは除く。
func usesLoopVarAsBindArg(call *ast.CallExpr, vars []string) bool {
	for i, a := range call.Args {
		if i <= sqlArgIndex(call) {
			continue
		}
		found := false
		ast.Inspect(a, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			for _, v := range vars {
				if id.Name == v {
					found = true
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// sqlArgIndex は SQL 文が入っている引数の位置（Context 付きなら 1、無しなら 0）。
func sqlArgIndex(call *ast.CallExpr) int {
	if strings.HasSuffix(selectorName(call.Fun), "Context") {
		return 1
	}
	return 0
}

// DomainTime: 業務ロジックの中で time.Now() を直に呼んでいないか。
//
// 見ているもの: domain/service などのパッケージでの time.Now()
// 見ていないもの: time.Since / time.Until（計測目的が多いため対象外）
var DomainTime = &analysis.Analyzer{
	Name: "domaintime",
	Doc: "業務ロジックの中で time.Now() を直に呼んでいないかを見る。" +
		"時刻を差し替えられないとテストが書けず、時計のずれも扱えない（EXP-2）。",
	Run: runDomainTime,
}

func runDomainTime(pass *analysis.Pass) (any, error) {
	if !isDomainPackage(pass.Pkg.Path()) {
		return nil, nil
	}
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || selectorName(call.Fun) != "Now" {
				return true
			}
			if receiverText(call.Fun) != "time" {
				return true
			}
			report(pass, "domaintime", call.Pos(),
				"業務ロジックの中で time.Now() を直に呼んでいる（時計を差し替えられる形にすること）")
			return true
		})
	}
	return nil, nil
}
