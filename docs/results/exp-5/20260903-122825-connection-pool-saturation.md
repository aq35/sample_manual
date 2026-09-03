# EXP-5 同時実行数と MaxOpenConns を振って、スループットが伸びなくなる点を探す

| | |
| --- | --- |
| Experiment | EXP-5 / connection-pool-saturation |
| Starting SHA | `744c66634cc4` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが線形に伸びる。 2) その点は MaxOpenConns 付近にあり、超えた分は database/sql の待ち行列（WaitCount）に現れる。 3) MaxOpenConns を増やしても、CPU やディスクが先に頭打ちになれば伸びない。 4) 遅い処理（DB 側で待つトランザクション）は、その時間ぶん接続を占有し、    同時実行数に関係なく MaxOpenConns / 処理時間 で上限が決まる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=744c66634cc4+dirty |
| Started / Ended | 2026-09-03T12:28:25Z / 2026-09-03T12:28:50Z |

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
| ops | 6475 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 4316.097 |
| wait_duration_ms | 0.000 |

遅延: n=6475 p50=218µs p95=369µs p99=454µs max=1.66ms

### 主キー読み / 並列 2 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 14486 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 9657.244 |
| wait_duration_ms | 0.000 |

遅延: n=14486 p50=195µs p95=330µs p99=428µs max=3.234ms

### 主キー読み / 並列 4 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 27179 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 18117.637 |
| wait_duration_ms | 0.000 |

遅延: n=27179 p50=204µs p95=371µs p99=507µs max=4.003ms

### 主キー読み / 並列 8 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 35885 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 0 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 23921.402 |
| wait_duration_ms | 0.000 |

遅延: n=35885 p50=294µs p95=661µs p99=997µs max=4.619ms

### 主キー読み / 並列 16 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 32221 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 32228 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 21472.265 |
| wait_duration_ms | 12054.000 |

遅延: n=32221 p50=638µs p95=1.59ms p99=2.33ms max=8.673ms

### 主キー読み / 並列 32 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 35747 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 35770 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 23828.564 |
| wait_duration_ms | 36048.000 |

遅延: n=35747 p50=1.026ms p95=3.434ms p99=5.158ms max=12.268ms

### 主キー読み / 並列 64 / MaxOpen 8 — OK

| 数えたもの | 値 |
| --- | --- |
| new_connections | 0 |
| ops | 34602 |
| server_threads_connected_max | 0 |
| server_threads_running_max | 0 |
| wait_count | 34657 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 23062.897 |
| wait_duration_ms | 84062.000 |

遅延: n=34602 p50=2.003ms p95=7.684ms p99=11.816ms max=34.738ms

### MaxOpenConns を振る（並列 32 固定） — OK

接続を増やしても伸びなくなる点を探す

| 数えたもの | 値 |
| --- | --- |
| knee_max_open | 32 |

- MaxOpen  1:     4080 ops/s（前段からの伸び  +0.0%）p99=33.658ms  待ち  6152 回
- MaxOpen  2:     8265 ops/s（前段からの伸び +102.6%）p99=16.935ms  待ち 12428 回
- MaxOpen  4:    15371 ops/s（前段からの伸び +86.0%）p99=8.728ms   待ち 23088 回
- MaxOpen  8:    24300 ops/s（前段からの伸び +58.1%）p99=4.941ms   待ち 36487 回
- MaxOpen 16:    27404 ops/s（前段からの伸び +12.8%）p99=3.783ms   待ち 41144 回
- MaxOpen 32:    27676 ops/s（前段からの伸び  +1.0%）p99=4.149ms   待ち     0 回

### 同時実行数を振る（MaxOpen 8 固定） — OK

頭打ちの位置と、超えた分がどこに現れるか

| 数えたもの | 値 |
| --- | --- |
| peak_concurrency | 8 |

| 測ったもの | 値 |
| --- | --- |
| peak_ops_per_sec | 23921.402 |

- 並列   1:     4316 ops/s p50=218µs    p99=454µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   2:     9657 ops/s p50=195µs    p99=428µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   4:    18118 ops/s p50=204µs    p99=507µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列   8:    23921 ops/s p50=294µs    p99=997µs     待ち     0 回 / 合計 0s・サーバ接続 最大 0
- 並列  16:    21472 ops/s p50=638µs    p99=2.33ms    待ち 32228 回 / 合計 12.054s・サーバ接続 最大 0
- 並列  32:    23829 ops/s p50=1.026ms  p99=5.158ms   待ち 35770 回 / 合計 36.048s・サーバ接続 最大 0
- 並列  64:    23063 ops/s p50=2.003ms  p99=11.816ms  待ち 34657 回 / 合計 1m24.063s・サーバ接続 最大 0

### MaxIdleConns の既定値（2）の影響 — OK

接続の張り直しがスループットに出るか

- MaxIdle  2:    27289 ops/s / 新規接続     0 本 / アイドル超過で閉じた    84 本 / p99 2.14ms
- MaxIdle 16:    27803 ops/s / 新規接続     0 本 / アイドル超過で閉じた     0 本 / p99 2.33ms

### DB 側で 100ms 待つトランザクション / MaxOpen 4 / 並列 32 — OK

遅い処理は、その時間ぶん接続を占有する

| 数えたもの | 値 |
| --- | --- |
| ops | 116 |
| server_threads_connected_max | 0 |
| wait_count | 144 |

| 測ったもの | 値 |
| --- | --- |
| ops_per_sec | 38.650 |
| theoretical_max | 40.000 |
| wait_duration_ms | 84025.000 |

遅延: n=116 p50=516.628ms p95=1.757416s p99=2.481412s max=2.585197s

- 実測 38.7 ops/s（理論上限 MaxOpen/処理時間 = 40 ops/s）
- 待ち 144 回・合計 1m24.026s。並列を増やしても、この上限は動かない
- ★上限を決めるのは同時実行数ではなく『接続数 ÷ 1件あたりの占有時間』

## Verdict

この環境（4 CPU・同一ホスト）では、主キー読みの頂点は並列 8 付近で 23921 ops/s だった。それを超えると database/sql の待ち行列に積まれ、スループットは伸びずに遅延だけが伸びる。遅いトランザクションでは上限が『接続数 ÷ 占有時間』で決まり、並列を増やしても動かない。

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

