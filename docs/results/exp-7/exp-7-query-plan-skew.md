# EXP-7 件数と偏りを変えたときの実行計画・走査行数・遅延

| | |
| --- | --- |
| Experiment | EXP-7 / query-plan-skew |
| Starting SHA | `bdc8149784b5` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 同じ問い合わせでも、行数が変われば選ばれる索引と Extra（filesort など）が変わる。 2) OFFSET は深くなるほど走査行数が増える。キーセット法は増えない。 3) 選択性の低い列（status）での絞り込みは、索引があっても走査行数が減らない。 4) 1テナントが 90% を占めると、そのテナントの一覧は他テナントより遅くなる。 5) N+1 は、まとめて引くより桁で遅い。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=bdc8149784b5+dirty |
| Started / Ended | 2026-09-03T22:44:25Z / 2026-09-03T22:44:30Z |

## Workload

- `datasets` = [50行・均等 5,000行・均等 100,000行・均等 100,000行・1テナントが90%]
- `indexes` = PRIMARY(tenant_id,id) / idx_tenant_created / idx_tenant_status

## Failure injection

- `none` = 故障注入なし。データの形だけを変える

## Results

### 50行・均等 — OK

t-000 の行数: 10（全 50 行）

| 数えたもの | 値 |
| --- | --- |
| rows_in_big_tenant | 10 |
| rows_total | 50 |

| 測ったもの | 値 |
| --- | --- |
| n_plus_one_ratio | 5.823 |

- 一覧: キーセット法                   type=range  key=PRIMARY            見積        4 行 / 実際に読んだ        5 行 / p50 142µs     p99 279µs     Using where
- 一覧: OFFSET                   type=ref    key=PRIMARY            見積       10 行 / 実際に読んだ       11 行 / p50 156µs     p99 390µs     
- 一覧: 索引だけで足りる                 type=range  key=PRIMARY            見積        4 行 / 実際に読んだ        5 行 / p50 161µs     p99 297µs     Using where; Using index
- 絞り込み: status（選択性が低い）         type=index_merge key=idx_tenant_status,PRIMARY 見積        1 行 / 実際に読んだ       22 行 / p50 265µs     p99 459µs     Using intersect(idx_tenant_status,PRIMARY); Using where; Using filesort
- 絞り込み: 作成日時の範囲                type=ref    key=idx_tenant_created 見積       10 行 / 実際に読んだ       11 行 / p50 199µs     p99 492µs     Using index condition
- 1件: 主キー                      type=const  key=PRIMARY            見積        1 行 / 実際に読んだ        1 行 / p50 122µs     p99 235µs     
- 10 件の取得: 1件ずつ 1.371ms / まとめて 235µs（6 倍）

### 5,000行・均等 — OK

t-000 の行数: 1000（全 5000 行）

| 数えたもの | 値 |
| --- | --- |
| rows_in_big_tenant | 1000 |
| rows_total | 5000 |

| 測ったもの | 値 |
| --- | --- |
| n_plus_one_ratio | 37.128 |

- 一覧: キーセット法                   type=range  key=PRIMARY            見積      499 行 / 実際に読んだ       50 行 / p50 267µs     p99 511µs     Using where
- 一覧: OFFSET                   type=ref    key=idx_tenant_created 見積     1000 行 / 実際に読んだ     1001 行 / p50 1.322ms   p99 1.709ms   Using filesort
- 一覧: 索引だけで足りる                 type=range  key=PRIMARY            見積      499 行 / 実際に読んだ       50 行 / p50 217µs     p99 731µs     Using where; Using index
- 絞り込み: status（選択性が低い）         type=index_merge key=idx_tenant_status,PRIMARY 見積      129 行 / 実際に読んだ     1027 行 / p50 737µs     p99 1.027ms   Using intersect(idx_tenant_status,PRIMARY); Using where; Using filesort
- 絞り込み: 作成日時の範囲                type=ref    key=idx_tenant_created 見積     1000 行 / 実際に読んだ       50 行 / p50 391µs     p99 624µs     Using index condition
- 1件: 主キー                      type=const  key=PRIMARY            見積        1 行 / 実際に読んだ        1 行 / p50 117µs     p99 225µs     
- 200 件の取得: 1件ずつ 23.096ms / まとめて 622µs（37 倍）

### 100,000行・均等 — OK

t-000 の行数: 20000（全 100000 行）

| 数えたもの | 値 |
| --- | --- |
| rows_in_big_tenant | 20000 |
| rows_total | 100000 |

| 測ったもの | 値 |
| --- | --- |
| n_plus_one_ratio | 34.208 |

