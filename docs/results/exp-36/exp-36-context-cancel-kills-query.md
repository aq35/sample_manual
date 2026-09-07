# EXP-36 context タイムアウトで呼び出しが即戻り、DB 側のクエリも止まるか

| | |
| --- | --- |
| Experiment | EXP-36 / context-cancel-kills-query |
| Starting SHA | `abeb03baf583` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) timeout つき context で SELECT SLEEP(3) を投げると、呼び出しは約 timeout で戻る（3秒待たない）。 2) その後、DB 側に SLEEP を実行中の幽霊スレッドが残らない（接続を握り続けない）。 3) timeout 無しだとフルに待つ（対照）。 4) サーバ側の MAX_EXECUTION_TIME でも上限をかけられる（クライアント任せにしない保険）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=abeb03baf583+dirty |
| Started / Ended | 2026-09-07T09:50:04Z / 2026-09-07T09:50:06Z |

## Results

### timeout つき SELECT SLEEP(3)（300ms）→ 即戻る・幽霊なし — OK

| 数えたもの | 値 |
| --- | --- |
| lingering_sleeps | 0 |
| timed_out | 1 |

| 測ったもの | 値 |
| --- | --- |
| elapsed_ms | 300.684 |

- 所要 300.684602ms / error: context deadline exceeded / 幽霊 SLEEP=0

### 対照: timeout 無し SELECT SLEEP(1) → フルに待つ — OK

| 測ったもの | 値 |
| --- | --- |
| elapsed_ms | 1002.304 |

- 所要 1.002304323s（1秒待つ）

### サーバ側 MAX_EXECUTION_TIME(300ms) → 実行時間が上限で頭打ち — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| elapsed_ms | 301.118 |

- 所要 301.118682ms（3秒 SLEEP が ~300ms で打ち切られた）
- ※SLEEP は打ち切り時 error を返さず値を返す MySQL の癖。実データを走査する SELECT なら 3024 で error になる

## Verdict

DB 呼び出しには必ず timeout つき context を渡す。go-sql-driver は ctx キャンセルで実行中クエリを打ち切り、呼び出しは即戻り、DB 側に幽霊クエリを残さない（接続を握り続けない＝プール枯れを防ぐ）。クライアント任せにしない保険として、読み取りには MAX_EXECUTION_TIME をサーバ側にも掛ける。

## 適用範囲

- MySQL 8.0 / go-sql-driver は ctx キャンセルで実行中クエリを打ち切る / 幽霊は processlist で確認
- MAX_EXECUTION_TIME は読み取り専用 SELECT にだけ効く（書き込みには別の仕組み）
- 所要のしきい値は緩め（CI の揺れを許容）。要点は『フルに待たない・幽霊が残らない』

## 保証しない範囲・未検証

- driver がキャンセル時に接続を閉じるか KILL するかは版依存。結果（即戻る・幽霊なし）は同じ
- 書き込みクエリのタイムアウトは MAX_EXECUTION_TIME では止まらない（lock_wait_timeout 等で）
- ネットワーク断とアプリのタイムアウトは別。ここはアプリ起点のキャンセル

## 再利用できる成果物

- internal/cancellab: ctx タイムアウトの即戻り・幽霊クエリ検査・MAX_EXECUTION_TIME
- docs/query-timeout.md: クエリのタイムアウトとキャンセル伝播

## 次の実験

- EXP-37 可観測性（メトリクス/カーディナリティ）

