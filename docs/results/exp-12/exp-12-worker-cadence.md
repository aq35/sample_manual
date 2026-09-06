# EXP-12 ワーカーのポーリング頻度: dispatch 遅延と DB 負荷のトレードオフ

| | |
| --- | --- |
| Experiment | EXP-12 / worker-cadence |
| Starting SHA | `dead7647bbd8` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 十分な batch で毎回 due を捌けるなら、dispatch 遅延 p95 はほぼポーリング間隔になる。 2) 間隔を短くすると遅延は下がるが、問い合わせ回数は増える。 3) 到着レートが低いのに速く引くと、空振りの問い合わせ回数が増える（無駄）。遅延は間隔で決まる。 4) 疎な到着では、signal 型の wake が空振りを無くし、ポーリングより低遅延で捌ける。 5) 別の制約: batch/interval が到着レート未満だと、間隔と無関係に backlog が溜まり遅延が発散する。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=dead7647bbd8+dirty |
| Started / Ended | 2026-09-06T23:11:43Z / 2026-09-06T23:12:30Z |

## Results

### 高到着 200/s / interval=100ms — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 600 |
| empty_polls | 0 |
| polls | 9 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 335.664 |
| latency_p95_ms | 417.594 |
| latency_p99_ms | 449.110 |
| queries_per_sec | 2.871 |

遅延: n=600 p50=335.665ms p95=417.594ms p99=449.11ms max=457.778ms

### 高到着 200/s / interval=250ms — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 600 |
| empty_polls | 0 |
| polls | 7 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 407.479 |
| latency_p95_ms | 549.108 |
| latency_p99_ms | 583.764 |
| queries_per_sec | 2.158 |

遅延: n=600 p50=407.479ms p95=549.108ms p99=583.764ms max=601.131ms

### 高到着 200/s / interval=500ms — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 600 |
| empty_polls | 0 |
| polls | 6 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 502.642 |
| latency_p95_ms | 767.134 |
| latency_p99_ms | 806.105 |
| queries_per_sec | 1.618 |

遅延: n=600 p50=502.643ms p95=767.134ms p99=806.106ms max=819.978ms

### 高到着 200/s / interval=1s — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 600 |
| empty_polls | 0 |
| polls | 4 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 1000.281 |
| latency_p95_ms | 1661.278 |
| latency_p99_ms | 1719.682 |
| queries_per_sec | 0.891 |

遅延: n=600 p50=1.000281s p95=1.661278s p99=1.719683s max=1.737043s

### 低到着 10/s / interval=100ms（空振りの無駄） — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 20 |
| polls | 49 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.408 |
| latency_p50_ms | 50.879 |
| latency_p95_ms | 104.127 |
| latency_p99_ms | 109.395 |
| queries_per_sec | 9.638 |

遅延: n=30 p50=50.879ms p95=104.127ms p99=109.395ms max=109.395ms

### 低到着 10/s / interval=250ms（空振りの無駄） — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 7 |
| polls | 20 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.350 |
| latency_p50_ms | 141.574 |
| latency_p95_ms | 242.331 |
| latency_p99_ms | 253.570 |
| queries_per_sec | 3.911 |

遅延: n=30 p50=141.574ms p95=242.332ms p99=253.571ms max=253.571ms

### 低到着 10/s / interval=500ms（空振りの無駄） — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 3 |
| polls | 10 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.300 |
| latency_p50_ms | 270.809 |
| latency_p95_ms | 480.419 |
| latency_p99_ms | 492.617 |
| queries_per_sec | 1.964 |

遅延: n=30 p50=270.81ms p95=480.42ms p99=492.618ms max=492.618ms

### 低到着 10/s / interval=1s（空振りの無駄） — OK

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 1 |
| polls | 5 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.200 |
| latency_p50_ms | 528.893 |
| latency_p95_ms | 945.630 |
| latency_p99_ms | 968.198 |
| queries_per_sec | 0.986 |

遅延: n=30 p50=528.894ms p95=945.63ms p99=968.198ms max=968.198ms

### wake（低到着10/s・signal 型。due 到来で起こす） — OK

ポーリングと違い空振りしない。疎な到着で低遅延・空振りゼロを狙う

| 数えたもの | 値 |
| --- | --- |
| dispatched | 30 |
| empty_polls | 0 |
| polls | 30 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 4.486 |
| latency_p95_ms | 9.235 |
| latency_p99_ms | 11.808 |
| queries_per_sec | 10.363 |

遅延: n=30 p50=4.487ms p95=9.235ms p99=11.809ms max=11.809ms

### ★制約: batch 50 / interval 500ms（処理能力 100/s < 到着 200/s） — **事故あり**

batch/interval が到着レート未満だと、間隔と無関係に backlog が溜まる

| 数えたもの | 値 |
| --- | --- |
| dispatched | 600 |
| polls | 13 |

| 測ったもの | 値 |
| --- | --- |
| empty_ratio | 0.000 |
| latency_p50_ms | 2259.066 |
| latency_p95_ms | 4705.792 |
| latency_p99_ms | 4758.204 |
| queries_per_sec | 1.706 |

遅延: n=600 p50=2.259067s p95=4.705793s p99=4.758204s max=4.767621s

- 処理能力 = batch/interval = 100/s。到着 200/s に追いつかず p95=4.705792962s まで発散
- → ポーリング間隔の前に、batch/interval ≥ 到着レート を満たすこと

## Verdict

ポーリング間隔は「許容する dispatch 遅延」で決める（batch を十分にすれば p95 ≈ 間隔）。ただし先に batch/interval ≥ 到着レート を満たすこと（さもないと間隔と無関係に発散）。到着が低いなら速く引くのは空振りの無駄。jitter で山をずらし、wake で低遅延と低負荷を両立する。

## 適用範囲

- MySQL 8.0 / 単一テナント・単一ワーカー（lease 前提）/ 命令を 3 秒に散らす
- dispatch 遅延 = scheduled_for から claim までの時間。ロボット側の実行時間は含まない
- claim は state 遷移（pending→dispatched）。複数ワーカーは対象外（1テナント1ワーカー）

## 保証しない範囲・未検証

- 最適間隔は到着レート・batch・DB の忙しさで動く。この数字はこのホストのもの
- 複数テナント同時のポーリングは未測定（jitter で山をずらす実装だけ）
- wake は in-process の producer を仮定。別プロセス跨ぎの push は別（MySQL に LISTEN/NOTIFY は無い）
- ロボット側の実行遅延・失敗・OUTCOME_UNKNOWN は cmd_result で観測する設計だが本実験の対象外

## 再利用できる成果物

- internal/cadencelab: スケジュール/命令/実績のスキーマ（schema.sql）とポーリングワーカー
- docs/scheduling.md: 設計・テナントワーカーの制約・最適間隔の決め方

## 次の実験

- なし（EXP-1..12）

