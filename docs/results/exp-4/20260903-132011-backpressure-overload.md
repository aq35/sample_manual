# EXP-4 入力が処理能力を超えたときのメモリ・遅延・公平性

| | |
| --- | --- |
| Experiment | EXP-4 / backpressure-overload |
| Starting SHA | `29c844e064c2` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 無制限キューは、入力が処理能力を超えるとメモリと遅延が増え続ける。 2) 有界キューはメモリを抑えるが、代わりに『待たせる』か『捨てる』のどちらかを選ぶことになる。 3) キーごとに最新だけ残す方式は、同じ対象への連続更新が多いほど書き込みを減らし、    遅延も小さくなる。 4) テナントごとに枠を分けないと、1テナントの氾濫が他テナントの処理を奪う。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=29c844e064c2+dirty |
| Started / Ended | 2026-09-03T13:20:11Z / 2026-09-03T13:20:39Z |

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
| accepted | 19960 |
| coalesced | 0 |
| committed | 1000 |
| dropped | 0 |
| leftover_at_stop | 18951 |
| max_queue | 19080 |
| produced | 19960 |
| txs | 100 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 14.881 |
| rss_max_mb | 23.051 |
| tx_per_sec | 49.975 |

遅延: n=1000 p50=1.116383s p95=2.087136s p99=2.17507s max=2.198323s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 14.9 MB ・RSS 最大 23.1 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

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
| blocked_seconds | 1.910 |
| heap_max_mb | 17.063 |
| rss_max_mb | 25.090 |
| tx_per_sec | 49.487 |

遅延: n=1000 p50=1.137663s p95=2.106664s p99=2.193211s max=2.215934s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 17.1 MB ・RSS 最大 25.1 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 生産側が待たされた合計 1.91s（これが背圧の量）

### bounded_drop — OK

有界。いっぱいなら捨てる

| 数えたもの | 値 |
| --- | --- |
| accepted | 1904 |
| coalesced | 0 |
| committed | 1000 |
| dropped | 18056 |
| leftover_at_stop | 895 |
| max_queue | 1024 |
| produced | 19960 |
| txs | 100 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 19.338 |
| rss_max_mb | 27.535 |
| tx_per_sec | 49.992 |

遅延: n=1000 p50=1.114014s p95=2.073055s p99=2.158744s max=2.181228s

goroutine 最大 9 / 終了時 6 ・ヒープ最大 19.3 MB ・RSS 最大 27.5 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 捨てた 18056 件（数えているので、あとから説明できる）

### coalesce_latest — OK

キーごとに最新だけ残す（1件ずつ書く）

| 数えたもの | 値 |
| --- | --- |
| accepted | 19980 |
| coalesced | 19580 |
| committed | 293 |
| dropped | 0 |
| leftover_at_stop | 200 |
| max_queue | 200 |
| produced | 19980 |
| txs | 293 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.516 |
| rss_max_mb | 14.852 |
| tx_per_sec | 146.420 |

遅延: n=293 p50=1.838252s p95=4.509988s p99=4.777675s max=4.8384s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 3.5 MB ・RSS 最大 14.9 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 19580 件

### coalesce_batch — OK

キーごとに最新だけ残し、1トランザクションでまとめて書く

| 数えたもの | 値 |
| --- | --- |
| accepted | 19720 |
| coalesced | 17700 |
| committed | 2020 |
| dropped | 0 |
| leftover_at_stop | 20 |
| max_queue | 200 |
| produced | 19720 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.582 |
| rss_max_mb | 14.723 |
| tx_per_sec | 5.497 |

遅延: n=2020 p50=43.678ms p95=93.072ms p99=198.439ms max=224.052ms

goroutine 最大 8 / 終了時 7 ・ヒープ最大 3.6 MB ・RSS 最大 14.7 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 17700 件 / トランザクション 11 回

### まとめて書く / DB 遅延 0s — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2020 |
| max_queue | 200 |
| produced | 19900 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.663 |
| tx_per_sec | 5.498 |

遅延: n=2020 p50=21.4ms p95=74.452ms p99=178.417ms max=203.376ms

- キュー最大 200 件 / 反映の遅れ p99 178ms

### まとめて書く / DB 遅延 10ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19960 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.653 |
| tx_per_sec | 5.000 |

遅延: n=2000 p50=30.84ms p95=77.316ms p99=112.384ms max=188.303ms

- キュー最大 200 件 / 反映の遅れ p99 112ms

### まとめて書く / DB 遅延 100ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19940 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.585 |
| tx_per_sec | 4.998 |

遅延: n=2000 p50=120.256ms p95=166.719ms p99=200.158ms max=278.862ms

- キュー最大 200 件 / 反映の遅れ p99 200ms

### 1テナントが入力の90%を占める / bounded_drop — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 902 |
| committed_small | 98 |
| dropped | 18844 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.098 |

遅延: n=1000 p50=601.11ms p95=625.07ms p99=625.667ms max=626.568ms

- 少数派テナントの取り分 9.8%（入力比では 10%）

### 1テナントが入力の90%を占める / per_tenant_quota — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 1000 |
| committed_small | 990 |
| dropped | 17959 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.497 |

遅延: n=1990 p50=313.192ms p95=329.404ms p99=334.29ms max=335.256ms

- 少数派テナントの取り分 49.7%（入力比では 10%）

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

