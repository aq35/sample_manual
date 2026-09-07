# EXP-51 確保の仕方で ns/op・allocs/op が桁で変わることを正規化して測る

| | |
| --- | --- |
| Experiment | EXP-51 / loop-alloc-cost |
| Starting SHA | `d70dc1d452cc` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) append は事前確保（make(,0,n)）すると再確保コピーが消え、alloc が多数→1 に、ns も下がる。 2) 文字列連結は += が O(n^2) バイトで遅く alloc も多い。strings.Builder は速く alloc 少。 3) map は make(map,n) とサイズを伝えると rehash が減り速い。 4) int を any に入れる(boxing)と要素ごとに確保が増える。型つき slice は 0 に近い。 5) ループ本体の素コスト（要素を1回触る）は 1件 1ns 前後の下限。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=d70dc1d452cc+dirty |
| Started / Ended | 2026-09-07T12:37:29Z / 2026-09-07T12:37:44Z |

## Results

### append 事前確保なし（再確保コピーが多発） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 19 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 357626.000 |
| ns_per_item | 15.084 |
| ns_per_op | 150838.000 |

- nil から append。容量超過のたびに確保＋全コピー → allocs/op=19

### append 事前確保 make(,0,n)（再確保なし） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 1 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 81920.000 |
| ns_per_item | 3.277 |
| ns_per_op | 32768.000 |

- allocs/op=1（1回の確保だけ）

### make(,n)＋添字代入（append チェックも無い） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 1 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 81920.000 |
| ns_per_item | 3.519 |
| ns_per_op | 35188.000 |

### 文字列連結 += （毎回新確保・O(n^2) バイト） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 10000 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 53164067.000 |
| ns_per_op | 29036919.000 |

- += は毎回新しい文字列を作る。n=10000 で alloc=10000

### 文字列連結 strings.Builder（内部バッファを伸ばす） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 16 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 46584.000 |
| ns_per_op | 68247.000 |

- += 比 425.5x 速い・alloc=16

### map サイズ未指定（負荷率超過で rehash） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 79 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 591483.000 |
| ns_per_op | 802944.000 |

### map make(map,n)（rehash を避ける） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 33 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 295552.000 |
| ns_per_op | 292083.000 |

- サイズ指定で 2.7x 速い

### any に int を積む（要素ごとに boxing 確保） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 9745 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 241793.000 |
| ns_per_op | 243800.000 |

- allocs/op=9745（boxing が要素数ぶん増える）

### 型つき []int に積む（boxing 無し） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 1 |

| 測ったもの | 値 |
| --- | --- |
| bytes_per_op | 81920.000 |
| ns_per_op | 27918.000 |

### ループ本体の素コスト（要素を1回触るだけ・基準） — OK

| 数えたもの | 値 |
| --- | --- |
| allocs_per_op | 0 |

| 測ったもの | 値 |
| --- | --- |
| ns_per_item | 0.639 |
| ns_per_op | 6394.000 |

- 1件 ≒ 0.64 ns。これがループの下限。DB 往復(0.1ms〜)は 1件でこの数千倍

## Verdict

同じ結果でも確保の仕方で ns と allocs/op が桁で変わる。件数が読めるなら append は make(,0,n) で事前確保（再確保コピーを消す）。文字列連結は += でなく strings.Builder（+= は O(n^2) バイト）。map は make(map,n) でサイズを伝えて rehash を避ける。ホットパスで int を any に入れない（boxing が要素数ぶん確保を生む）。ループ本体の素コストは 1件数 ns で、実処理は DB 往復が支配的。『allocs/op を減らす』が GC 圧を下げる最短路。

## 適用範囲

- 純 Go（DB 不要）/ n=10000・testing.Benchmark（ns/op・B/op・allocs/op）/ このホストの CPU
- ns は相対比較用（このサンドボックス CPU の絶対値は本番と違う）。alloc/バイトは環境非依存に近い
- 『1件 ns』はループ本体の下限。実処理は DB 往復（EXP-31: 0.1ms〜）が支配的

## 保証しない範囲・未検証

- ns/op はマシン依存。桁（10x・O(n^2)）と allocs/op・B/op を主に読む
- 事前確保は件数が読めるときだけ。読めないなら append の増幅（2 倍成長）に任せてよい
- strings.Builder は Grow(n) で更に確保を1回にできる（本実験は素の Builder）
- boxing は any/interface・fmt.Sprint・reflect で起きる。ホットパスでは型つきを保つ

## 再利用できる成果物

- internal/looplab: append/連結/map/boxing の比較関数
- docs/reference-numbers.md: 正規化した早見表（ループ・確保の節）

## 次の実験

- （土台の実測はここまで。以降は正規化早見表 docs/reference-numbers.md へ集約）

