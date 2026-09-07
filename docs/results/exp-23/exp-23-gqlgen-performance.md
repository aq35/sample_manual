# EXP-23 gqlgen の N+1 と DataLoader・射影・ページ上限

| | |
| --- | --- |
| Experiment | EXP-23 / gqlgen-performance |
| Starting SHA | `068530920c27` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 一覧の各要素で子（commands）を解決すると、DataLoader が無ければ N+1（robots 1 + 台数ぶん）。 2) DataLoader を入れると、子の取得は robot_id をまとめた1クエリに畳まれる（robots 1 + commands 1）。 3) 射影: name（別表 robot_profile）を要求しなければ、その解決関数は呼ばれず profile を引かない。 4) robots(first) はサーバ側で上限が掛かる（10万件要求しても MaxPageSize で頭打ち）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=068530920c27+dirty |
| Started / Ended | 2026-09-07T05:19:24Z / 2026-09-07T05:19:24Z |

## Workload

- `cmds_per_robot` = 10
- `robots` = 150

## Results

### N+1: ローダ無し（各ロボットで commands を1クエリずつ） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| db_queries | 51 |

| 測ったもの | 値 |
| --- | --- |
| latency_ms | 116.586 |

- robots 1 + 各台の commands = 1 + 台数ぶんのクエリ

### DataLoader: robot_id をまとめて1クエリ — OK

| 数えたもの | 値 |
| --- | --- |
| db_queries | 2 |

| 測ったもの | 値 |
| --- | --- |
| latency_ms | 15.015 |

- ローダ無し 51 クエリ → 有り 2 クエリ

### 射影: name を要求しない（profile を引かない） — OK

| 数えたもの | 値 |
| --- | --- |
| db_queries | 1 |

### 射影: name を要求する（別表 robot_profile を引く） — OK

| 数えたもの | 値 |
| --- | --- |
| db_queries | 51 |

- name 無し 1 クエリ → 有り 51 クエリ（要求時だけ別表を引く）

### ページ上限: first=100000 要求 → MaxPageSize で頭打ち — OK

| 数えたもの | 値 |
| --- | --- |
| returned | 100 |

- 要求 10万 → 返却 100（無制限一覧を作らせない）

## Verdict

一覧の子フィールド（commands）は DataLoader で畳む（N+1 → 1）。name のような別表は要求時だけ引く（射影）。robots(first) はサーバ側で必ず上限を掛け、無制限一覧を作らせない。

## 適用範囲

- MySQL 8.0 / 1テナント 50台・各10命令 / gqlgen v0.17 / DataLoader=dataloadgen
- クエリ数は repo.Stats().Queries の差分（SELECT の回数）
- commands は cmd_command を robot_id で引く（by_robot 索引）

## 保証しない範囲・未検証

- 絶対レイテンシはローカルのもの。ネットワーク往復や N が増えると N+1 の差はさらに開く
- DataLoader の待ち窓（WithWait）や並行度で畳まれ方は動く
- name はここでは素朴解決。実務では name も別ローダで畳める

## 再利用できる成果物

- internal/gql: gqlgen サーバ（リゾルバ・DataLoader・複雑度・射影・ページ上限）
- docs/graphql.md: gqlgen ベストプラクティス（パフォーマンス）

## 次の実験

- EXP-24 セキュリティ（テナント分離・複雑度 DoS）

