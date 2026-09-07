# EXP-18 日付範囲検索は何件で重くなるか。総行数ではなく走査行数で決まる

| | |
| --- | --- |
| Experiment | EXP-18 / date-range-search |
| Starting SHA | `1bf983f05ca7` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) (tenant_id, observed_date) 索引があれば、日付範囲はテナント局所の range scan。    重さは『テーブルの総行数』ではなく『範囲がヒットして走査/返す行数』で決まる。 2) 同じ日付幅なら、総行数を 100K→1M に増やしても所要はほぼ変わらない。 3) 索引だけで完結（covering, COUNT や索引列）なら速い。payload を取ると本体行へ    ランダムに引きに行くので、返す行数が増えるほど重くなる。 4) 範囲がテーブルの大部分を占めると、オプティマイザが full scan(type=ALL) に切り替える。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=1bf983f05ca7+dirty |
| Started / Ended | 2026-09-07T02:05:26Z / 2026-09-07T02:09:01Z |

## Results

### 総行数 100000 / 1日検索（covering COUNT） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 274 |
| matched | 274 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 0.739 |
| p95_ms | 1.112 |

- EXPLAIN type=range key=k_date rows=274 Using where; Using index

### 総行数 1000000 / 1日検索（covering COUNT） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 2740 |
| matched | 2740 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 1.815 |
| p95_ms | 1.896 |

- EXPLAIN type=range key=k_date rows=2740 Using where; Using index

### 範囲 1日（covering COUNT / 1M行） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 2740 |
| matched | 2740 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 3.182 |
| p95_ms | 3.356 |

- type=range rows=2740 Using where; Using index

### 範囲 7日（covering COUNT / 1M行） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 35820 |
| matched | 19178 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 13.041 |
| p95_ms | 19.033 |

- type=range rows=35820 Using where; Using index

### 範囲 30日（covering COUNT / 1M行） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 154172 |
| matched | 82192 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 45.839 |
| p95_ms | 48.215 |

- type=range rows=154172 Using where; Using index

### 範囲 180日（covering COUNT / 1M行） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 528939 |
| matched | 493151 |

| 測ったもの | 値 |
| --- | --- |
| p50_ms | 620.034 |
| p95_ms | 644.509 |

- type=ref rows=528939 Using where

### 30日: covering(COUNT) vs payload取得（本体行ランダムアクセス） — OK

| 数えたもの | 値 |
| --- | --- |
| matched | 82192 |
| matched_payload | 82192 |

| 測ったもの | 値 |
| --- | --- |
| covering_p50_ms | 43.153 |
| payload_p50_ms | 477.867 |

- covering 43.153399ms → payload取得 477.867802ms（82192行の本体アクセス）
- payload は索引に無い → 行ごとに本体（clustered index）へ引きに行く。返す行数が効く
- EXPLAIN(payload): type=ref Using index condition

### 全期間（365日 ≒ 全行） — OK

| 数えたもの | 値 |
| --- | --- |
| explain_rows | 525435 |
| matched | 1000000 |

- EXPLAIN type=ref key="PRIMARY" rows=525435 Using where
- 範囲が広すぎると索引を使わず full scan になりうる

## Verdict

日付範囲検索の重さは、テーブルの総行数ではなく『範囲がヒットして走査/返す行数』で決まる。(tenant_id, observed_date) 索引があれば、同じ日付幅なら 100K でも 1M でも所要はほぼ同じ。重くなるのは、(a) 範囲が広くヒット行数が多い、(b) payload など索引外の列を取り本体行へランダムアクセスする、(c) 範囲がテーブルの大部分で full scan に切り替わる、のいずれか。対策: 索引に tenant_id を先頭で含める／必要な列を索引に載せて covering にする／範囲を絞る・keyset でページングする／巨大な追記表は日付パーティション（EXP-15）。

## 適用範囲

- MySQL 8.0 / date_search(tenant_id,id) PK + (tenant_id,observed_date,id) 索引
- 1テナントを 365 日に均等散布 + 別テナントの noise 10万行（テナント局所性の確認）
- covering = COUNT/索引列のみ。payload取得 = 本体行へのランダムアクセスを含む

## 保証しない範囲・未検証

- 絶対値はこのホスト・バッファプールに載った状態のもの。ディスクから読む状況は別
- full scan への切り替え閾値はオプティマイザの推定次第（データ分布で動く。EXP-7 参照）
- OFFSET 深いページ・複合条件（status との AND）は本実験では別途未測定

## 再利用できる成果物

- internal/datelab: 日付範囲検索の所要 vs 総行数/範囲幅/covering/full scan

## 次の実験

- なし

