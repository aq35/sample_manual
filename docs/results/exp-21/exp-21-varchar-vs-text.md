# EXP-21 太い列の重さは型名でなく inline/off-page で決まる・SELECT * の効き

| | |
| --- | --- |
| Experiment | EXP-21 / varchar-vs-text |
| Starting SHA | `15074bf55bf8` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 3KB の memo は VARCHAR でも TEXT でも行に収まり inline に置かれる。だから memo を取らない    全体走査でも narrow より大幅に重い。『TEXT にしただけ』では軽くならない。 2) 24KB の TEXT は行に収まらず off-page になる。行(clustered index)は細いので、memo を取らない    走査は narrow 並みに軽い。→『軽さ』は型名でなくサイズ（inline か off-page か）で決まる。 3) その大きい TEXT を SELECT *（本体込み）で読むと、1行ごとに外へ本体を取りに行き非常に重い。 4) 別表に分ければサイズによらず舐める表は narrow 並みに軽い（EXP-19）。確実なのは分割。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=15074bf55bf8+dirty |
| Started / Ended | 2026-09-07T04:11:47Z / 2026-09-07T04:13:01Z |

## Workload

- `memo_big` = 約24KB(off-page)
- `memo_small` = 約3KB(inline)
- `range_rows` = 5000
- `rows` = 30000

## Results

### 全体走査(COUNT・memo取らない): VARCHAR 3KB（inline・太い） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 116.562 |

### 全体走査(COUNT・memo取らない): TEXT 3KB（これも inline・太い） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 147.275 |

- VARCHAR 3KB 116.562431ms / TEXT 3KB 147.275453ms（どちらも inline で narrow より大幅に重い。TEXT にしても軽くならない）

### 全体走査(COUNT・memo取らない): TEXT 24KB（off-page・行は細い） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 10.997 |

- TEXT 3KB(inline) 147.275453ms → TEXT 24KB(off-page) 10.997588ms（大きいほど外へ出て、舐めは軽い）

### 全体走査(COUNT・memo取らない): narrow（memo 無し・基準） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 10.750 |

### 範囲読み TEXT 24KB: memo 取らない（SELECT id,status） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 4.132 |

- off-page なので本体に触らず軽い

### 範囲読み TEXT 24KB: memo も取る（SELECT * 相当） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 170.344 |

- 取らない 4.132902ms → 取る 170.344066ms（1行ごとに 24KB を外へ取りに行く）

## Verdict

『太い列が重い』の正体は VARCHAR か TEXT かではなく、値が行の中(inline)にあるか外(off-page)にあるか。3KB 程度なら TEXT でも行に載る（inline）ので、TEXT にしても舐めは軽くならない。十分大きい(24KB)TEXT は行の外に出て舐めは軽いが、SELECT * で読むと外を取りに行き重い。→ 確実なのは『別表に分ける（EXP-19）』。同居のまま軽くしたいなら、　 値を off-page になる大きさにでき、かつ普段の一覧で SELECT * しない場合に限る。まず SELECT * をやめる。

## 適用範囲

- MySQL 8.0 / 3万行 / ROW_FORMAT=DYNAMIC / memo 3KB(inline)・24KB(off-page)
- 全体走査は status で絞る COUNT（status 索引は張らない＝行を順に読む）
- 範囲読みは id < 5000 で 5000 行を舐め、drain で全行 Scan する

## 保証しない範囲・未検証

- inline/off-page の境目は行フォーマットとページサイズで動く（DYNAMIC・16KB ページ前提）
- 絶対値はバッファプールに載った状態のもの。ディスクから読むと差はさらに広がる
- VARCHAR も 8KB を超えるほど巨大だと off-page になりうる（今回の VARCHAR(2000)/3KB は inline）

## 再利用できる成果物

- internal/widthlab: サイズ違いの memo を inline/off-page に作り分けた走査と範囲読み
- docs/column-projection.md: VARCHAR/TEXT の使い分けと SELECT * の指針

## 次の実験

- なし

