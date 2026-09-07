# EXP-46 無キャッシュ / TTL / イベント失効の DB 負荷と stale の trade-off

| | |
| --- | --- |
| Experiment | EXP-46 / cache-invalidation |
| Starting SHA | `174e88c41208` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 無キャッシュ: 毎回 DB を読む（DB 読み=読み回数）が stale は 0。 2) TTL: DB 読みは 読み回数/ttl に減るが、書き込み後 ttl まで stale を返しうる。 3) イベント失効: 書き込みで無効化 → 次の読みで1回引き直す。DB 読みは少なく stale は 0。 4) 期限切れ直後の一斉読みは singleflight で1回の DB 読みに畳める（stampede 防止）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=174e88c41208+dirty |
| Started / Ended | 2026-09-07T12:12:11Z / 2026-09-07T12:12:11Z |

## Results

### 無キャッシュ: DB 読み=読み回数・stale=0 — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| db_reads | 100 |
| stale_reads | 0 |

### TTL(10): DB 読み激減・ただし stale を返しうる — OK

| 数えたもの | 値 |
| --- | --- |
| db_reads | 10 |
| stale_reads | 5 |

- DB 読み 100→10 / stale 5 回（書込後 ttl まで古い）

### イベント失効: DB 読み少なく stale=0 — OK

| 数えたもの | 値 |
| --- | --- |
| db_reads | 4 |
| stale_reads | 0 |

- 書込で無効化 → 次読みで1回引く。DB 読み 4・stale 0

### stampede: 期限切れ一斉読み300 → singleflight で DB 読みごく少数 — OK

| 数えたもの | 値 |
| --- | --- |
| db_reads | 1 |
| stampeders | 300 |

- 300 が一斉に読んでも DB 読みは 1 回（in-flight に合流）

## Verdict

キャッシュは DB 負荷を下げるが stale を生む。無キャッシュ=常に新鮮だが毎回 DB、TTL=DB 激減だが書込後 ttl まで古い、イベント失効=書込で無効化して新鮮かつ DB 少。データの stale 許容で選ぶ（許さないならイベント失効＋短めTTL）。期限切れ一斉読みは singleflight で1回に畳む。

## 適用範囲

- 純 Go（DB 不要）/ 論理時計 100 step・TTL=10・書込 3 回 / stampede は 300 goroutine
- stale = 返した値が『その時点の truth』と違う回数。DB 読み = 引き直した回数
- singleflight は golang.org/x/sync（EXP-38 と同じ）

## 保証しない範囲・未検証

- 実際の stale 許容はデータ次第（残高は不可・表示名は数秒可 等）。ここは回数の trade-off を示す
- イベント失効は『書込を確実に無効化に届ける』のが前提（同プロセスなら簡単、跨ぐなら pub/sub・EXP-43）
- TTL とイベント失効は併用できる（短めTTL＋失効で上限保証）。ここは各単体

## 再利用できる成果物

- docs/cache.md: キャッシュ無効化（TTL / イベント失効 / stampede）

## 次の実験

- EXP-47 順序・冪等消費

