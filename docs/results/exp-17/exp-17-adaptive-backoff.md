# EXP-17 適応的バックオフ: 空振りで間隔を伸ばし、仕事で詰める

| | |
| --- | --- |
| Experiment | EXP-17 / adaptive-backoff |
| Starting SHA | `65f8b884a5ab` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 固定 100ms ポーリングは、暇な間も一定間隔で空振りし続ける。 2) 適応的バックオフ（空振りで倍々、仕事で Min へ）は、暇な間の空振りを大きく減らす。 3) 仕事が来たときの遅延は、固定 100ms とほぼ同等に保てる（Min へ戻すので）。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=65f8b884a5ab+dirty |
| Started / Ended | 2026-09-06T23:55:47Z / 2026-09-06T23:55:59Z |

## Workload

- `arrival` = 10/s（疎）
- `commands` = 30
- `hold` = 6s

## Results

### 固定 100ms ポーリング — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 29 |
| polls | 59 |

| 測ったもの | 値 |
| --- | --- |
| p95_ms | 82.142 |

### 適応的バックオフ（Min 100ms → Max 2s） — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 4 |
| polls | 34 |

| 測ったもの | 値 |
| --- | --- |
| p95_ms | 95.025 |

- 空振り 29 → 4
- 仕事が来たら Min へ戻すので、dispatch 遅延は固定 100ms と同等に保てる

## Verdict

適応的バックオフは、暇な間の空振りの問い合わせを大きく減らす。仕事が来たら Min へ戻すので、dispatch 遅延は固定間隔とほぼ同等に保てる。低到着のテナントを多数抱えるとき、fan-out を畳む（EXP-14）と合わせて DB 負荷を下げられる。

## 適用範囲

- MySQL 8.0 / 単一テナント / 命令 30 を 3 秒に散らし、その後 3 秒暇にする
- 空振り = 0 件だったポーリング。dispatch 遅延 = scheduled_for から claim まで

## 保証しない範囲・未検証

- 暇の長さ・Min/Max・到着パターンで効果は動く。この数字はこのホストのもの
- 仕事が『バックオフ中に来た』場合、最大 Max だけ遅れる。低遅延が要るなら wake（EXP-12）と併用
- 複数テナントを畳む（EXP-14）場合のバックオフは、担当集合ごとに1つ回す想定（未測定）

## 再利用できる成果物

- internal/cadencelab: Adaptive バックオフ（空振りで倍々、仕事で Min へ）

## 次の実験

- なし

