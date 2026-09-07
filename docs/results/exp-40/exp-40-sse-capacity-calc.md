# EXP-40 SSE は何人まで（hub あり・なし）を実測係数から計算

| | |
| --- | --- |
| Experiment | EXP-40 / sse-capacity-calc |
| Starting SHA | `4ee03b5c73be` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) hub ありの天井はメモリ/fd（1接続 ~34KB）。DB 読みは接続数に無関係（テナント数 / 間隔）。 2) hub なしの DB 読みは接続数に比例（接続数 / 間隔）＝すぐ DB 予算を食う。 3) だから hub なしは数百〜低い千で頭打ち、hub ありは 1タスク 1万超＋横に線形。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=4ee03b5c73be+dirty |
| Started / Ended | 2026-09-07T10:49:09Z / 2026-09-07T10:49:09Z |

## Results

### 既定: 30テナント・1万接続・1秒 — OK

| 数えたもの | 値 |
| --- | --- |
| nohub_max_conns | 2000 |
| per_task_conn_cap | 15034 |
| tasks_needed | 1 |

| 測ったもの | 値 |
| --- | --- |
| hub_db_reads_per_sec | 30.000 |
| nohub_db_reads_per_sec | 10000.000 |

- hub あり: 1タスク 15034 接続（memory律速）× 1 タスク / DB 読み 30/秒（接続数に無関係）
- hub なし: DB 読み 10000/秒 → 予算内=いいえ、予算で許される最大 2000 接続

### 間隔を緩める: 30テナント・1万接続・5秒 — OK

| 数えたもの | 値 |
| --- | --- |
| nohub_max_conns | 10000 |
| per_task_conn_cap | 15034 |
| tasks_needed | 1 |

| 測ったもの | 値 |
| --- | --- |
| hub_db_reads_per_sec | 6.000 |
| nohub_db_reads_per_sec | 2000.000 |

- hub あり: 1タスク 15034 接続（memory律速）× 1 タスク / DB 読み 6/秒（接続数に無関係）
- hub なし: DB 読み 2000/秒 → 予算内=はい、予算で許される最大 10000 接続

### 大規模: 500テナント・10万接続・1秒 — OK

| 数えたもの | 値 |
| --- | --- |
| nohub_max_conns | 2000 |
| per_task_conn_cap | 15034 |
| tasks_needed | 7 |

| 測ったもの | 値 |
| --- | --- |
| hub_db_reads_per_sec | 500.000 |
| nohub_db_reads_per_sec | 100000.000 |

- hub あり: 1タスク 15034 接続（memory律速）× 7 タスク / DB 読み 500/秒（接続数に無関係）
- hub なし: DB 読み 100000/秒 → 予算内=いいえ、予算で許される最大 2000 接続

### 小規模: 30テナント・300接続・1秒 — OK

| 数えたもの | 値 |
| --- | --- |
| nohub_max_conns | 2000 |
| per_task_conn_cap | 15034 |
| tasks_needed | 1 |

| 測ったもの | 値 |
| --- | --- |
| hub_db_reads_per_sec | 30.000 |
| nohub_db_reads_per_sec | 300.000 |

- hub あり: 1タスク 15034 接続（memory律速）× 1 タスク / DB 読み 30/秒（接続数に無関係）
- hub なし: DB 読み 300/秒 → 予算内=はい、予算で許される最大 2000 接続

## Verdict

hub なしは DB 律速で数百〜低い千（接続数ぶん DB を食う）。hub ありはメモリ/fd 律速で 1タスク1万〜1.5万・DB は平ら（テナント数/間隔）で横に線形。SSE をやるなら hub 必須、が数字で出る。実値（間隔・テナント数・接続数・スペック・DB 予算）を Inputs に入れれば即再計算できる。

## 適用範囲

- 純計算（DB 不要）/ 係数: 1接続 34KB(EXP-31)・安全率 0.4・予約 800MB・fd 60000・SSE の DB 予算 2000/秒
- hub あり: 天井 = min(メモリ由来, fd)。DB 読み = テナント数/間隔
- hub なし: DB 読み = 接続数/間隔。最大接続 = DB 予算 × 間隔

## 保証しない範囲・未検証

- 係数は実測ベースだが環境で動く（TLS 実装・GC・行の太さ）。実 RSS は上下する
- DB 予算 2000/秒は保守的な例。実際は DB の余力と実クエリとの取り合いで決める（EXP-5）
- fan-out CPU は無視（EXP-38）。ソケット書込み・帯域は別途（更新頻度が高いと効く）
- 複数タスクに分かれると poller はタスク（プロセス）ごとに要る → プロセス跨ぎは pub/sub（EXP-38）

## 再利用できる成果物

- internal/ssecapacity: SSE 容量計算機（実値を差し替えて再計算できる）
- docs/sse-fan-in.md: hub あり・なしの何人まで

## 次の実験

- なし

