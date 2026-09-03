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
	return strings.Join(strings.Fields(strings.ToLower(b.String())), " ")
}

type statementKind int

const (
	kindSelect statementKind = iota
	kindInsert
	kindUpdate
	kindDelete
	kindOther
)

func kindOf(norm string) statementKind {
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
	kind := kindOf(norm)

	// ① テナントの目印が必ず要る（`WHERE tenant_id = :tenant` など）
	if !opt.allowCrossTenant && !strings.Contains(norm, TenantToken) {
		return fmt.Errorf("%w: %q を書くこと（テナントを跨ぐ操作は DB.Unscoped を使う）", ErrMissingTenant, TenantToken)
	}

	switch kind {
	case kindUpdate, kindDelete:
		// ② WHERE の無い UPDATE / DELETE は通さない
		if !strings.Contains(norm, " where ") {
			return fmt.Errorf("%w: WHERE の無い %s", ErrUnsafeStatement, strings.ToUpper(strings.Fields(norm)[0]))
		}
		// ③ tenant_id が WHERE 側にあること（SET 側だけにあっても意味がない）
		if !opt.allowCrossTenant {
			where := norm[strings.Index(norm, " where "):]
			if !strings.Contains(where, TenantToken) {
				return fmt.Errorf("%w: WHERE 句に %s が無い", ErrMissingTenant, TenantToken)
			}
		}
	case kindSelect:
		// ④ 上限の無い読み出しは、明示的に許可したときだけ通す。
		// 1行取得（QueryRow）と集計は対象外。件数は増えようがないため。
		if !opt.singleRow && !opt.allowUnbounded &&
			!strings.Contains(norm, " limit ") && !strings.Contains(norm, "count(") {
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
