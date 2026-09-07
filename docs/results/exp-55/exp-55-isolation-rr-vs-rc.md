# EXP-55 RR はスナップショットで再読安定＋gap ロックで phantom 防止、RC は最新読みで gap 取らない

| | |
| --- | --- |
| Experiment | EXP-55 / isolation-rr-vs-rc |
| Starting SHA | `81eda7f21f28` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 非再読(non-repeatable read): RR は tx 内で再読しても同じ値（開始時スナップショット）。 RC は別 tx の commit が見え、再読で値が変わる。 2) phantom/gap: RR は範囲のロック読みで gap ロックを取り、隙間への INSERT をブロック(1205)。 RC は gap を取らず INSERT が通る。 3) RC は競合が減る代わりに phantom を許す（trade-off）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=81eda7f21f28+dirty |
| Started / Ended | 2026-09-07T13:10:54Z / 2026-09-07T13:10:55Z |

## Results

### RR: tx 内の再読は同じ値（スナップショット） — OK

| 数えたもの | 値 |
| --- | --- |
| changed | 0 |
| first | 1 |
| second | 1 |

- 1回目=1 → 2回目=1（別 tx が +100 commit しても見えない）

### RC: 再読で最新の確定値に変わる（non-repeatable read） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| changed | 1 |
| first | 1 |
| second | 101 |

- 1回目=1 → 2回目=101（別 tx の commit が見える）

### RR: 範囲ロック読み中は隙間 INSERT を gap ロックでブロック（phantom 防止） — OK

| 数えたもの | 値 |
| --- | --- |
| gap_insert_blocked | 1 |

- k∈(10,30) を FOR UPDATE → 隙間 15 の INSERT は 1205（ブロック）

### RC: gap ロックを取らないので隙間 INSERT が通る（phantom・競合は少ない） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| gap_insert_blocked | 0 |

- 同じ範囲でも隙間 15 の INSERT が成功する

## Verdict

MySQL 既定の RR は tx 開始時スナップショットを読み続け（再読が安定・non-repeatable read が起きない）、範囲のロック読みで gap ロックを取り phantom INSERT を防ぐ。RC は文ごとに最新の確定値を読み（再読で変わる）、gap ロックを取らないので並行 INSERT の競合は減るが phantom を許す。既定の RR のままで多くは安全。ロック競合が問題で phantom をアプリ側（版・一意制約）で守れるなら RC を検討する、という順で選ぶ。

## 適用範囲

- MySQL 8.0 InnoDB / iso_row(k PK 10,20,30) / 2接続を db.Conn で固定・victim は lock_wait_timeout=1
- non-repeatable: 別 tx の commit が tx 内の再読に見えるか。phantom: 範囲ロック中に隙間 INSERT できるか
- gap ロックは『索引レンジのロック読み(FOR UPDATE)』で発生。読み取り専用の一貫性読みには不要

## 保証しない範囲・未検証

- RR の一貫性読み(非ロック SELECT)は MVCC スナップショット。ロック読み(FOR UPDATE/共有)は最新＋gap ロック
- RC は gap ロックが減り並行 INSERT に強いが、再読が変わる・phantom を許す。アプリが版/条件で守る必要
- デッドロック(1213)や外部キー・ユニーク競合は別軸（EXP-49/2）。ここは分離レベルの2挙動に集中
- binlog を ROW にすれば RC でもレプリケーションは安全（STATEMENT だと RC は不可）。本実験はロック挙動のみ

## 再利用できる成果物

- internal/isolationlab: NonRepeatableRead / GapInsertBlocked（RR/RC の実挙動）
- docs/isolation.md: 分離レベル RR vs RC（再読の安定・gap ロック）

## 次の実験

- EXP-56 bulk INSERT の正規化

