package repo

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// ---- SQL の検査 ----
//
// 文字列を見て判断するので万能ではない（正確にやるならパーサが要る）。
// ここで狙っているのは「うっかり」を止めることで、悪意を止めることではない。
// 実験（TestExperiment_テナント指定を忘れる / WHEREの書き間違い）で起きた事故が、
// この検査で止まることを repo_test.go で確認している。

// TenantToken は SQL に必ず書く目印。`?` ではなくこれを書かせることで、
// 「テナントの値をアプリが渡し忘れる」ことが起こらないようにする。
const TenantToken = ":tenant"

// normalize はコメントと文字列リテラルを潰し、空白をまとめた小文字を返す。
// 検査を文字列一致でやるための下ごしらえ。
func normalize(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	runes := []rune(q)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			b.WriteString(" 'x' ")
			for i++; i < len(runes); i++ {
				if runes[i] == '\\' {
					i++
					continue
				}
				if runes[i] == quote {
					break
				}
			}
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		case c == '\n' || c == '\t' || c == '\r':
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
		}
	}
	// ★小文字化しない。:tenant の判定は大文字小文字を区別する必要がある
	// （fuzz が `:tenAnt':tenant'0` を見つけた。小文字化して判定すると
	//   「トークンはある」と見なされるのに、置換は行われず driver まで届く）。
	return strings.Join(strings.Fields(b.String()), " ")
}

// lower はキーワード判定用の小文字版。
func lower(norm string) string { return strings.ToLower(norm) }

type statementKind int

const (
	kindSelect statementKind = iota
	kindInsert
	kindUpdate
	kindDelete
	kindOther
)

func kindOf(normLower string) statementKind {
	norm := normLower
	switch {
	case strings.HasPrefix(norm, "select"), strings.HasPrefix(norm, "with"):
		return kindSelect
	case strings.HasPrefix(norm, "insert"), strings.HasPrefix(norm, "replace"):
		return kindInsert
	case strings.HasPrefix(norm, "update"):
		return kindUpdate
	case strings.HasPrefix(norm, "delete"):
		return kindDelete
	}
	return kindOther
}

// checkStatement は、テナントに束縛したハンドルから出してよい SQL かを見る。
func checkStatement(q string, opt statementOptions) error {
	norm := normalize(q)
	if norm == "" {
		return fmt.Errorf("%w: 空の SQL", ErrUnsafeStatement)
	}
	low := lower(norm)
	kind := kindOf(low)

	// ① テナントの目印が必ず要る（`WHERE tenant_id = :tenant` など）
	if !opt.allowCrossTenant && !strings.Contains(norm, TenantToken) {
		return fmt.Errorf("%w: %q を書くこと（テナントを跨ぐ操作は DB.Unscoped を使う）", ErrMissingTenant, TenantToken)
	}

	// ①'' 引用符やコメントが閉じていない SQL は通さない（EXP-8 の fuzz が見つけた）。
	// 閉じていないと、その先の :tenant が束縛されずに driver まで届く。
	// そもそも壊れた SQL なので、検査の段階で落とす。
	if unbalancedQuote(q) {
		return fmt.Errorf("%w: 引用符またはコメントが閉じていない", ErrUnsafeStatement)
	}

	// ①' 1回の呼び出しに複数の文を入れない（EXP-8 の fuzz で見つかった抜け道）。
	// driver の既定では実行されないが、DSN に multiStatements=true が付いた瞬間に通る。
	if hasMultipleStatements(low) {
		return fmt.Errorf("%w: 1回の呼び出しに複数の文がある", ErrUnsafeStatement)
	}

	switch kind {
	case kindUpdate, kindDelete:
		// ②' 複数の表に触れる UPDATE / DELETE は通さない（EXP-8 の fuzz で発見）。
		//     :tenant が片方の表にしか掛からず、もう一方が無条件に書き換わりうる。
		if !opt.allowCrossTenant && looksMultiTableWrite(low) {
			return fmt.Errorf("%w: 複数の表を書き換える文（:tenant が全ての表に掛かっている保証が無い）",
				ErrUnsafeStatement)
		}
		// ② WHERE の無い UPDATE / DELETE は通さない
		if !strings.Contains(low, " where ") {
			return fmt.Errorf("%w: WHERE の無い %s", ErrUnsafeStatement, strings.ToUpper(strings.Fields(low)[0]))
		}
		// ③ tenant_id が WHERE 側にあること（SET 側だけにあっても意味がない）
		if !opt.allowCrossTenant {
			where := norm[strings.Index(low, " where "):]
			if !strings.Contains(where, TenantToken) {
				return fmt.Errorf("%w: WHERE 句に %s が無い", ErrMissingTenant, TenantToken)
			}
		}
	case kindSelect:
		// ④ 上限の無い読み出しは、明示的に許可したときだけ通す。
		// 1行取得（QueryRow）と集計は対象外。件数は増えようがないため。
		if !opt.singleRow && !opt.allowUnbounded &&
			!strings.Contains(low, " limit ") && !strings.Contains(low, "count(") {
			return fmt.Errorf("%w: LIMIT の無い SELECT（一覧は Keyset を使う。全件が要るなら AllowUnbounded）", ErrTooManyRows)
		}
	}
	return nil
}

