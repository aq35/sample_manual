# EXP-50 型ごとの1個あたりバイトを実測し、2GB 予算に載る個数へ正規化する

| | |
| --- | --- |
| Experiment | EXP-50 / memory-unit-cost |
| Starting SHA | `d70dc1d452cc` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) int64 は 8B、ポインタ/参照は 8B、空 struct{} は 0B（静的サイズ）。 2) 実確保のヒープ増分は、slice 要素はほぼ要素サイズ、map エントリは要素より大きい（バケット・ハッシュのオーバーヘッド）。 3) goroutine スタックは 1本あたり数 KB（EXP-31 の接続コストの下限と整合）。 4) 予算 1.2GB に載る個数 = 予算 ÷ 1個あたりバイト。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=d70dc1d452cc+dirty |
| Started / Ended | 2026-09-07T12:37:29Z / 2026-09-07T12:37:29Z |

## Results

### 静的サイズ（unsafe.Sizeof）: 変数そのものが占める幅 — OK

| 数えたもの | 値 |
| --- | --- |
| empty_struct_B | 0 |
| int64_B | 8 |
| pointer_B | 8 |
| row_struct_B | 48 |
| slice_header_B | 24 |
| small_struct_B | 16 |
| string_header_B | 16 |

- string=ヘッダ16B（本体は別ヒープ）/ slice=ヘッダ24B / small_struct=8+4+1→アライン16B

### 実確保のヒープ増分: 1個あたりバイト（ヘッダ・アライン・map 込み） — OK

| 測ったもの | 値 |
| --- | --- |
| int64_slice | 7.966 |
| map_int64_int64_entry | 37.834 |
| pointer_slice | 16.009 |
| small_struct_slice | 16.007 |
| string_heap_別本体 | 32.007 |
| string_literal_共有 | 16.007 |

- int64 slice ≒ 8B / *int64 は指す先が別確保で ≒ 16.0B / map エントリは要素 16B より大きい ≒ 37.8B（バケットのオーバーヘッド）
- ★文字列リテラルは本体を共有し per-item ≒ 16.0B（ヘッダのみ）。DB から読んだ別本体の文字列は ≒ 32.0B（ヘッダ＋本体）

### goroutine スタック単価: 1本あたり実バイト（park 中） — OK

| 数えたもの | 値 |
| --- | --- |
| goroutines | 20000 |

| 測ったもの | 値 |
| --- | --- |
| stack_bytes_per_goroutine | 2046.362 |

- 1本 ≒ 2046.4B。EXP-31 の『接続下限 ~2.6KB』と整合（初期スタック 2KB 近辺）

### 予算 1.2GB に載る個数（= 予算 ÷ 1個あたり） — OK

| 数えたもの | 値 |
| --- | --- |
| goroutine_本 | 629649 |
| int64_件 | 161745781 |
| map_entry_件 | 34056596 |
| small_struct_件 | 80494374 |
| string_heap_件 | 40256250 |

- 『2GB あるから何件でも』ではない。1件の単価 × 件数が予算を超えたら OOM

## Verdict

型ごとのメモリ単価を実測した。静的サイズは int64/ポインタ=8B・string ヘッダ16B・slice ヘッダ24B・空 struct=0B。実確保は int64 slice ≒ 8B、map エントリは要素より重い（バケット分）。文字列はリテラルなら本体を共有しヘッダ16B だけ、DB から読んだ別本体なら本体ぶん重い。goroutine は1本 KB オーダー（EXP-31 の接続下限と整合）。『2GB あるから無限』ではなく、1件の単価 × 件数が予算を超えれば OOM。見積もりは単価 × 件数、実運用は単価の 2〜3 倍を安全率で見込む。

## 適用範囲

- 純 Go（DB 不要）/ 実確保 100万件・map 50万件・goroutine 2万本 / このホストの GC 設定
- 実単価 = runtime.HeapAlloc の GC 後増分 ÷ 個数。map は bucket オーバーヘッド込み
- 予算 1.2GB は EXP-31 の『2GB のうち接続/データに使える現実的な枠』

## 保証しない範囲・未検証

- 実単価は Go バージョン・GC・アロケータで上下する（絶対値でなくオーダーで使う）
- string/[]byte はヘッダと本体が別。本体が大きいと『本体バイト＋ヘッダ』で効く
- map は要素数で段階的にリサイズする（負荷率で単価が変わる）。ここは 1点の代表値
- RSS はこれに加えてランタイム・GC の余白が乗る。実運用は単価の 2〜3 倍を見込む（EXP-31 の安全率）

## 再利用できる成果物

- internal/memlab: HeapPerItem / GoroutineStackPerItem / FitInBudget（メモリ単価の測定器）
- docs/reference-numbers.md: 正規化した早見表（メモリ単価の節）

## 次の実験

- EXP-51 ループとアロケーションの正規化コスト

