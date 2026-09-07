# DB 切断・フェイルオーバへの耐性（EXP-33）

DB が再起動・フェイルオーバすると、プールが握っていた接続は切れる。このセッション中も
MySQL が何度も落ちた（[mysql-stop-forensics](mysql-stop-forensics.md)）。だから「切れても
自力で戻るか」は実運用の必須項目。接続を KILL して切断を模して確かめた。

実装は [internal/resiliencelab](../internal/resiliencelab)、receipt は
[docs/results/exp-33](results/exp-33/exp-33-db-failover-resilience.md)。

## 結果

| 状況 | 結果 |
| --- | --- |
| アイドル接続を 6 本切断 → 直後に 50 クエリ | **エラー 0**（次のクエリで自動的に張り直す） |
| 実行中のクエリ（SLEEP）を切断 | その1本は **error（invalid connection）**。自動リトライされない |
| 実行中切断の後の新クエリ | エラー 0（回復） |

## 何をすればよいか

- **自前の再接続コードは要らない**。`database/sql` はアイドル接続が死んでいると `ErrBadConn` を
  検知し、新しい接続で自動的にやり直す。プールを使い、接続を長く握らない（`*sql.Conn` を
  自分でキャッシュして使い回さない）だけでよい。
- **`ConnMaxLifetime` を短め**にしておく（例 数分）。フェイルオーバ後、DB 側では無効になった
  古い接続を、クライアントが掴み続けないための保険。`ConnMaxIdleTime` も併用。
- **実行中に切られたクエリは自動リトライされない**（送信済みなので当然）。だから
  **冪等な読み・冪等な操作（[EXP-27](graphql.md)）だけをアプリ側で retry** する。書き込みは
  冪等キーで再送を吸収する。トランザクション中の切断はロールバックされる。
- リトライしてよいエラーは限定する（デッドロック 1213・ロック待ち 1205・接続断）。
  それ以外（制約違反など）は retry しても無駄なので即失敗させる。ジッタつきバックオフ（[EXP-17](adaptive-backoff.md)）。

## readiness との連動

- フェイルオーバ中は `/readyz`（プールの疎通）を落とし、ロードバランサから外す。復旧したら戻す
  （[web-worker-split](web-worker-split.md) の readiness）。落とすのは readiness で、liveness では
  ない（プロセスは生きているので kill させない）。

## 保証しない範囲・未検証

- 実 DB のフェイルオーバは DNS 切替・read-only 昇格・書き込み先の変更を伴い、KILL より複雑。
  ここは「接続が切れる」局面だけを模した。
- 回復に要する時間・一時的なエラー本数は、切断本数・プール・タイミングで動く。
- マルチ AZ・レプリカ昇格の整合（GTID）や split-brain は本実験外。
