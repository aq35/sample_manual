package looplab_test

// EXP-51: ループとアロケーションの正規化コスト。
//
//	go test ./internal/looplab/ -run TestEXP51 -v   （DB 不要）
//
// 同じ結果でも「確保の仕方」で ns/op と allocs/op（GC 圧）が桁で変わる。append の事前確保・
// strings.Builder・map のサイズ指定・interface boxing を testing.Benchmark で測り、
// 「1件あたり ns / 1操作あたり alloc」に正規化する。ループ本体の素コストも基準として置く。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/looplab"
)

func TestEXP51_ループとアロケーションのコスト(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-51", "loop-alloc-cost",
		"確保の仕方で ns/op・allocs/op が桁で変わることを正規化して測る")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) append は事前確保（make(,0,n)）すると再確保コピーが消え、alloc が多数→1 に、ns も下がる。 " +
			"2) 文字列連結は += が O(n^2) バイトで遅く alloc も多い。strings.Builder は速く alloc 少。 " +
			"3) map は make(map,n) とサイズを伝えると rehash が減り速い。 " +
			"4) int を any に入れる(boxing)と要素ごとに確保が増える。型つき slice は 0 に近い。 " +
			"5) ループ本体の素コスト（要素を1回触る）は 1件 1ns 前後の下限。")

	const n = 10_000

	// --- ① append: 事前確保の有無 ---
	rNo := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.BuildAppendNoPrealloc(n)
		}
	})
	rPre := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.BuildAppendPrealloc(n)
		}
	})
	rIdx := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.BuildIndex(n)
		}
	})

	rec.Add(expkit.Variant{
		Name:     "append 事前確保なし（再確保コピーが多発）",
		Accident: true,
		Metrics:  map[string]float64{"ns_per_op": nsOp(rNo), "ns_per_item": nsItem(rNo, n), "bytes_per_op": bOp(rNo)},
		Counters: map[string]int64{"allocs_per_op": rNo.AllocsPerOp()},
		Notes:    []string{"nil から append。容量超過のたびに確保＋全コピー → allocs/op=" + itoa64(rNo.AllocsPerOp())},
	})
	rec.Add(expkit.Variant{
		Name:     "append 事前確保 make(,0,n)（再確保なし）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rPre), "ns_per_item": nsItem(rPre, n), "bytes_per_op": bOp(rPre)},
		Counters: map[string]int64{"allocs_per_op": rPre.AllocsPerOp()},
		Notes:    []string{"allocs/op=" + itoa64(rPre.AllocsPerOp()) + "（1回の確保だけ）"},
	})
	rec.Add(expkit.Variant{
		Name:     "make(,n)＋添字代入（append チェックも無い）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rIdx), "ns_per_item": nsItem(rIdx, n), "bytes_per_op": bOp(rIdx)},
		Counters: map[string]int64{"allocs_per_op": rIdx.AllocsPerOp()},
	})

	// --- ② 文字列連結: += vs Builder ---
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "x"
	}
	rPlus := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.ConcatPlus(parts)
		}
	})
	rBuild := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.ConcatBuilder(parts)
		}
	})
	rec.Add(expkit.Variant{
		Name:     "文字列連結 += （毎回新確保・O(n^2) バイト）",
		Accident: true,
		Metrics:  map[string]float64{"ns_per_op": nsOp(rPlus), "bytes_per_op": bOp(rPlus)},
		Counters: map[string]int64{"allocs_per_op": rPlus.AllocsPerOp()},
		Notes:    []string{"+= は毎回新しい文字列を作る。n=" + itoa(n) + " で alloc=" + itoa64(rPlus.AllocsPerOp())},
	})
	rec.Add(expkit.Variant{
		Name:     "文字列連結 strings.Builder（内部バッファを伸ばす）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rBuild), "bytes_per_op": bOp(rBuild)},
		Counters: map[string]int64{"allocs_per_op": rBuild.AllocsPerOp()},
		Notes:    []string{"+= 比 " + speedup(rPlus, rBuild) + "x 速い・alloc=" + itoa64(rBuild.AllocsPerOp())},
	})

	// --- ③ map: サイズ指定の有無 ---
	rMapNo := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.FillMapNoSize(n)
		}
	})
	rMapSz := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.FillMapSized(n)
		}
	})
	rec.Add(expkit.Variant{
		Name:     "map サイズ未指定（負荷率超過で rehash）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rMapNo), "bytes_per_op": bOp(rMapNo)},
		Counters: map[string]int64{"allocs_per_op": rMapNo.AllocsPerOp()},
	})
	rec.Add(expkit.Variant{
		Name:     "map make(map,n)（rehash を避ける）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rMapSz), "bytes_per_op": bOp(rMapSz)},
		Counters: map[string]int64{"allocs_per_op": rMapSz.AllocsPerOp()},
		Notes:    []string{"サイズ指定で " + speedup(rMapNo, rMapSz) + "x 速い"},
	})

	// --- ④ interface boxing の有無 ---
	rBox := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.BoxAny(n)
		}
	})
	rTyped := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.BuildTyped(n)
		}
	})
	rec.Add(expkit.Variant{
		Name:     "any に int を積む（要素ごとに boxing 確保）",
		Accident: true,
		Metrics:  map[string]float64{"ns_per_op": nsOp(rBox), "bytes_per_op": bOp(rBox)},
		Counters: map[string]int64{"allocs_per_op": rBox.AllocsPerOp()},
		Notes:    []string{"allocs/op=" + itoa64(rBox.AllocsPerOp()) + "（boxing が要素数ぶん増える）"},
	})
	rec.Add(expkit.Variant{
		Name:     "型つき []int に積む（boxing 無し）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rTyped), "bytes_per_op": bOp(rTyped)},
		Counters: map[string]int64{"allocs_per_op": rTyped.AllocsPerOp()},
	})

	// --- ⑤ ループ本体の素コスト（基準）---
	base := looplab.BuildIndex(n)
	rSum := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = looplab.SumSlice(base)
		}
	})
	rec.Add(expkit.Variant{
		Name:     "ループ本体の素コスト（要素を1回触るだけ・基準）",
		Metrics:  map[string]float64{"ns_per_op": nsOp(rSum), "ns_per_item": nsItem(rSum, n)},
		Counters: map[string]int64{"allocs_per_op": rSum.AllocsPerOp()},
		Notes:    []string{"1件 ≒ " + f2(nsItem(rSum, n)) + " ns。これがループの下限。DB 往復(0.1ms〜)は 1件でこの数千倍"},
	})

	t.Logf("append: no=%.0fns/%dalloc pre=%.0fns/%dalloc idx=%.0fns",
		nsOp(rNo), rNo.AllocsPerOp(), nsOp(rPre), rPre.AllocsPerOp(), nsOp(rIdx))
	t.Logf("concat: plus=%.0fns/%dalloc builder=%.0fns/%dalloc",
		nsOp(rPlus), rPlus.AllocsPerOp(), nsOp(rBuild), rBuild.AllocsPerOp())
	t.Logf("map: no=%.0fns sized=%.0fns / box=%.0fns/%dalloc typed=%.0fns/%dalloc / sum=%.1fns",
		nsOp(rMapNo), nsOp(rMapSz), nsOp(rBox), rBox.AllocsPerOp(), nsOp(rTyped), rTyped.AllocsPerOp(), nsOp(rSum))

	// ---- 検証 ----
	if rPre.AllocsPerOp() >= rNo.AllocsPerOp() {
		t.Errorf("事前確保で alloc が減っていない: pre=%d no=%d", rPre.AllocsPerOp(), rNo.AllocsPerOp())
	}
	if nsOp(rPre) >= nsOp(rNo) {
		t.Errorf("事前確保が速くない: pre=%.0f no=%.0f", nsOp(rPre), nsOp(rNo))
	}
	if nsOp(rBuild) >= nsOp(rPlus) {
		t.Errorf("Builder が += より速くない: builder=%.0f plus=%.0f", nsOp(rBuild), nsOp(rPlus))
	}
	if bOp(rPlus) <= bOp(rBuild) {
		t.Errorf("+= のバイトが Builder 以下（O(n^2) のはず）: plus=%.0f builder=%.0f", bOp(rPlus), bOp(rBuild))
	}
	if nsOp(rMapSz) >= nsOp(rMapNo) {
		t.Errorf("map サイズ指定が速くない: sized=%.0f no=%.0f", nsOp(rMapSz), nsOp(rMapNo))
	}
	if rBox.AllocsPerOp() <= rTyped.AllocsPerOp() {
		t.Errorf("boxing で alloc が増えていない: box=%d typed=%d", rBox.AllocsPerOp(), rTyped.AllocsPerOp())
	}

	rec.Scope(
		"純 Go（DB 不要）/ n=10000・testing.Benchmark（ns/op・B/op・allocs/op）/ このホストの CPU",
		"ns は相対比較用（このサンドボックス CPU の絶対値は本番と違う）。alloc/バイトは環境非依存に近い",
		"『1件 ns』はループ本体の下限。実処理は DB 往復（EXP-31: 0.1ms〜）が支配的",
	)
	rec.Uncertain(
		"ns/op はマシン依存。桁（10x・O(n^2)）と allocs/op・B/op を主に読む",
		"事前確保は件数が読めるときだけ。読めないなら append の増幅（2 倍成長）に任せてよい",
		"strings.Builder は Grow(n) で更に確保を1回にできる（本実験は素の Builder）",
		"boxing は any/interface・fmt.Sprint・reflect で起きる。ホットパスでは型つきを保つ",
	)
	rec.Artifact(
		"internal/looplab: append/連結/map/boxing の比較関数",
		"docs/reference-numbers.md: 正規化した早見表（ループ・確保の節）",
	)
	rec.Next("（土台の実測はここまで。以降は正規化早見表 docs/reference-numbers.md へ集約）")

	files, err := rec.Save(
		"同じ結果でも確保の仕方で ns と allocs/op が桁で変わる。件数が読めるなら append は make(,0,n) で" +
			"事前確保（再確保コピーを消す）。文字列連結は += でなく strings.Builder（+= は O(n^2) バイト）。" +
			"map は make(map,n) でサイズを伝えて rehash を避ける。ホットパスで int を any に入れない（boxing が" +
			"要素数ぶん確保を生む）。ループ本体の素コストは 1件数 ns で、実処理は DB 往復が支配的。" +
			"『allocs/op を減らす』が GC 圧を下げる最短路。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func nsOp(r testing.BenchmarkResult) float64   { return float64(r.NsPerOp()) }
func bOp(r testing.BenchmarkResult) float64    { return float64(r.AllocedBytesPerOp()) }
func nsItem(r testing.BenchmarkResult, n int) float64 {
	return float64(r.NsPerOp()) / float64(n)
}
func speedup(slow, fast testing.BenchmarkResult) string {
	if fast.NsPerOp() == 0 {
		return "∞"
	}
	return f1(float64(slow.NsPerOp()) / float64(fast.NsPerOp()))
}

func itoa64(n int64) string { return itoa(int(n)) }
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
func f1(v float64) string {
	n := int64(v*10 + 0.5)
	return itoa(int(n/10)) + "." + itoa(int(n%10))
}
func f2(v float64) string {
	n := int64(v*100 + 0.5)
	return itoa(int(n/100)) + "." + pad2(int(n%100))
}
func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