- 一覧: キーセット法                   type=range  key=PRIMARY            見積    19670 行 / 実際に読んだ       50 行 / p50 201µs     p99 366µs     Using where
- 一覧: OFFSET                   type=ref    key=PRIMARY            見積    39246 行 / 実際に読んだ    10050 行 / p50 2.666ms   p99 4.52ms    
- 一覧: 索引だけで足りる                 type=range  key=PRIMARY            見積    19670 行 / 実際に読んだ       50 行 / p50 588µs     p99 703µs     Using where; Using index
- 絞り込み: status（選択性が低い）         type=range  key=PRIMARY            見積    39246 行 / 実際に読んだ      133 行 / p50 283µs     p99 460µs     Using where
- 絞り込み: 作成日時の範囲                type=ref    key=idx_tenant_created 見積    39246 行 / 実際に読んだ       50 行 / p50 462µs     p99 639µs     Using index condition
- 1件: 主キー                      type=const  key=PRIMARY            見積        1 行 / 実際に読んだ        1 行 / p50 125µs     p99 285µs     
- 200 件の取得: 1件ずつ 25.922ms / まとめて 758µs（34 倍）

### 100,000行・1テナントが90% — OK

t-000 の行数: 90000（全 100000 行）

| 数えたもの | 値 |
| --- | --- |
| rows_in_big_tenant | 90000 |
| rows_total | 100000 |

| 測ったもの | 値 |
| --- | --- |
| n_plus_one_ratio | 37.643 |

- 一覧: キーセット法                   type=range  key=PRIMARY            見積    49300 行 / 実際に読んだ       50 行 / p50 238µs     p99 432µs     Using where
- 一覧: OFFSET                   type=ref    key=PRIMARY            見積    49300 行 / 実際に読んだ    45050 行 / p50 9.442ms   p99 11.286ms  
- 一覧: 索引だけで足りる                 type=range  key=PRIMARY            見積    49300 行 / 実際に読んだ       50 行 / p50 374µs     p99 572µs     Using where; Using index
- 絞り込み: status（選択性が低い）         type=ref    key=PRIMARY            見積    49300 行 / 実際に読んだ      133 行 / p50 198µs     p99 325µs     Using where
- 絞り込み: 作成日時の範囲                type=ref    key=idx_tenant_created 見積    49300 行 / 実際に読んだ       50 行 / p50 413µs     p99 630µs     Using index condition
- 1件: 主キー                      type=const  key=PRIMARY            見積        1 行 / 実際に読んだ        1 行 / p50 117µs     p99 408µs     
- 200 件の取得: 1件ずつ 24.438ms / まとめて 649µs（38 倍）

### 同じ問い合わせ・大きいテナント vs 小さいテナント — OK

テナントごとに行数が違えば、同じ SQL でも選ばれる索引・走査行数・遅延が変わる

| 測ったもの | 値 |
| --- | --- |
| big_over_small_p50 | 0.189 |

- t-000（90000 行）: type=ref key=PRIMARY 見積 49300 行 / 実読 133 行 / p50 229µs
- t-001（2500 行）: type=index_merge key=idx_tenant_status,PRIMARY 見積 39 行 / 実読 2524 行 / p50 1.214ms
- ★仮説4（大きいテナントのほうが遅くなる）は **外れた**。
- 　小さいテナントのほうが index_merge を選ばれ、テナント全行を読んで遅くなった。
- 　オプティマイザはテナントごとの選択性の見積もりで判断するので、『行数が少ない = 速い』とは限らない。
- 　『平均の応答時間』はこの差を隠す。テナント別に見る必要がある

## Verdict

同じ問い合わせでも、行数が変われば選ばれる索引も Extra も変わる（5,000行では idx_tenant_created + filesort、100,000行では PRIMARY の range）。OFFSET は深さに比例して走査行数が増え（11 → 1,001 → 10,050 → 45,050 行）、キーセット法は 50 行のまま。N+1 は 200 件で 32〜37 倍遅い。★仮説4は外れた: 偏りがあるとき遅くなったのは **小さいほうのテナント** で、index_merge を選ばれてテナント全行を読んでいた。『行数が少ない = 速い』は成り立たない。

## 適用範囲

- MySQL 8.0 / 同一ホスト / 1表・3索引（主キー + 作成日時 + status）
- 遅延はバッファプールが温まった状態での値。1回目（初回アクセス）は別に記録している
- 1件あたり payload 180 バイト

## 保証しない範囲・未検証

- **本当のコールドキャッシュは未検証**（バッファプールを空にするには再起動か縮小が要る）。ここでの『初回』は、直前の投入でページが載っている可能性がある
- 索引の追加・削除が既存の計画に与える影響は EXP-6 の 0002/0003 で部分的に見ただけ
- 結合（JOIN）を含む問い合わせは測っていない
- 統計情報の更新タイミング（ANALYZE TABLE の有無）による揺れは1回しか確認していない

## 再利用できる成果物

- internal/planlab: 件数・偏りを変えてデータを作り、EXPLAIN と Handler_* の差分（実際に読んだ行数）と遅延を1つの接続で測る道具
- repotest.RequireIndexed と組み合わせると、『本番に近い件数で計画を固定する』テストが書ける

## 次の実験

- EXP-8 SQL guard の fuzz

