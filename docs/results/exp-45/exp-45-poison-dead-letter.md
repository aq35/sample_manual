# EXP-45 必ず失敗する命令を無限リトライせず、上限で dead に隔離する

| | |
| --- | --- |
| Experiment | EXP-45 / poison-dead-letter |
| Starting SHA | `bec717ea663f` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 上限なし: poison は pending のまま残り、周回のたびに再試行され続ける（キューが drain しない・試行が青天井）。良い命令は done になる（先頭で止めない前提）。 2) 上限→dead(DLQ): poison は maxAttempts 回で dead に隔離され、キューは drain（pending=0）。試行は maxAttempts で有界。良い命令は done。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=bec717ea663f+dirty |
| Started / Ended | 2026-09-07T11:53:59Z / 2026-09-07T11:53:59Z |

## Results

### 上限なし: poison が pending に残り再試行が青天井・キューが drain しない — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| dead | 0 |
| done | 9 |
| pending | 1 |
| poison_attempts | 8 |

- poison は pending のまま（1 件）試行 8 回（周回数ぶん・止まらない）。良い命令は done 9

### 上限→dead(DLQ): poison を maxAttempts で隔離・キューは drain・試行は有界 — OK

| 数えたもの | 値 |
| --- | --- |
| dead | 1 |
| done | 9 |
| max_attempts | 3 |
| pending | 0 |
| poison_attempts | 3 |

- poison は 3 回で dead(1)。pending 0・done 9（キュー drain）

## Verdict

必ず失敗する命令(poison)は無限リトライしない。試行上限(maxAttempts)を決め、超えたら dead に隔離（アラート）してキューを drain させる。良い命令は先頭で止めず処理を続けるので poison に依らず流れる。バックオフ(EXP-17)と併用して隔離までの負荷も抑える。dead は原因修正後に replay する運用にする。

## 適用範囲

- MySQL 8.0 / 10命令中 1つが poison(必ず失敗) / 無限は 8 周で代表・DLQ は maxAttempts=3
- 失敗しても後続は処理する（先頭で止めない）ので良い命令は poison に依らず流れる
- 隔離＝state を dead に落とす。実運用は dead をアラートし人が調べる

## 保証しない範囲・未検証

- 実際の無限リトライは永遠に drain しない（ここは 8 周で代表・試行が周回数に比例することを示す）
- 先頭で止める実装（strict 順序）なら poison が後続もブロックする＝さらに悪い。ここは skip 継続前提
- バックオフ（EXP-17）と併用: 隔離までの間も間隔を伸ばして DB/外部を守る
- dead の再投入（原因修正後の replay）は運用手順（本実験外）

## 再利用できる成果物

- internal/dlqlab: 試行上限つき処理と dead 隔離
- docs/dead-letter.md: poison / dead-letter の扱い

## 次の実験

- なし

