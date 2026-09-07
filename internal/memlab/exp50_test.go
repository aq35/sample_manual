package memlab_test

// EXP-50: Go の変数・型ごとのメモリ単価。
//
//	go test ./internal/memlab/ -run TestEXP50 -v   （DB 不要）
//
// 「2GB のタスクに何本 / 何件載るか」を見積もる土台。型ごとに「1個あたり何バイト」を実測して
// 正規化し、接続予算 1.2GB に何個載るかまで出す。静的サイズ（unsafe.Sizeof）と、実確保の
// ヒープ増分（ヘッダ・アライン・map のオーバーヘッド込み）の両方を並べる。

import (
	"context"
	"testing"
	"unsafe"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/memlab"
)

func TestEXP50_型ごとのメモリ単価(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-50", "memory-unit-cost",
		"型ごとの1個あたりバイトを実測し、2GB 予算に載る個数へ正規化する")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) int64 は 8B、ポインタ/参照は 8B、空 struct{} は 0B（静的サイズ）。 " +
			"2) 実確保のヒープ増分は、slice 要素はほぼ要素サイズ、map エントリは要素より大きい" +
			"（バケット・ハッシュのオーバーヘッド）。 " +
			"3) goroutine スタックは 1本あたり数 KB（EXP-31 の接続コストの下限と整合）。 " +
			"4) 予算 1.2GB に載る個数 = 予算 ÷ 1個あたりバイト。")

	const budget = 1.2 * 1024 * 1024 * 1024 // 接続/データに使える現実的な予算（EXP-31）

	// ---- 静的サイズ（unsafe.Sizeof）----
	var (
		i64  int64
		ptr  *int64
		str  string // ヘッダ 16B（ptr+len）。本体は別
		sl   []byte // ヘッダ 24B（ptr+len+cap）
		ss   memlab.SmallStruct
		row  memlab.Row
		emp  struct{}
	)
	staticI64 := int64(unsafe.Sizeof(i64))
	staticPtr := int64(unsafe.Sizeof(ptr))
	staticStrHdr := int64(unsafe.Sizeof(str))
	staticSliceHdr := int64(unsafe.Sizeof(sl))
	staticSmall := int64(unsafe.Sizeof(ss))
	staticRow := int64(unsafe.Sizeof(row))
	staticEmpty := int64(unsafe.Sizeof(emp))

	rec.Add(expkit.Variant{
		Name: "静的サイズ（unsafe.Sizeof）: 変数そのものが占める幅",
		Counters: map[string]int64{
			"int64_B": staticI64, "pointer_B": staticPtr, "string_header_B": staticStrHdr,
			"slice_header_B": staticSliceHdr, "small_struct_B": staticSmall,
			"row_struct_B": staticRow, "empty_struct_B": staticEmpty,
		},
		Notes: []string{"string=ヘッダ16B（本体は別ヒープ）/ slice=ヘッダ24B / small_struct=8+4+1→アライン16B"},
	})

	// ---- 実確保のヒープ増分（1個あたり）----
	const n = 1_000_000

	// int64 を n 個（slice に）
	perInt64 := memlab.HeapPerItem(n, func() {
		s := make([]int64, n)
		memlab.Keep(s)
	})
	// ポインタ *int64 を n 個（各要素が別確保 → 8B ヘッダ + 指す先 8B ≒ 16B超）
	perPtr := memlab.HeapPerItem(n, func() {
		s := make([]*int64, n)
		for i := range s {
			v := int64(i)
			s[i] = &v
		}
		memlab.Keep(s)
	})
	// SmallStruct を n 個（値・連続）
	perSmall := memlab.HeapPerItem(n, func() {
		s := make([]memlab.SmallStruct, n)
		memlab.Keep(s)
	})
	// 文字列リテラルを n 個（★本体は静的領域を共有 → ヘッダ16B だけが per-item）
	perStrLiteral := memlab.HeapPerItem(n, func() {
		s := make([]string, n)
		for i := range s {
			s[i] = "worker-000123" // 同じリテラル → 本体は1つを共有
		}
		memlab.Keep(s)
	})
	// 別々にヒープ確保した文字列 n 個（DB から読んだ値の代表・本体が per-item で載る）
	perStrHeap := memlab.HeapPerItem(n, func() {
		s := make([]string, n)
		b := []byte("worker-000000")
		for i := range s {
			b[6] = byte('0' + (i/100000)%10)
			b[7] = byte('0' + (i/10000)%10)
			b[8] = byte('0' + (i/1000)%10)
			s[i] = string(b) // 変換でヒープに本体をコピー → 各要素が別本体
		}
		memlab.Keep(s)
	})
	// map[int64]int64 のエントリ n 個
	const nm = 500_000
	perMapEntry := memlab.HeapPerItem(nm, func() {
		m := make(map[int64]int64, nm)
		for i := 0; i < nm; i++ {
			m[int64(i)] = int64(i)
		}
		memlab.Keep(m)
	})

	rec.Add(expkit.Variant{
		Name: "実確保のヒープ増分: 1個あたりバイト（ヘッダ・アライン・map 込み）",
		Metrics: map[string]float64{
			"int64_slice": perInt64, "pointer_slice": perPtr, "small_struct_slice": perSmall,
			"string_literal_共有": perStrLiteral, "string_heap_別本体": perStrHeap,
			"map_int64_int64_entry": perMapEntry,
		},
		Notes: []string{
			"int64 slice ≒ 8B / *int64 は指す先が別確保で ≒ " + f1(perPtr) + "B / " +
				"map エントリは要素 16B より大きい ≒ " + f1(perMapEntry) + "B（バケットのオーバーヘッド）",
			"★文字列リテラルは本体を共有し per-item ≒ " + f1(perStrLiteral) + "B（ヘッダのみ）。" +
				"DB から読んだ別本体の文字列は ≒ " + f1(perStrHeap) + "B（ヘッダ＋本体）",
		},
	})

	// ---- goroutine スタック単価 ----
	const ng = 20_000
	perGoroutine := memlab.GoroutineStackPerItem(ng)
	rec.Add(expkit.Variant{
		Name:     "goroutine スタック単価: 1本あたり実バイト（park 中）",
		Metrics:  map[string]float64{"stack_bytes_per_goroutine": perGoroutine},
		Counters: map[string]int64{"goroutines": ng},
		Notes:    []string{"1本 ≒ " + f1(perGoroutine) + "B。EXP-31 の『接続下限 ~2.6KB』と整合（初期スタック 2KB 近辺）"},
	})

	// ---- 予算 1.2GB に載る個数（正規化）----
	rec.Add(expkit.Variant{
		Name: "予算 1.2GB に載る個数（= 予算 ÷ 1個あたり）",
		Counters: map[string]int64{
			"int64_件":         memlab.FitInBudget(budget, perInt64),
			"small_struct_件":  memlab.FitInBudget(budget, perSmall),
			"string_heap_件":   memlab.FitInBudget(budget, perStrHeap),
			"map_entry_件":     memlab.FitInBudget(budget, perMapEntry),
			"goroutine_本":     memlab.FitInBudget(budget, perGoroutine),
		},
		Notes: []string{"『2GB あるから何件でも』ではない。1件の単価 × 件数が予算を超えたら OOM"},
	})

	t.Logf("static: int64=%d ptr=%d strHdr=%d sliceHdr=%d small=%d row=%d empty=%d",
		staticI64, staticPtr, staticStrHdr, staticSliceHdr, staticSmall, staticRow, staticEmpty)
	t.Logf("heap/item: int64=%.1f ptr=%.1f small=%.1f strLit=%.1f strHeap=%.1f mapEntry=%.1f goroutine=%.1f",
		perInt64, perPtr, perSmall, perStrLiteral, perStrHeap, perMapEntry, perGoroutine)

	// ---- 検証 ----
	if staticI64 != 8 || staticPtr != 8 {
		t.Errorf("静的サイズが想定外: int64=%d ptr=%d（8 のはず）", staticI64, staticPtr)
	}
	if staticEmpty != 0 {
		t.Errorf("空 struct{} が 0B でない: %d", staticEmpty)
	}
	if staticStrHdr != 16 || staticSliceHdr != 24 {
		t.Errorf("ヘッダサイズが想定外: string=%d slice=%d（16/24 のはず）", staticStrHdr, staticSliceHdr)
	}
	if perInt64 < 6 || perInt64 > 12 { // ほぼ 8B
		t.Errorf("int64 slice の実単価が想定外: %.1f（~8 のはず）", perInt64)
	}
	if perMapEntry <= perSmall {
		t.Errorf("map エントリが値 struct より軽い（重いはず）: map=%.1f small=%.1f", perMapEntry, perSmall)
	}
	if perStrHeap <= perStrLiteral {
		t.Errorf("別本体の文字列がリテラル共有より重くない（重いはず）: heap=%.1f literal=%.1f", perStrHeap, perStrLiteral)
	}
	if perGoroutine < 1000 { // 初期スタックは KB オーダー
		t.Errorf("goroutine スタックが軽すぎる: %.1f", perGoroutine)
	}

	rec.Scope(
		"純 Go（DB 不要）/ 実確保 100万件・map 50万件・goroutine 2万本 / このホストの GC 設定",
		"実単価 = runtime.HeapAlloc の GC 後増分 ÷ 個数。map は bucket オーバーヘッド込み",
		"予算 1.2GB は EXP-31 の『2GB のうち接続/データに使える現実的な枠』",
	)
	rec.Uncertain(
		"実単価は Go バージョン・GC・アロケータで上下する（絶対値でなくオーダーで使う）",
		"string/[]byte はヘッダと本体が別。本体が大きいと『本体バイト＋ヘッダ』で効く",
		"map は要素数で段階的にリサイズする（負荷率で単価が変わる）。ここは 1点の代表値",
		"RSS はこれに加えてランタイム・GC の余白が乗る。実運用は単価の 2〜3 倍を見込む（EXP-31 の安全率）",
	)
	rec.Artifact(
		"internal/memlab: HeapPerItem / GoroutineStackPerItem / FitInBudget（メモリ単価の測定器）",
		"docs/reference-numbers.md: 正規化した早見表（メモリ単価の節）",
	)
	rec.Next("EXP-51 ループとアロケーションの正規化コスト")

	files, err := rec.Save(
		"型ごとのメモリ単価を実測した。静的サイズは int64/ポインタ=8B・string ヘッダ16B・slice ヘッダ24B・" +
			"空 struct=0B。実確保は int64 slice ≒ 8B、map エントリは要素より重い（バケット分）。文字列は" +
			"リテラルなら本体を共有しヘッダ16B だけ、DB から読んだ別本体なら本体ぶん重い。goroutine は" +
			"1本 KB オーダー（EXP-31 の接続下限と整合）。『2GB あるから無限』ではなく、1件の単価 × 件数が" +
			"予算を超えれば OOM。見積もりは単価 × 件数、実運用は単価の 2〜3 倍を安全率で見込む。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func f1(v float64) string {
	// 小数1桁の簡易整形（依存を増やさない）
	n := int64(v*10 + 0.5)
	return itoa(int(n/10)) + "." + itoa(int(n%10))
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