type statementOptions struct {
	allowCrossTenant bool
	allowUnbounded   bool
	singleRow        bool // QueryRow（1行だけ取る）
}

// ---- fuzz（EXP-8）で見つかった抜け道の検出 ----

// hasMultipleStatements は「1回の呼び出しに複数の文が入っているか」。
//
// go-sql-driver は既定で複数文を実行しないが、DSN に multiStatements=true が
// 付いた瞬間に通る。検査側で落としておく。
// normalize 済みの文字列を渡すこと（文字列リテラルとコメントは潰れている）。
func hasMultipleStatements(norm string) bool {
	i := strings.Index(norm, ";")
	if i < 0 {
		return false
	}
	// 末尾のセミコロンだけなら1文
	return strings.TrimSpace(norm[i+1:]) != ""
}

// looksMultiTableWrite は UPDATE / DELETE が複数の表に触れる形か。
//
//	UPDATE a JOIN b ON ... SET b.v = 1 WHERE a.tenant_id = :tenant
//
// :tenant は a にしか掛かっておらず、b は無条件に書き換わる。
// 「:tenant がある」だけでは、テナント境界の保証にならない形。
func looksMultiTableWrite(norm string) bool {
	head := norm
	if i := strings.Index(norm, " where "); i > 0 {
		head = norm[:i]
	}
	if strings.Contains(head, " join ") {
		return true
	}
	// UPDATE a, b SET ... / DELETE a, b FROM ...
	if strings.HasPrefix(head, "update ") {
		if i := strings.Index(head, " set "); i > 0 {
			return strings.Contains(head[:i], ",")
		}
	}
	if strings.HasPrefix(head, "delete ") {
		if i := strings.Index(head, " from "); i > 0 {
			return strings.Contains(head[:i], ",")
		}
	}
	return false
}

// unbalancedQuote は引用符・ブロックコメントが閉じていないか。
//
// fuzz が見つけた形: `:tenant":tenant0`
// 2つめの `:tenant` が閉じていない文字列の中にあるため束縛されず、
// そのまま driver へ渡っていた。壊れた SQL は検査で落とす。
func unbalancedQuote(q string) bool {
	runes := []rune(q)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '\\' {
					i++
					continue
				}
				if runes[i] == quote {
					closed = true
					break
				}
			}
			if !closed {
				return true
			}
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			closed := false
			for i += 2; i+1 < len(runes); i++ {
				if runes[i] == '*' && runes[i+1] == '/' {
					i++
					closed = true
					break
				}
			}
			if !closed {
				return true
			}
		}
	}
	return false
}

// isInsertSelect は INSERT ... SELECT か。
// 元になる SELECT がテナントで絞られているかは、この検査では分からない。
func isInsertSelect(norm string) bool {
	return strings.HasPrefix(norm, "insert") && strings.Contains(norm, " select ")
}

// compiled は1つの SQL 文について、検査結果と書き換え後の形を覚えておくもの。
//
// SQL 文はコード中の定数なので、種類は有限。毎回文字列を舐め直す必要はない。
// （キャッシュ導入前は検査＋束縛に 4.8µs / 2KB かかっていた）
type compiled struct {
	sql      string // :tenant を ? に置き換えたもの
	tenantAt []int  // 最終的な引数列の中で、テナント ID を差し込む位置
	argCount int    // 呼び出し側が渡すべき引数の数
	err      error  // 検査で弾かれた場合の理由
}

var (
	compiledCache sync.Map // map[cacheKey]*compiled
	compiledCount atomic.Int64
)

// compiledCacheMax は覚えておく SQL 文の上限。
// 文字列連結で組み立てる SQL（IN 句の数が可変など）があると種類が増えるので、
// 上限を超えたら覚えるのをやめる（正しさは変わらず、その分だけ毎回舐め直す）。
const compiledCacheMax = 4096

type cacheKey struct {
	sql string
	opt statementOptions
}

func compile(q string, opt statementOptions) *compiled {
	key := cacheKey{sql: q, opt: opt}
	if v, ok := compiledCache.Load(key); ok {
		return v.(*compiled)
	}
	c := &compiled{}
	if err := checkStatement(q, opt); err != nil {
		c.err = err
	} else {
		sqlText, positions, argCount, err := rewriteTenant(q)
		c.sql, c.tenantAt, c.argCount, c.err = sqlText, positions, argCount, err
	}
	if compiledCount.Load() < compiledCacheMax {
		if _, loaded := compiledCache.LoadOrStore(key, c); !loaded {
			compiledCount.Add(1)
		}
	}
	return c
}

