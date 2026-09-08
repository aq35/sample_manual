# EXP-61 律速に合う lever を選ぶ: 接続律速→Aurora、CPU律速→タスク（AWS 未使用の判断モデル）

| | |
| --- | --- |
| Experiment | EXP-61 / cost-scaling-priority |
| Starting SHA | `4476c9fcf95e` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 接続律速（タスク×pool が Aurora の max_connections を超える）では、タスクを足しても使える rps は増えない（EXP-60）→ Aurora↑ が第一優先。 2) 接続に余裕がある CPU 律速では、タスク数/スペックが第一優先で、Aurora↑ は web rps を増やさない。 3) Fargate は線形料金なので『タスク数↑』と『スペック↑』は 1rps あたり同程度→分離に有利な横（タスク数）を既定に。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=4476c9fcf95e+dirty |
| Started / Ended | 2026-09-08T21:56:00Z / 2026-09-08T21:56:00Z |

## Results

### 接続律速（12台希望・小Aurora=90接続/pool10→9台で頭打ち）: 第一優先=Aurora↑ — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| more_tasks_applicable | 0 |
| top_is_aurora | 1 |
| usable_tasks | 9 |

- 律速=connections / 第一優先=bigger-aurora（タスク増は 無駄）

### CPU律速（8台・大Aurora接続余裕・rps不足）: 第一優先=タスク（Aurora↑は効かない） — OK

| 数えたもの | 値 |
| --- | --- |
| more_tasks_applicable | 1 |
| top_is_task | 1 |
| web_rps_now | 1600 |

- 律速=app-cpu / 現状 1600rps / 第一優先=more-tasks

### Fargate 線形: 『タスク数↑』と『スペック↑』は 1kRPS あたり同程度 → 横を既定に — OK

| 測ったもの | 値 |
| --- | --- |
| bigger_task_usd_per_1kRPS | 180.201 |
| more_tasks_usd_per_1kRPS | 180.201 |

- 横 $180.20 / 縦 $180.20 per 1kRPS（近い→GC/障害影響で横）

## Verdict

月予算のもとでの優先順位は『律速に合う lever に使う』が原則。接続律速（タスク×pool > Aurora の max_connections）ではタスクを足しても無駄（EXP-60）→ Aurora↑（or RDS Proxy）が第一。接続に余裕が ある CPU 律速ではタスク数/スペックが第一で Aurora は効かない。Fargate は線形料金なので横と縦は 1rps あたり同程度→GC 停止・障害影響で有利な横（タスク数）を既定に。最優先は $0 のクエリ修正/pool 適正化。料金は AWS 公式から入れ、単位容量は実機で測って置き換える。

## 適用範囲

- 純 Go の判断モデル / 単位容量 RPSPerVCPU=200・pool10（実機で置換）/ 料金は入力（AWS 公式から）
- 接続律速の判定は UsableTasks=max_connections/pool（EXP-60）。web rps=使えるタスク×vCPU×RPSPerVCPU
- 検証しているのは『律速→lever』の向き（料金の絶対値に依らない）

## 保証しない範囲・未検証

- RPSPerVCPU・pool・SSE/GiB は実機で測って入れる（EXP-31/59）。1リクエストの重さで大きく動く
- 料金は変動する。AWS の Fargate/Aurora 公式料金ページから現在値を入れる。リージョン差・Savings Plans も
- RDS Proxy を挟むと接続律速が緩む（DB 接続がタスク数に比例しない・rds-proxy.md）→ 第4の lever
- DB スループット律速（クエリが遅い/DB CPU 飽和）は別軸。まずクエリ修正($0)→レプリカ→Aurora↑

## 再利用できる成果物

- internal/costlab: 律速→lever の優先順位を出すコストモデル
- docs/cost-scaling-priority.md: 月予算での縦/横/Aurora の優先順位

## 次の実験

- （アーキテクチャ×コスト）

