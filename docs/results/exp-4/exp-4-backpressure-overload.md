# EXP-4 入力が処理能力を超えたときのメモリ・遅延・公平性

| | |
| --- | --- |
| Experiment | EXP-4 / backpressure-overload |
| Starting SHA | `bdc8149784b5` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 無制限キューは、入力が処理能力を超えるとメモリと遅延が増え続ける。 2) 有界キューはメモリを抑えるが、代わりに『待たせる』か『捨てる』のどちらかを選ぶことになる。 3) キーごとに最新だけ残す方式は、同じ対象への連続更新が多いほど書き込みを減らし、    遅延も小さくなる。 4) テナントごとに枠を分けないと、1テナントの氾濫が他テナントの処理を奪う。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=bdc8149784b5+dirty |
| Started / Ended | 2026-09-03T22:43:24Z / 2026-09-03T22:43:48Z |

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
| accepted | 19940 |
| coalesced | 0 |
| committed | 1020 |
| dropped | 0 |
| leftover_at_stop | 18911 |
| max_queue | 19040 |
| produced | 19940 |
| txs | 102 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 14.868 |
| rss_max_mb | 23.195 |
| tx_per_sec | 50.992 |

遅延: n=1020 p50=1.116538s p95=2.07788s p99=2.162623s max=2.185121s

goroutine 最大 9 / 終了時 8 ・ヒープ最大 14.9 MB ・RSS 最大 23.2 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- ★キューが伸び続け、反映の遅れもそのぶん増える。落ちるまで気づきにくい

### bounded_block — OK

有界。いっぱいなら生産側を待たせる（入口へ背圧をかける）

| 数えたもの | 値 |
| --- | --- |
| accepted | 1920 |
| coalesced | 0 |
| committed | 1020 |
| dropped | 0 |
| leftover_at_stop | 891 |
| max_queue | 1024 |
| produced | 1920 |
| txs | 102 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 1.908 |
| heap_max_mb | 17.077 |
| rss_max_mb | 25.250 |
| tx_per_sec | 50.557 |

遅延: n=1020 p50=1.133568s p95=2.098741s p99=2.185063s max=2.207683s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 17.1 MB ・RSS 最大 25.2 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 生産側が待たされた合計 1.908s（これが背圧の量）

### bounded_drop — OK

有界。いっぱいなら捨てる

| 数えたもの | 値 |
| --- | --- |
| accepted | 1914 |
| coalesced | 0 |
| committed | 1020 |
| dropped | 18026 |
| leftover_at_stop | 885 |
| max_queue | 1024 |
| produced | 19940 |
| txs | 102 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 19.352 |
| rss_max_mb | 27.473 |
| tx_per_sec | 50.974 |

遅延: n=1020 p50=1.114525s p95=2.085963s p99=2.170937s max=2.193875s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 19.4 MB ・RSS 最大 27.5 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 捨てた 18026 件（数えているので、あとから説明できる）

### coalesce_latest — OK

キーごとに最新だけ残す（1件ずつ書く）

| 数えたもの | 値 |
| --- | --- |
| accepted | 19560 |
| coalesced | 19160 |
| committed | 92 |
| dropped | 0 |
| leftover_at_stop | 200 |
| max_queue | 200 |
| produced | 19560 |
| txs | 92 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.476 |
| rss_max_mb | 14.492 |
| tx_per_sec | 45.979 |

遅延: n=92 p50=1.076905s p95=2.009893s p99=2.072771s max=2.133153s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 3.5 MB ・RSS 最大 14.5 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 19160 件

### coalesce_batch — OK

キーごとに最新だけ残し、1トランザクションでまとめて書く

| 数えたもの | 値 |
| --- | --- |
| accepted | 19920 |
| coalesced | 17900 |
| committed | 2020 |
| dropped | 0 |
| leftover_at_stop | 20 |
| max_queue | 200 |
| produced | 19920 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.644 |
| rss_max_mb | 14.691 |
| tx_per_sec | 5.498 |

遅延: n=2020 p50=40.514ms p95=94.126ms p99=199.221ms max=223.475ms

goroutine 最大 8 / 終了時 7 ・ヒープ最大 3.6 MB ・RSS 最大 14.7 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 17900 件 / トランザクション 11 回

### まとめて書く / DB 遅延 0s — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19880 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.596 |
| tx_per_sec | 4.999 |

遅延: n=2000 p50=18.798ms p95=66.612ms p99=101.36ms max=177.75ms

- キュー最大 200 件 / 反映の遅れ p99 101ms

### まとめて書く / DB 遅延 10ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19580 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.690 |
| tx_per_sec | 4.999 |

遅延: n=2000 p50=29.399ms p95=75.63ms p99=108.134ms max=210.657ms

- キュー最大 200 件 / 反映の遅れ p99 108ms

### まとめて書く / DB 遅延 100ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 20000 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.729 |
| tx_per_sec | 4.999 |

遅延: n=2000 p50=120.541ms p95=166.772ms p99=203.546ms max=277.257ms

- キュー最大 200 件 / 反映の遅れ p99 204ms

### 1テナントが入力の90%を占める / bounded_drop — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 894 |
| committed_small | 116 |
| dropped | 18854 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.115 |

遅延: n=1010 p50=588.622ms p95=612.727ms p99=614.206ms max=614.214ms

- 少数派テナントの取り分 11.5%（入力比では 10%）

### 1テナントが入力の90%を占める / per_tenant_quota — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 1010 |
| committed_small | 1010 |
| dropped | 17964 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.500 |

遅延: n=2020 p50=309.336ms p95=320.158ms p99=324.663ms max=325.139ms

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