// bind は覚えておいた形に、呼び出し側の引数を流し込む。
func (c *compiled) bind(tenant string, args []any) (string, []any, error) {
	if c.err != nil {
		return "", nil, c.err
	}
	if len(args) != c.argCount {
		return "", nil, fmt.Errorf("%w: ? は %d 個だが引数は %d 個", ErrUnsafeStatement, c.argCount, len(args))
	}
	out := make([]any, 0, len(args)+len(c.tenantAt))
	next := 0
	tenantIdx := 0
	total := len(args) + len(c.tenantAt)
	for i := 0; i < total; i++ {
		if tenantIdx < len(c.tenantAt) && c.tenantAt[tenantIdx] == i {
			out = append(out, tenant)
			tenantIdx++
			continue
		}
		out = append(out, args[next])
		next++
	}
	return c.sql, out, nil
}

// rewriteTenant は :tenant を ? に置き換え、その位置を記録する。
func rewriteTenant(q string) (string, []int, int, error) {
	var (
		out       strings.Builder
		positions []int
		argCount  int
		slot      int
		i         int
	)
	runes := []rune(q)
	for i < len(runes) {
		c := runes[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			out.WriteRune(c)
			for i++; i < len(runes); i++ {
				out.WriteRune(runes[i])
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
					out.WriteRune(runes[i])
					continue
				}
				if runes[i] == quote {
					break
				}
			}
			i++
		case c == '?':
			argCount++
			slot++
			out.WriteRune(c)
			i++
		case c == ':' && strings.HasPrefix(string(runes[i:]), TenantToken):
			positions = append(positions, slot)
			slot++
			out.WriteRune('?')
			i += len(TenantToken)
		default:
			out.WriteRune(c)
			i++
		}
	}
	return out.String(), positions, argCount, nil
}

// bindTenant は SQL 中の :tenant を ? に置き換え、その位置にテナント ID を差し込んだ
// 引数列を返す（キャッシュを使わない素の実装。単体テスト用）。
func bindTenant(q string, tenant string, args []any) (string, []any, error) {
	var (
		out   strings.Builder
		bound = make([]any, 0, len(args)+2)
		next  int
		i     int
	)
	runes := []rune(q)
	for i < len(runes) {
		c := runes[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			out.WriteRune(c)
			for i++; i < len(runes); i++ {
				out.WriteRune(runes[i])
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
					out.WriteRune(runes[i])
					continue
				}
				if runes[i] == quote {
					break
				}
			}
			i++
		case c == '?':
			if next >= len(args) {
				return "", nil, fmt.Errorf("%w: ? の数より引数が少ない", ErrUnsafeStatement)
			}
			bound = append(bound, args[next])
			next++
			out.WriteRune(c)
			i++
		case c == ':' && strings.HasPrefix(string(runes[i:]), TenantToken):
			bound = append(bound, tenant)
			out.WriteRune('?')
			i += len(TenantToken)
		default:
			out.WriteRune(c)
			i++
		}
	}
	if next != len(args) {
		return "", nil, fmt.Errorf("%w: 引数が %d 個余っている", ErrUnsafeStatement, len(args)-next)
	}
	return out.String(), bound, nil
}

// ---- 影響行数の検査 ----

// Expect は「この UPDATE / DELETE は何行に当たるはずか」の宣言。
//
// 実験（TestExperiment_WHEREの書き間違い）のとおり、WHERE を間違えても MySQL は
// エラーを返さない。宣言と違えばロールバックする、という形でしか止められない。
type Expect struct {
	Min, Max int64
	name     string
}

var (
	// ExpectOne: ちょうど1行。主キー指定の更新はこれ。
	ExpectOne = Expect{1, 1, "ちょうど1行"}
	// ExpectAtMostOne: 0 か 1 行（無ければ何もしない、を許す）。
	ExpectAtMostOne = Expect{0, 1, "0か1行"}
	// ExpectAny: 検査しない。バッチ更新など、行数が読めないときだけ。
	ExpectAny = Expect{-1, -1, "検査しない"}
)

// ExpectRows はちょうど n 行。
func ExpectRows(n int64) Expect { return Expect{n, n, fmt.Sprintf("ちょうど%d行", n)} }

// ExpectAtMost は最大 n 行（バッチの上限を宣言するのに使う）。
func ExpectAtMost(n int64) Expect { return Expect{0, n, fmt.Sprintf("最大%d行", n)} }

func (e Expect) check(affected int64) error {
	if e.Min < 0 && e.Max < 0 {
		return nil
	}
	if affected < e.Min || affected > e.Max {
		return fmt.Errorf("%w: %s のはずが %d 行", ErrUnexpectedRowCount, e.name, affected)
	}
	return nil
}

func (e Expect) checked() bool { return e.Min >= 0 || e.Max >= 0 }
