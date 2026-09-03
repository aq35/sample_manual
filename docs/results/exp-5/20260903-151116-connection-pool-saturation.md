# EXP-5 同時実行数と MaxOpenConns を振って、スループットが伸びなくなる点を探す

| | |
| --- | --- |
| Experiment | EXP-5 / connection-pool-saturation |
| Starting SHA | `83598d6b4d1e` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが線形に伸びる。 2) その点は MaxOpenConns 付近にあり、超えた分は database/sql の待ち行列（WaitCount）に現れる。 3) MaxOpenConns を増やしても、CPU やディスクが先に頭打ちになれば伸びない。 4) 遅い処理（DB 側で待つトランザクション）は、その時間ぶん接続を占有し、    同時実行数に関係なく MaxOpenConns / 処理時間 で上限が決まる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=83598d6b4d1e+dirty |
| Started / Ended | 2026-09-03T15:11:16Z / 2026-09-03T15:11:41Z |

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
| ops | 7156 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 4770.339 |
| wait_duration_ms | 0.000 |

遅延: n=7156 p50=196µs p95=313µs p99=382µs max=20.304ms

### 主キー読み / 並列 2 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 14672 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 9780.491 |
| wait_duration_ms | 0.000 |

遅延: n=14672 p50=194µs p95=313µs p99=399µs max=987µs

### 主キー読み / 並列 4 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 23766 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 15842.242 |
| wait_duration_ms | 0.000 |

遅延: n=23766 p50=231µs p95=436µs p99=602µs max=4.551ms

### 主キー読み / 並列 8 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 35119 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 23408.245 |
| wait_duration_ms | 0.000 |

遅延: n=35119 p50=296µs p95=670µs p99=1.046ms max=6.766ms

### 主キー読み / 並列 16 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 33873 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 33880 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 22578.823 |
| wait_duration_ms | 12060.000 |

遅延: n=33873 p50=612µs p95=1.474ms p99=2.101ms max=5.506ms

### 主キー読み / 並列 32 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 34214 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 34238 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 22803.768 |
| wait_duration_ms | 36050.000 |

遅延: n=34214 p50=1.084ms p95=3.542ms p99=5.283ms max=16.046ms

### 主キー読み / 並列 64 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 37292 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 37348 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 24857.900 |
| wait_duration_ms | 84041.000 |

遅延: n=37292 p50=1.851ms p95=7.157ms p99=11.016ms max=39.45ms

### MaxOpenConns を振る（並列 32 固定） — OK

接続を増やしても伸びなくなる点を探す

| 数えたもの | 値 |
| --- | --- |
| knee_max_open | 32 |

- MaxOpen  1:     4728 ops/s（前段からの伸び  +0.0%）p99=31.041ms  待ち  7124 回
- MaxOpen  2:     8983 ops/s（前段からの伸び +90.0%）p99=15.209ms  待ち 13507 回
- MaxOpen  4:    16896 ops/s（前段からの伸び +88.1%）p99=7.892ms   待ち 25374 回
- MaxOpen  8:    22460 ops/s（前段からの伸び +32.9%）p99=5.454ms   待ち 33719 回
- MaxOpen 16:    29336 ops/s（前段からの伸び +30.6%）p99=3.775ms   待ち 44038 回
- MaxOpen 32:    29231 ops/s（前段からの伸び  -0.4%）p99=3.657ms   待ち     0 回

### 同時実行数を振る（MaxOpen 8 固定） — OK

頭打ちの位置と、超えた分がどこに現れるか

| 数えたもの | 値 |
| --- | --- |
| peak_concurrency | 64 |

| 測ったもの | 値 |
| --- | --- |
| peak_ops_per_sec | 24857.900 |

- 並列   1:     4770 ops/s p50=196µs    p99=382µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   2:     9780 ops/s p50=194µs    p99=399µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   4:    15842 ops/s p50=231µs    p99=602µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   8:    23408 ops/s p50=296µs    p99=1.046ms   待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列  16:    22579 ops/s p50=612µs    p99=2.101ms   待ち 33880 回 / 合計 12.06s・サーバ接続 最大 0
- 並列  32:    22804 ops/s p50=1.084ms  p99=5.283ms   待ち 34238 回 / 合計 36.05s・サーバ接続 最大 0
- 並列  64:    24858 ops/s p50=1.851ms  p99=11.016ms  待ち 37348 回 / 合計 1m24.042s・サーバ接続 最大 0

### MaxIdleConns の既定値（2）の影響 — OK

接続の張り直しがスループットに出るか

- MaxIdle  2:    31778 ops/s / 新規接続     0 本 / アイドル超過で閉じた    88 本 / p99 2.078ms
- MaxIdle 16:    30909 ops/s / 新規接続     0 本 / アイドル超過で閉じた     0 本 / p99 1.803ms

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
| wait_duration_ms | 84013.000 |

遅延: n=116 p50=514.529ms p95=1.441109s p99=2.468231s max=2.569798s

- 実測 38.7 ops/s（理論上限 MaxOpen/処理時間 = 40 ops/s）
- 待ち 144 回・合計 1m24.013s。並列を増やしても、この上限は動かない
- ★上限を決めるのは同時実行数ではなく『接続数 ÷ 1件あたりの占有時間』

## Verdict

この環境（4 CPU・同一ホスト）では、主キー読みの頂点は並列 64 付近で 24858 ops/s だった。それを超えると database/sql の待ち行列に積まれ、スループットは伸びずに遅延だけが伸びる。遅いトランザクションでは上限が『接続数 ÷ 占有時間』で決まり、並列を増やしても動かない。

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

