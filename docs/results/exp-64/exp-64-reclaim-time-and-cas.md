# EXP-64 in_progress→pending の回収は時間指定(heartbeat)とCASで書く（①時間なしは二重実行）

| | |
| --- | --- |
| Experiment | EXP-64 / reclaim-time-and-cas |
| Starting SHA | `d5beaeb87c79` |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 回収を『status=in_progress を全部戻す』(①時間なし)で書くと、heartbeat が新しい＝生きている担当の行まで 奪い、二重実行になる。heartbeat が lease より古い行だけを CAS(WHERE に status と heartbeat)で戻すと、 生きている担当は奪わず(奪取0)、落ちた担当ぶんだけを affected_rows として回収できる。 また回収候補の抽出は、索引末尾を heartbeat_at にすると completed の山を舐めずに済む。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=d5beaeb87c79 |
| Started / Ended | 2026-09-10T22:49:33Z / 2026-09-10T22:49:43Z |

## Workload

- `alive` = 500
- `completed_noise` = 100000
- `lease_sec` = 30
- `stale` = 500

## Results

### 回収①: 時間を見ない（status だけで全部戻す） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| reclaimed | 1000.000 |
| stolen_alive | 500.000 |

- 生きている担当から stolen_alive 件を奪う＝そのぶん二重実行になる

### 回収②: heartbeat が古い行だけを CAS で戻す — OK

| 測ったもの | 値 |
| --- | --- |
| reclaimed | 500.000 |
| stolen_alive | 0.000 |

- affected_rows=500 が『実際に奪えた件数』。生存担当は奪取0

### 回収候補の抽出: 索引 (tenant,status,heartbeat_at) あり — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 0.411 |

### 回収候補の抽出: 索引なし（completed の山を舐める） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 41.723 |

- 索引あり 411.46µs → なし 41.723436ms

## Verdict

回収(in_progress→pending)は『時間指定(heartbeat が lease より古い)』＋『CAS(affected_rows で奪取確認)』で書く。status だけで全部戻す①は、生きている担当を奪って二重実行になる。基準時刻は必ず DB の NOW(3)。回収候補の抽出索引は末尾を heartbeat_at にする(id ではない)。

## 適用範囲

- MySQL 8.0 / in_progress 1000(生存500・落ち500) + completed 10万 / lease 30s・stale 120s
- 回収①は status=in_progress を全 UPDATE。②は status=in_progress AND heartbeat_at < NOW(3)-INTERVAL 30 SECOND
- 基準時刻はすべて DB の NOW(3)。生存/落ちの区別は heartbeat_at の新旧のみ

## 保証しない範囲・未検証

- 二重実行の『実害』はここでは奪取件数で代理測定（実際の重複処理は worker 側の話）
- stale の絶対レイテンシは completed 量とバッファプール状態に依存。桁の関係だけが要点
- lease 値そのものの決め方は業務要件（何秒気づかなくて許されるか）で技術では決まらない（調査 §2.5）

## 再利用できる成果物

- internal/statuslab/exp64.go: SeedJobs / ReclaimAll(①) / ReclaimStale(②) / FindStale
- docs/worker-state-time.md: 欲しい状態は時間指定が要るか要らないか

## 次の実験

- なし

