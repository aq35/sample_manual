// Package appx は app / usecase / handler 層のための小物ユーティリティ置き場。
//
// ★このパッケージは repo / domain 層から import しない（EXP-9 の layerimport 検査で禁止）。
// 理由は docs/samber-io.md と moio のテストにある:
// 「繋ぎの手数を減らす」道具は上の層に置き、DB 境界の意味（error・context・
// テナント束縛）を薄い汎用型で覆い隠さないため。
//
// 方針:
//   - 汎用の map/slice 操作は samber/lo をそのまま使ってよい（ここで re-export はしない）。
//   - ここに貯めるのは「lo に無い」「この repo で何度も書く」小物だけ。
//   - Go に三項演算子が無いぶんは lo.Ternary / このパッケージの If で埋める。
package appx

import "github.com/samber/lo"

// If は三項演算子の代わり。**両辺を評価する**ので、副作用のある式には使わない。
//
//	label := appx.If(online, "稼働", "停止")
//
// 副作用や高コストな式は IfF（遅延評価）を使う。
func If[T any](cond bool, ifTrue, ifFalse T) T {
	return lo.Ternary(cond, ifTrue, ifFalse)
}

// IfF は遅延評価版。選ばれた側の関数だけを呼ぶ。
//
//	conn := appx.IfF(useReplica, openReplica, openPrimary)
func IfF[T any](cond bool, ifTrue, ifFalse func() T) T {
	return lo.TernaryF(cond, ifTrue, ifFalse)
}

// Coalesce は最初の非ゼロ値を返す（環境変数のフォールバック等）。
//
//	port := appx.Coalesce(os.Getenv("PORT"), "8080")
func Coalesce[T comparable](vals ...T) T {
	var zero T
	for _, v := range vals {
		if v != zero {
			return v
		}
	}
	return zero
}

// KeyByID は「ID を持つ要素」のスライスを、ID をキーにした map にする。
// N+1 を避けてまとめて引いたあと、元の順序へ引き当てるのに使う（GetMany の後段）。
func KeyByID[T any, K comparable](items []T, id func(T) K) map[K]T {
	out := make(map[K]T, len(items))
	for _, it := range items {
		out[id(it)] = it
	}
	return out
}

// Partition は述語で2つに分ける（採用/棄却、成功/失敗など）。
func Partition[T any](items []T, pred func(T) bool) (yes, no []T) {
	for _, it := range items {
		if pred(it) {
			yes = append(yes, it)
		} else {
			no = append(no, it)
		}
	}
	return yes, no
}

// ChunkIDs は IN 句の上限に合わせて ID を分割する（GetMany を安全に回すため）。
// lo.Chunk でも同じだが、0 や負の size を安全側（1固まり）に倒すのが違い。
func ChunkIDs[T any](ids []T, size int) [][]T {
	if size <= 0 {
		return [][]T{ids}
	}
	return lo.Chunk(ids, size)
}
