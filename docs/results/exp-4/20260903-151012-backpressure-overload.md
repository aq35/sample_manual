# EXP-4 入力が処理能力を超えたときのメモリ・遅延・公平性

| | |
| --- | --- |
| Experiment | EXP-4 / backpressure-overload |
| Starting SHA | `83598d6b4d1e` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 無制限キューは、入力が処理能力を超えるとメモリと遅延が増え続ける。 2) 有界キューはメモリを抑えるが、代わりに『待たせる』か『捨てる』のどちらかを選ぶことになる。 3) キーごとに最新だけ残す方式は、同じ対象への連続更新が多いほど書き込みを減らし、    遅延も小さくなる。 4) テナントごとに枠を分けないと、1テナントの氾濫が他テナントの処理を奪う。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=83598d6b4d1e+dirty |
| Started / Ended | 2026-09-03T15:10:12Z / 2026-09-03T15:10:35Z |

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
| accepted | 19760 |
| coalesced | 0 |
| committed | 1000 |
| dropped | 0 |
| leftover_at_stop | 18751 |
| max_queue | 18880 |
| produced | 19760 |
| txs | 100 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 14.771 |
| rss_max_mb | 23.301 |
| tx_per_sec | 49.996 |

遅延: n=1000 p50=1.120555s p95=2.074443s p99=2.162485s max=2.185049s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 14.8 MB ・RSS 最大 23.3 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

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
| blocked_seconds | 1.912 |
| heap_max_mb | 16.978 |
| rss_max_mb | 25.277 |
| tx_per_sec | 50.446 |

遅延: n=1020 p50=1.133909s p95=2.10361s p99=2.189081s max=2.212206s

goroutine 最大 9 / 終了時 8 ・ヒープ最大 17.0 MB ・RSS 最大 25.3 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 生産側が待たされた合計 1.912s（これが背圧の量）

### bounded_drop — OK

有界。いっぱいなら捨てる

| 数えたもの | 値 |
| --- | --- |
| accepted | 1914 |
| coalesced | 0 |
| committed | 1010 |
| dropped | 18086 |
| leftover_at_stop | 895 |
| max_queue | 1024 |
| produced | 20000 |
| txs | 101 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 19.535 |
| rss_max_mb | 28.160 |
| tx_per_sec | 50.488 |

遅延: n=1010 p50=1.101316s p95=2.075202s p99=2.160764s max=2.181035s

goroutine 最大 9 / 終了時 7 ・ヒープ最大 19.5 MB ・RSS 最大 28.2 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- 捨てた 18086 件（数えているので、あとから説明できる）

### coalesce_latest — OK

キーごとに最新だけ残す（1件ずつ書く）

| 数えたもの | 値 |
| --- | --- |
| accepted | 19700 |
| coalesced | 19300 |
| committed | 92 |
| dropped | 0 |
| leftover_at_stop | 200 |
| max_queue | 200 |
| produced | 19700 |
| txs | 92 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.502 |
| rss_max_mb | 14.754 |
| tx_per_sec | 45.995 |

遅延: n=92 p50=1.108477s p95=1.998159s p99=2.058673s max=2.118383s

goroutine 最大 9 / 終了時 6 ・ヒープ最大 3.5 MB ・RSS 最大 14.8 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 19300 件

### coalesce_batch — OK

キーごとに最新だけ残し、1トランザクションでまとめて書く

| 数えたもの | 値 |
| --- | --- |
| accepted | 19580 |
| coalesced | 17561 |
| committed | 2019 |
| dropped | 0 |
| leftover_at_stop | 19 |
| max_queue | 200 |
| produced | 19580 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| blocked_seconds | 0.000 |
| heap_max_mb | 3.729 |
| rss_max_mb | 14.988 |
| tx_per_sec | 5.497 |

遅延: n=2019 p50=41.157ms p95=91.251ms p99=180.342ms max=222.621ms

goroutine 最大 9 / 終了時 7 ・ヒープ最大 3.7 MB ・RSS 最大 15.0 MB ・DB 接続 最大 1（待ち 0 回 / 0s）

- まとめた 17561 件 / トランザクション 11 回

### まとめて書く / DB 遅延 0s — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 19540 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.667 |
| tx_per_sec | 4.998 |

遅延: n=2000 p50=21.115ms p95=72.674ms p99=107.237ms max=179.414ms

- キュー最大 200 件 / 反映の遅れ p99 107ms

### まとめて書く / DB 遅延 10ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2020 |
| max_queue | 200 |
| produced | 20000 |
| txs | 11 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.634 |
| tx_per_sec | 5.499 |

遅延: n=2020 p50=31.146ms p95=80.794ms p99=190.194ms max=215.088ms

- キュー最大 200 件 / 反映の遅れ p99 190ms

### まとめて書く / DB 遅延 100ms — OK

| 数えたもの | 値 |
| --- | --- |
| committed | 2000 |
| max_queue | 200 |
| produced | 20000 |
| txs | 10 |

| 測ったもの | 値 |
| --- | --- |
| heap_max_mb | 3.595 |
| tx_per_sec | 4.997 |

遅延: n=2000 p50=122.113ms p95=169.296ms p99=204.08ms max=279.627ms

- キュー最大 200 件 / 反映の遅れ p99 204ms

### 1テナントが入力の90%を占める / bounded_drop — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 898 |
| committed_small | 112 |
| dropped | 18864 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.111 |

遅延: n=1010 p50=591.763ms p95=614.773ms p99=616.788ms max=617.625ms

- 少数派テナントの取り分 11.1%（入力比では 10%）

### 1テナントが入力の90%を占める / per_tenant_quota — OK

| 数えたもの | 値 |
| --- | --- |
| committed_big | 1010 |
| committed_small | 1010 |
| dropped | 17971 |
| max_queue | 256 |

| 測ったもの | 値 |
| --- | --- |
| small_tenant_share | 0.500 |

遅延: n=2020 p50=310.455ms p95=318.631ms p99=319.579ms max=320.01ms

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

