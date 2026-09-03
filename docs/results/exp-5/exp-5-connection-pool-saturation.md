# EXP-5 同時実行数と MaxOpenConns を振って、スループットが伸びなくなる点を探す

| | |
| --- | --- |
| Experiment | EXP-5 / connection-pool-saturation |
| Starting SHA | `bdc8149784b5` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが線形に伸びる。 2) その点は MaxOpenConns 付近にあり、超えた分は database/sql の待ち行列（WaitCount）に現れる。 3) MaxOpenConns を増やしても、CPU やディスクが先に頭打ちになれば伸びない。 4) 遅い処理（DB 側で待つトランザクション）は、その時間ぶん接続を占有し、    同時実行数に関係なく MaxOpenConns / 処理時間 で上限が決まる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=bdc8149784b5+dirty |
| Started / Ended | 2026-09-03T22:43:48Z / 2026-09-03T22:44:13Z |

## Workload

- `duration_per_point` = 1.5s
- `table_rows` = 64

## Failure injection

- `none` = 故障注入なし。負荷と設定だけを振る

## Results

### 主キー読み / 並列 1 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 7501 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 5000.289 |
| wait_duration_ms | 0.000 |

遅延: n=7501 p50=192µs p95=300µs p99=353µs max=711µs

### 主キー読み / 並列 2 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 16892 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 11260.280 |
| wait_duration_ms | 0.000 |

遅延: n=16892 p50=170µs p95=264µs p99=326µs max=1.691ms

### 主キー読み / 並列 4 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 27820 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 18543.448 |
| wait_duration_ms | 0.000 |

遅延: n=27820 p50=199µs p95=369µs p99=514µs max=3.382ms

### 主キー読み / 並列 8 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 45362 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 30236.650 |
| wait_duration_ms | 0.000 |

遅延: n=45362 p50=225µs p95=535µs p99=855µs max=6.789ms

### 主キー読み / 並列 16 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 44103 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 44111 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 29398.227 |
| wait_duration_ms | 12073.000 |

遅延: n=44103 p50=468µs p95=1.16ms p99=1.671ms max=7.16ms

### 主キー読み / 並列 32 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 42692 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 42716 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 28458.637 |
| wait_duration_ms | 36056.000 |

遅延: n=42692 p50=875µs p95=2.824ms p99=4.181ms max=9.832ms

### 主キー読み / 並列 64 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 41778 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 41830 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 27848.382 |
| wait_duration_ms | 84010.000 |

遅延: n=41778 p50=1.678ms p95=6.349ms p99=9.615ms max=22.77ms

### MaxOpenConns を振る（並列 32 固定） — OK

接続を増やしても伸びなくなる点を探す

| 数えたもの | 値 |
| --- | --- |
| knee_max_open | 32 |

- MaxOpen  1:     5682 ops/s（前段からの伸び  +0.0%）p99=24.859ms  待ち  8555 回
- MaxOpen  2:    10469 ops/s（前段からの伸び +84.2%）p99=12.998ms  待ち 15734 回
- MaxOpen  4:    17969 ops/s（前段からの伸び +71.6%）p99=7.353ms   待ち 26990 回
- MaxOpen  8:    26196 ops/s（前段からの伸び +45.8%）p99=4.589ms   待ち 39321 回
- MaxOpen 16:    31667 ops/s（前段からの伸び +20.9%）p99=3.143ms   待ち 47524 回
- MaxOpen 32:    31513 ops/s（前段からの伸び  -0.5%）p99=3.291ms   待ち     0 回

### 同時実行数を振る（MaxOpen 8 固定） — OK

頭打ちの位置と、超えた分がどこに現れるか

| 数えたもの | 値 |
| --- | --- |
| peak_concurrency | 8 |

| 測ったもの | 値 |
| --- | --- |
| peak_ops_per_sec | 30236.650 |

- 並列   1:     5000 ops/s p50=192µs    p99=353µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   2:    11260 ops/s p50=170µs    p99=326µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   4:    18543 ops/s p50=199µs    p99=514µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   8:    30237 ops/s p50=225µs    p99=855µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列  16:    29398 ops/s p50=468µs    p99=1.671ms   待ち 44111 回 / 合計 12.074s・サーバ接続 最大 0
- 並列  32:    28459 ops/s p50=875µs    p99=4.181ms   待ち 42716 回 / 合計 36.057s・サーバ接続 最大 0
- 並列  64:    27848 ops/s p50=1.678ms  p99=9.615ms   待ち 41830 回 / 合計 1m24.011s・サーバ接続 最大 0

### MaxIdleConns の既定値（2）の影響 — OK

接続の張り直しがスループットに出るか

- MaxIdle  2:    29286 ops/s / 新規接続     0 本 / アイドル超過で閉じた    63 本 / p99 1.772ms
- MaxIdle 16:    32990 ops/s / 新規接続     0 本 / アイドル超過で閉じた     0 本 / p99 1.456ms

### DB 側で 100ms 待つトランザクション / MaxOpen 4 / 並列 32 — OK

遅い処理は、その時間ぶん接続を占有する

| 数えたもの | 値 |
| --- | --- |
| ops | 116 |
| server_threads_connected_max | 0 |
| wait_count | 144 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 38.658 |
| theoretical_max | 40.000 |
| wait_duration_ms | 84011.000 |

遅延: n=116 p50=515.049ms p95=1.742324s p99=2.459767s max=2.97174s

- 実測 38.7 ops/s（理論上限 MaxOpen/処理時間 = 40 ops/s）
- 待ち 144 回・合計 1m24.011s。並列を増やしても、この上限は動かない
- ★上限を決めるのは同時実行数ではなく『接続数 ÷ 1件あたりの占有時間』

## Verdict

この環境（4 CPU・同一ホスト）では、主キー読みの頂点は並列 8 付近で 30237 ops/s だった。それを超えると database/sql の待ち行列に積まれ、スループットは伸びずに遅延だけが伸びる。遅いトランザクションでは上限が『接続数 ÷ 占有時間』で決まり、並列を増やしても動かない。

## 適用範囲

- MySQL 8.0 / 同一ホスト / 4 CPU のコンテナ。DB とアプリが同じ機械にある
- 表は 64 行・主キー読みが中心。実データの分布や索引の効き方は含まない
- 1条件 1.5〜3 秒の測定。長時間の安定性は見ていない

## 保証しない範囲・未検証

- ネットワークを跨ぐ構成では、往復が支配的になり飽和点が変わる
- RDS Proxy などの接続集約を挟んだ場合は docs/rds-proxy.md（LIVE_ENV_REQUIRED）
- 同一ホストなので、アプリと MySQL が CPU を奪い合っている。専用機での値とは異なる
- 接続確立のコスト（TLS）は測っていない（TLS 無効の接続）

## 再利用できる成果物

- internal/poollab: 同時実行数・MaxOpen・MaxIdle・処理時間を振って、database/sql と MySQL の両方の数字を同時に取る測定器

## 次の実験

- EXP-7 データ偏り付きの実行計画

