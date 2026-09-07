# EXP-52 1台変更時の再読を『丸ごと O(N)』と『版で差分 O(変更数)』で比べる

| | |
| --- | --- |
| Experiment | EXP-52 / hub-cache-granularity |
| Starting SHA | `fd59624975cd` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) テナント丸ごと引き直すと、1回の poll で毎回 N 行読む → M 回で ≒ N×M 行。 2) 版(ver)で差分だけ引くと、変わった台数しか読まない → M 回で ≒ 変更総数。 3) 台数 N が増えるほど丸ごとの無駄が線形に増える。差分は N に依らず変更数で決まる。 4) 購読者 S 人でも hub なら poll は共有（1本）。hub 無しだと S 倍に膨らむ（本実験は poll 側を測る）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=fd59624975cd+dirty |
| Started / Ended | 2026-09-07T12:51:28Z / 2026-09-07T12:51:29Z |

## Results

### テナント丸ごと引き直し（粗い破棄）: 毎 poll N 行 — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| per_poll | 2000 |
| total_rows_read | 100000 |

| 測ったもの | 値 |
| --- | --- |
| read_KB_目安 | 19531.250 |

- N=2000 × poll=50 = 100000 行（変更が5台でも毎回2000行）

### 版で差分だけ引く（細かい破棄）: 変更数しか読まない — OK

| 数えたもの | 値 |
| --- | --- |
| total_changes | 250 |
| total_rows_read | 250 |

| 測ったもの | 値 |
| --- | --- |
| read_KB_目安 | 48.828 |
| reduction_x | 400.000 |

- 読んだ行=変更総数 250。丸ごと比 400.0x 少ない

## Verdict

1 hub に複数ロボットがいるとき、キャッシュ破棄は『テナント丸ごと再読 O(N)』でなく『版で差分 O(変更数)』にする。まばらな変更では差分が数百倍少なく読む（N=2000・5台/poll で実測）。破棄の合図はテナント単位の版 bump で粗くてよいが、再取得は ver>last の索引レンジで差分だけ。購読者が多くてもpoll は hub で共有（S 倍にしない・EXP-38）。全台が毎回変わるなら丸ごとと同じ＝まばらさが効く。

## 適用範囲

- MySQL 8.0 / 1テナント N=2000 台 / poll=50 回・変更 5 台/poll（まばら）/ payload ~200B
- 読んだ行数 = 破棄の粗さの実弾。丸ごと=N×poll、差分=変更総数（版索引レンジ）
- バイトは行幅 200B の目安（実際はヘッダ・索引で上下）

## 保証しない範囲・未検証

- 版(ver)はテナント内で単調増加する前提（書き込み時に採番・EXP-47 と同じ）
- 差分は『変更が来た行だけ』。全台が毎 poll 変わるワークロードなら丸ごとと同じになる（まばらさが効く）
- 購読者 S 人ぶんの膨張は hub で poll を共有すれば消える（EXP-38）。本実験は poll 1本ぶんの再読量
- payload が太い/off-page だと丸ごとの不利が更に増える（EXP-21）。差分は触る行が少なく有利

## 再利用できる成果物

- internal/hubcachelab: WholeSnapshot / DeltaSince（破棄粒度の測定）
- docs/cache.md / docs/subscription-design.md: hub のキャッシュ破棄

## 次の実験

- EXP-53 長寿命接続の途中失権（re-authorization）

