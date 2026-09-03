# EXP-5 同時実行数と MaxOpenConns を振って、スループットが伸びなくなる点を探す

| | |
| --- | --- |
| Experiment | EXP-5 / connection-pool-saturation |
| Starting SHA | `29c844e064c2` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが線形に伸びる。 2) その点は MaxOpenConns 付近にあり、超えた分は database/sql の待ち行列（WaitCount）に現れる。 3) MaxOpenConns を増やしても、CPU やディスクが先に頭打ちになれば伸びない。 4) 遅い処理（DB 側で待つトランザクション）は、その時間ぶん接続を占有し、    同時実行数に関係なく MaxOpenConns / 処理時間 で上限が決まる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=29c844e064c2+dirty |
| Started / Ended | 2026-09-03T13:21:19Z / 2026-09-03T13:21:45Z |

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
| ops | 7228 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 4818.202 |
| wait_duration_ms | 0.000 |

遅延: n=7228 p50=194µs p95=327µs p99=400µs max=3.531ms

### 主キー読み / 並列 2 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 14386 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 9589.953 |
| wait_duration_ms | 0.000 |

遅延: n=14386 p50=199µs p95=318µs p99=396µs max=2.788ms

### 主キー読み / 並列 4 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 24622 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 16412.070 |
| wait_duration_ms | 0.000 |

遅延: n=24622 p50=224µs p95=427µs p99=594µs max=3.623ms

### 主キー読み / 並列 8 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 34904 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 23264.463 |
| wait_duration_ms | 0.000 |

遅延: n=34904 p50=299µs p95=691µs p99=1.034ms max=5.344ms

### 主キー読み / 並列 16 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 32467 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 32474 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 21637.329 |
| wait_duration_ms | 12059.000 |

遅延: n=32467 p50=637µs p95=1.568ms p99=2.223ms max=6.677ms

### 主キー読み / 並列 32 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 28154 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 28178 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 18766.522 |
| wait_duration_ms | 36045.000 |

遅延: n=28154 p50=1.299ms p95=4.364ms p99=6.613ms max=19.89ms

### 主キー読み / 並列 64 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 32235 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 32291 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 21488.691 |
| wait_duration_ms | 84047.000 |

遅延: n=32235 p50=2.13ms p95=8.41ms p99=13.15ms max=34.043ms

### MaxOpenConns を振る（並列 32 固定） — OK

接続を増やしても伸びなくなる点を探す

| 数えたもの | 値 |
| --- | --- |
| knee_max_open | 32 |

- MaxOpen  1:     4135 ops/s（前段からの伸び  +0.0%）p99=34.38ms   待ち  6235 回
- MaxOpen  2:     8088 ops/s（前段からの伸び +95.6%）p99=17.443ms  待ち 12163 回
- MaxOpen  4:    15117 ops/s（前段からの伸び +86.9%）p99=8.876ms   待ち 22708 回
- MaxOpen  8:    21392 ops/s（前段からの伸び +41.5%）p99=5.814ms   待ち 32116 回
- MaxOpen 16:    25726 ops/s（前段からの伸び +20.3%）p99=4.008ms   待ち 38616 回
- MaxOpen 32:    26262 ops/s（前段からの伸び  +2.1%）p99=4.449ms   待ち     0 回

### 同時実行数を振る（MaxOpen 8 固定） — OK

頭打ちの位置と、超えた分がどこに現れるか

| 数えたもの | 値 |
| --- | --- |
| peak_concurrency | 8 |

| 測ったもの | 値 |
| --- | --- |
| peak_ops_per_sec | 23264.463 |

- 並列   1:     4818 ops/s p50=194µs    p99=400µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   2:     9590 ops/s p50=199µs    p99=396µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   4:    16412 ops/s p50=224µs    p99=594µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   8:    23264 ops/s p50=299µs    p99=1.034ms   待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列  16:    21637 ops/s p50=637µs    p99=2.223ms   待ち 32474 回 / 合計 12.06s・サーバ接続 最大 0
- 並列  32:    18767 ops/s p50=1.299ms  p99=6.613ms   待ち 28178 回 / 合計 36.045s・サーバ接続 最大 0
- 並列  64:    21489 ops/s p50=2.13ms   p99=13.15ms   待ち 32291 回 / 合計 1m24.048s・サーバ接続 最大 0

### MaxIdleConns の既定値（2）の影響 — OK

接続の張り直しがスループットに出るか

- MaxIdle  2:    26759 ops/s / 新規接続     0 本 / アイドル超過で閉じた    35 本 / p99 2.056ms
- MaxIdle 16:    28968 ops/s / 新規接続     0 本 / アイドル超過で閉じた     0 本 / p99 1.794ms

### DB 側で 100ms 待つトランザクション / MaxOpen 4 / 並列 32 — OK

遅い処理は、その時間ぶん接続を占有する

| 数えたもの | 値 |
| --- | --- |
| ops | 116 |
| server_threads_connected_max | 0 |
| wait_count | 144 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 38.652 |
| theoretical_max | 40.000 |
| wait_duration_ms | 84026.000 |

遅延: n=116 p50=513.938ms p95=1.439098s p99=2.056769s max=2.158016s

- 実測 38.7 ops/s（理論上限 MaxOpen/処理時間 = 40 ops/s）
- 待ち 144 回・合計 1m24.027s。並列を増やしても、この上限は動かない
- ★上限を決めるのは同時実行数ではなく『接続数 ÷ 1件あたりの占有時間』

## Verdict

この環境（4 CPU・同一ホスト）では、主キー読みの頂点は並列 8 付近で 23264 ops/s だった。それを超えると database/sql の待ち行列に積まれ、スループットは伸びずに遅延だけが伸びる。遅いトランザクションでは上限が『接続数 ÷ 占有時間』で決まり、並列を増やしても動かない。

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

