# EXP-4 入力が処理能力を超えたときのメモリ・遅延・公平性

| | |
| --- | --- |
| Experiment | EXP-4 / backpressure-overload |
| Starting SHA | `07fc9a222df4` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 無制限キューは、入力が処理能力を超えるとメモリと遅延が増え続ける。 2) 有界キューはメモリを抑えるが、代わりに『待たせる』か『捨てる』のどちらかを選ぶことになる。 3) キーごとに最新だけ残す方式は、同じ対象への連続更新が多いほど書き込みを減らし、    遅延も小さくなる。 4) テナントごとに枠を分けないと、1テナントの氾濫が他テナントの処理を奪う。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=07fc9a222df4+dirty |
| Started / Ended | 2026-09-03T12:25:09Z / 2026-09-03T12:25:36Z |

## Workload

- `batch_size` = 10
- `capacity` = 1024
- `duration` = 2s
- `keys_per_tenant` = 200
- `payload_bytes` = 512
- `rate_events_per_sec` = 10000
- `workers` = 1

## Failure injection

- `db_delay_per_tx` = 20ms
- `tenant_skew` = 1テナントが入力の 90% を占める条件を別途実行

## Results

### unbounded_queue — **事故あり**

入ってきたぶんだけ溜める

| 数えたもの | 値 |
| --- | --- |
| accepted | 19920 |
| coalesced | 0 |
| committed | 990 |
| dropped | 0 |
| leftover_at_stop | 18921 |
| max_queue | 19050 |
| produced | 19920 |
| txs | 99 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 14.869 |
| rss_max_mb | 23.262 |
| tx_per_sec | 49.474 |

遅延: n=990 p50=1.107414s p95=2.098845s p99=2.166063s max=2.186285s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 14.9 MB ・RSS 最大 23.3 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- ★キューが伸び続け、反映の遅れもそのぶん増える。落ちるまで気づきにくい

### bounded_block — OK

有界。いっぱいなら生産側を待たせる（入口へ背圧をかける）

| 数えたもの | 値 |
| --- | --- |
| accepted | 1900 |
| coalesced | 0 |
| committed | 1000 |
| dropped | 0 |
| leftover_at_stop | 891 |
| max_queue | 1024 |
| produced | 1900 |
| txs | 100 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 1.919 |
| heap_max_mb | 17.053 |
| rss_max_mb | 25.266 |
| tx_per_sec | 49.291 |

遅延: n=1000 p50=1.146699s p95=2.115724s p99=2.203848s max=2.226614s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 17.1 MB ・RSS 最大 25.3 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 生産側が待たされた合計 1.919s（これが背圧の量）

### bounded_drop — OK

有界。いっぱいなら捨てる

| 数えたもの | 値 |
| --- | --- |
| accepted | 1904 |
| coalesced | 0 |
| committed | 1000 |
| dropped | 18096 |
| leftover_at_stop | 895 |
| max_queue | 1024 |
| produced | 20000 |
| txs | 100 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 19.330 |
| rss_max_mb | 28.008 |
| tx_per_sec | 49.986 |

遅延: n=1000 p50=1.117964s p95=2.086933s p99=2.174304s max=2.19681s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 19.3 MB ・RSS 最大 28.0 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 捨てた 18096 件（数えているので、あとから説明できる）

### coalesce_latest — OK

キーごとに最新だけ残す（1件ずつ書く）

| 数えたもの | 値 |
| --- | --- |
| accepted | 19500 |
| coalesced | 19100 |
| committed | 290 |
| dropped | 0 |
| leftover_at_stop | 200 |
| max_queue | 200 |
| produced | 19500 |
| txs | 290 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.483 |
| rss_max_mb | 15.191 |
| tx_per_sec | 144.964 |

遅延: n=290 p50=1.853909s p95=4.551597s p99=4.846405s max=4.87697s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 3.5 MB ・RSS 最大 15.2 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 19100 件

### coalesce_batch — OK

キーごとに最新だけ残し、1トランザクションでまとめて書く

| 数えたもの | 値 |
| --- | --- |
| accepted | 20000 |
| coalesced | 17980 |
| committed | 2020 |
| dropped | 0 |
| leftover_at_stop | 20 |
| max_queue | 200 |
| produced | 20000 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.695 |
| rss_max_mb | 15.215 |
| tx_per_sec | 5.497 |

遅延: n=2020 p50=41.59ms p95=92.514ms p99=200.404ms max=224.616ms

goroutine 最大 8 / 終了時 7 ・ヒープ最大 3.7 MB ・RSS 最大 15.2 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 17980 件 / トランザクション 11 回

### まとめて書く / DB 遅延 0s — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 20000 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.664 |
| tx_per_sec | 4.999 |

遅延: n=2000 p50=20.671ms p95=67.877ms p99=102.466ms max=178.971ms

- キュー最大 200 件 / 反映の遅れ p99 102ms

### まとめて書く / DB 遅延 10ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19620 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.692 |
| tx_per_sec | 4.999 |

遅延: n=2000 p50=30.762ms p95=76.905ms p99=109.794ms max=188.46ms

- キュー最大 200 件 / 反映の遅れ p99 110ms

### まとめて書く / DB 遅延 100ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 20000 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.625 |
| tx_per_sec | 4.998 |

遅延: n=2000 p50=121.121ms p95=167.824ms p99=202.763ms max=280.27ms

- キュー最大 200 件 / 反映の遅れ p99 203ms

### 1テナントが入力の90%を占める / bounded_drop — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 882 |
| committed_small | 108 |
| dropped | 18874 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.109 |

遅延: n=990 p50=600.013ms p95=623.605ms p99=625.215ms max=625.27ms

- 少数派テナントの取り分 10.9%（入力比では 10%）

### 1テナントが入力の90%を占める / per_tenant_quota — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 990 |
| committed_small | 990 |
| dropped | 18004 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.500 |

遅延: n=1980 p50=315.694ms p95=338.365ms p99=340.497ms max=341.544ms

- 少数派テナントの取り分 50.0%（入力比では 10%）

## Verdict

無制限キューは入力が処理能力を超えた瞬間からメモリと遅延が伸び続け、有界にすると『待たせる』『捨てる』のどちらかを選ぶことになる（選ばない選択肢は無い）。キーごとに最新だけ残す方式は、同じ対象への更新が多い場面で書き込みと遅延の両方を下げた。テナントごとに枠を分けると、氾濫しているテナントの隣で少数派の処理が生き残った。

## 適用範囲

- MySQL 8.0 / 同一ホスト / 1プロセス内のパイプライン（生成→キュー→バッチ→トランザクション）
- DB 遅延は SELECT SLEEP で DB 側に実際に待たせている（接続の占有まで含まれる）
- 入力レートは 10,000 件/秒。処理側は DB 遅延によって明確に追いつかない条件

## 保証しない範囲・未検証

- ヒープ最大値は方式の比較に使えなかった（10,000件/秒では割り当ての回転と GC の時機に支配される）。方式の差は『キューが抱えている件数×1件の大きさ』で見ている
- spill-to-disk（あふれをディスクへ退避）は未実装・未測定
- 複数プロセスでの公平性（同じ DB を複数 worker が奪い合う場合）は未測定
- 入力が止まったあとの回復時間は、2秒の走行では十分に観測できていない
- GC の挙動は Go の既定のまま（GOGC 未調整）

## 再利用できる成果物

- internal/loadlab: 6方式の受け口（無制限/有界待ち/有界捨て/まとめ/まとめ+バッチ/テナント枠）
- expkit.Sampler の custom で queue depth を時系列に残す型

## 次の実験

- EXP-5 接続プールの飽和点

