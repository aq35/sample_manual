# EXP-22 ステータスでテーブルを分けるべきか（索引 vs 分割 vs hot/cold）

| | |
| --- | --- |
| Experiment | EXP-22 / status-table-split |
| Starting SHA | `15074bf55bf8` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) pending を古い順に取る worker クエリは、1テーブル＋status索引で狙い撃てる。終端行が大量でも、    索引が効くので速い（分割は不要）。索引が無いと、古い terminal を大量に舐めて遅い。 2) 状態を別テーブルに分けると、遷移が UPDATE 一発から DELETE+INSERT の引っ越し（要トランザクション）    になり、明確に重い。状態は『分ける鍵』でなく『索引で絞るもの』。 3) 終端行が溜まると、テーブル全体を舐める操作（全体 COUNT など）は重くなる。    → pending 抽出そのものは索引で速いまま。hot/cold 分離が効くのは全体走査と掃除（EXP-15 の DROP 10x）。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=15074bf55bf8+dirty |
| Started / Ended | 2026-09-07T04:18:09Z / 2026-09-07T04:18:29Z |

## Workload

- `active_rows` = 2000
- `page_limit` = 100
- `total_rows` = 200000

## Results

### pending 抽出: 1テーブル＋status索引（終端が大量でも） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 0.434 |

- 索引で pending だけを狙い撃つので、terminal が 99% でも速い

### pending 抽出: 1テーブル・status索引なし（全表走査） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 76.611 |

- 索引あり 434.07µs → 索引なし 76.611678ms（索引が無いと古い terminal を大量に舐める）

### pending 抽出: hot 表（active だけを隔離した小表） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 0.405 |

- 1テーブル索引 434.07µs ≒ hot 405.526µs（索引が効くなら分割しても大差ない）

### 状態遷移: 1テーブル UPDATE 一発 — OK

| 測ったもの | 値 |
| --- | --- |
| per_move_p50_ms | 1.031 |
| per_move_p95_ms | 1.484 |

### 状態遷移: 別テーブルへ引っ越し（DELETE+INSERT・要トランザクション） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| per_move_p50_ms | 1.724 |
| per_move_p95_ms | 2.199 |

- UPDATE 1.03199ms → 引っ越し 1.72414ms（2表への書き込み＋2索引更新＋tx）

### 全体 COUNT（terminal 込み・1テーブル） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 63.639 |

### 全体 COUNT（active だけの hot 表） — OK

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 0.992 |

- 1表(20万) 63.639335ms → hot(2千) 992.438µs（全体を舐める操作は溜まった terminal に比例して重い）

## Verdict

ステータスは遷移で動くので『状態ごとに表を分ける』のは基本しない（遷移が引っ越しになって重い）。status に索引を張れば、終端行が大量でも 1 テーブルのまま active 抽出は速い。分けるなら状態でなく hot/cold（動く行 vs 終わった行）。全体走査・掃除が重くなってきたら、終端をアーカイブ表へ移すか、時間でパーティションして DROP PARTITION で捨てる（EXP-15）。

## 適用範囲

- MySQL 8.0 / 20万行・うち active 1%（2千）/ status索引 (tenant_id,status,id)
- active 抽出は status IN (0,1,2) を id 順に LIMIT 100
- 遷移は active→done。UPDATE 版は st_one、引っ越し版は st_active→st_terminal を tx で

## 保証しない範囲・未検証

- 索引ありの active 抽出は terminal 量にほぼ依らない（索引シーク）。この結論は索引前提
- 掃除（terminal の削除）は本実験外。DELETE より DROP PARTITION が速いのは EXP-15（10x）
- 分割の別の代償（結合・二重書き込みの整合・移行）は本実験の対象外
- 絶対値はバッファプールに載った状態のもの

## 再利用できる成果物

- internal/statuslab: 索引あり/なし・分割・hot での active 抽出、遷移コスト、全体走査
- docs/status-table-split.md: 状態でテーブルを分けるべきかの指針

## 次の実験

- なし

