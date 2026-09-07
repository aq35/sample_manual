# クエリのタイムアウトとキャンセル伝播（EXP-36）

クライアントがタイムアウトしても DB 側のクエリが走り続けて接続を握りっぱなしだと、プールが
枯れて全体が止まる。context のキャンセルがちゃんと DB まで届くかを実測した。

実装は [internal/cancellab](../internal/cancellab)、receipt は
[docs/results/exp-36](results/exp-36/exp-36-context-cancel-kills-query.md)。

## 結果

| ケース | 所要 | DB 側 |
| --- | --- | --- |
| `SELECT SLEEP(3)` を 300ms の context timeout で | **301ms で戻る**（`context deadline exceeded`） | 幽霊 SLEEP **0**（クエリも止まる） |
| timeout 無し（対照） | 1000ms（フルに待つ） | — |
| `MAX_EXECUTION_TIME(300)` ヒント付き | **301ms で頭打ち** | サーバが打ち切る |

- **timeout つき context を渡せば、呼び出しは即戻り、DB 側の実行中クエリも止まる**。go-sql-driver は
  ctx キャンセルで実行中クエリを打ち切り、接続を握り続けない（＝プール枯れを防ぐ）。
- `MAX_EXECUTION_TIME` は実行時間を頭打ちにする**サーバ側の保険**。ただし `SLEEP` は打ち切り時に
  error を返さず値を返す MySQL の癖がある（実データを走査する SELECT なら `3024` で error になる）。

## 何をすればよいか

- **すべての DB 呼び出しに timeout つき context を渡す**（`context.Background()` を直接使わない）。
  Web はリクエストの deadline、Worker は1件あたりの上限を context に載せる。
- 渡さないと、遅い1本のクエリが接続を占有し続け、プールが枯れて**無関係なリクエストまで巻き込む**
  （[EXP-5](pool-saturation.md) の膝・[EXP-29](web-worker-split.md) の acquire 待ち）。
- **読み取りにはサーバ側 `MAX_EXECUTION_TIME` も併用**（クライアント任せにしない二重の保険）。
  暴走 SELECT を DB 自身が打ち切る。
- 書き込みは `MAX_EXECUTION_TIME` では止まらない。ロック待ちは `innodb_lock_wait_timeout` で上限を。
- タイムアウトで打ち切られたクエリは**やり直しになる**。冪等な読み/操作（[EXP-27](graphql.md)）だけを
  安全に retry する（[EXP-33](db-resilience.md) と同じ原則）。

## 保証しない範囲・未検証

- driver がキャンセル時に接続を閉じるか KILL するかは版依存。結果（即戻る・幽霊なし）は同じ。
- `MAX_EXECUTION_TIME` は読み取り専用 SELECT にだけ効く。書き込み・ロック待ちは別の仕組み。
- ネットワーク断起点のタイムアウトは別（[EXP-33](db-resilience.md)）。ここはアプリ起点のキャンセル。
