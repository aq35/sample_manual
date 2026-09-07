// Package looplab は EXP-51（ループとアロケーションの正規化コスト）の実験本体。
//
// 「1回のループで何が起きているか」をコストで見える化する。同じ結果でも、確保の仕方で
// allocs/op（GC 圧）と ns/op が桁で変わる。append の事前確保・strings.Builder・map の事前
// サイズ指定・interface boxing の有無を、testing.Benchmark で正規化して測る。
package looplab

import "strings"

// BuildAppendNoPrealloc は nil から append で n 件積む（容量が足りず何度も再確保・コピー）。
func BuildAppendNoPrealloc(n int) []int {
	var s []int
	for i := 0; i < n; i++ {
		s = append(s, i)
	}
	return s
}

// BuildAppendPrealloc は make(,0,n) で容量を先に取り append する（再確保なし）。
func BuildAppendPrealloc(n int) []int {
	s := make([]int, 0, n)
	for i := 0; i < n; i++ {
		s = append(s, i)
	}
	return s
}

// BuildIndex は make(,n) して添字代入する（append のチェックも無い）。
func BuildIndex(n int) []int {
	s := make([]int, n)
	for i := 0; i < n; i++ {
		s[i] = i
	}
	return s
}

// ConcatPlus は文字列を += で連結する（毎回新しい文字列を確保・コピー → O(n^2) バイト）。
func ConcatPlus(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

// ConcatBuilder は strings.Builder で連結する（内部バッファを伸ばすだけ）。
func ConcatBuilder(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}

// SumSlice はループ本体の素コスト（要素を1回触るだけ）の基準。
func SumSlice(s []int) int {
	total := 0
	for _, v := range s {
		total += v
	}
	return total
}

// FillMapNoSize はサイズ未指定の map に n 件入れる（負荷率超過のたびに rehash）。
func FillMapNoSize(n int) map[int]int {
	m := map[int]int{}
	for i := 0; i < n; i++ {
		m[i] = i
	}
	return m
}

// FillMapSized は make(map,n) と容量を先に伝えて n 件入れる（rehash を避ける）。
func FillMapSized(n int) map[int]int {
	m := make(map[int]int, n)
	for i := 0; i < n; i++ {
		m[i] = i
	}
	return m
}

// BoxAny は int を any に入れて積む（要素ごとに boxing のヒープ確保が起きうる）。
func BoxAny(n int) []any {
	s := make([]any, 0, n)
	for i := 0; i < n; i++ {
		s = append(s, i)
	}
	return s
}

// BuildTyped は同じ件数を型つき []int で積む（boxing 無し・比較用）。
func BuildTyped(n int) []int {
	s := make([]int, 0, n)
	for i := 0; i < n; i++ {
		s = append(s, i)
	}
	return s
}
