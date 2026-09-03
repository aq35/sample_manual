# EXP-5b RDS Proxy と接続の対応関係 — **LIVE_ENV_REQUIRED**

> **この実験は未実施。** この環境には AWS が無く、RDS Proxy を立てられない。
> 実行した体裁だけ整えることはしない。ここにあるのは
> **手順・収集項目・確かめるべき主張**と、どこへ向けても走る測定器。

再現（エンドポイントを差し替えるだけ）:

```bash
# 直接 MySQL（集約なしの基準値）
go run ./cmd/proxylab -dsn "$MYSQL_DSN" -clients 8 -hold 2s

# RDS Proxy 経由
go run ./cmd/proxylab -dsn "user:pass@tcp(<proxy-endpoint>:3306)/db" -clients 8 -hold 2s
```

---

## 1. 確かめるべき主張

| # | 主張 | 判定に必要なもの |
| --- | --- | --- |
| 1 | **クライアント接続数と物理 DB 接続数は同じではない** | 両側の接続数を同時に見る |
| 2 | **どの操作が session pinning を起こすか** | 操作ごとに pinning 指標を見る |
| 3 | **pinning はいつ解除されるか** | 操作終了後の時系列 |
| 4 | **プロセスを強制終了したとき、接続とロックが残るか** | kill 後のサーバ側の状態 |

## 2. 収集する項目

**クライアント側（このリポジトリの測定器で取れる）**

- `db.Stats()`: `OpenConnections` / `InUse` / `Idle` / `WaitCount` / `WaitDuration`
- 保持しているクライアント接続数（`sql.Conn` の本数）

**サーバ側（エンドポイント経由で取れる）**

- `SHOW GLOBAL STATUS LIKE 'Threads_connected'` / `'Threads_running'`
- `SELECT COUNT(*) FROM information_schema.processlist WHERE user = ...`

**RDS Proxy 側（CloudWatch。ここが本命で、エンドポイントからは見えない）**

- `DatabaseConnections`（プロキシ → DB の実接続数）
- `ClientConnections`（アプリ → プロキシ）
- **`DatabaseConnectionsCurrentlySessionPinned`**（pinning の直接の指標）
- `DatabaseConnectionsBorrowLatency`

> **エンドポイントから見える数字だけでは pinning は確定できない。**
> `cmd/proxylab` が出すのは「クライアント接続数 vs サーバから見える接続数」までで、
> pinning の有無は CloudWatch を併せて見る必要がある。ここを混同しないこと。

## 3. 個別に比較する操作

`cmd/proxylab` が実装しているシナリオ（1本の接続で行い、保持したまま観測する）:

| シナリオ | 操作 | pinning の疑い（文書上） |
| --- | --- | --- |
| `plain_select` | ふつうの SELECT | なし |
| `transaction` | トランザクションを開いて閉じる | なし |
| `open_transaction` | トランザクションを開いたまま保持 | あり |
| `session_variable` | `SET @var = 1` | あり |
| `temp_table` | 一時表を作る | あり |
| `user_lock` | `GET_LOCK` | あり |
| `prepared_statement` | プリペアドステートメントを保持 | あり |

**「疑い」は文書上の話で、実測ではない。** 実測はこの表を埋めるために行う。

## 4. この環境で取れた基準値（直接接続）

```
接続先: 127.0.0.1:3306（RDS Proxy なし）
クライアント接続 6 本・操作後 500ms 保持

シナリオ               client   server
plain_select              6        7
transaction               6        7
open_transaction          6        7
session_variable          6        7
temp_table                6        7
user_lock                 6        7
prepared_statement        6        7
```

**集約が無ければ client ≒ server**（+1 は観測用の接続）。
RDS Proxy を挟んだとき、この表の `server` 側が減れば集約が効いている。
**減らない操作が pinning を起こしている操作**、という読み方をする。

## 5. 実施するときの手順

1. RDS Proxy を作り、対象 DB とアプリの両方から到達できるようにする
2. `-clients 8 -hold 30s` で各シナリオを1つずつ実行する
3. 各シナリオの実行中に CloudWatch で
   `ClientConnections` / `DatabaseConnections` / `DatabaseConnectionsCurrentlySessionPinned` を記録
4. 操作を終えた（接続を閉じた）あと、pinning が解除されるまでの時間を測る
5. 強制終了（`kill -9`）でプロセスを落とし、
   サーバ側の接続と `GET_LOCK` が解放されるまでの時間を測る
6. 結果を `docs/results/exp-5b/` に、この測定器の出力と CloudWatch の値の両方で残す

## 6. いまの時点で言えること・言えないこと

**言えること**

- `GET_LOCK` は接続に紐づく（`docs/locking.md` 3.1）。
  接続が集約・再利用される環境では、**ロックを取った接続と解放する接続が別になりうる**。
  RDS Proxy を挟むなら、ユーザーロックの利用は特に慎重に確認する
- セッション変数・一時表・プリペアドステートメントは、いずれも
  「セッションに状態を持つ」操作であり、集約とは本質的に相性が悪い

**言えないこと（実測していない）**

- 各操作が実際に pinning を起こすか
- pinning がいつ解除されるか
- 強制終了後に接続やロックがどれだけ残るか
