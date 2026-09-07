# トランザクション分離レベル（RR vs RC）（EXP-55）

MySQL/InnoDB の既定は **REPEATABLE READ (RR)**。RR はトランザクション開始時点の
**スナップショット**を読み続ける（tx 内で再読しても同じ＝non-repeatable read が起きない）。
さらに範囲の**ロック読み**で **gap ロック**を取り、範囲内への INSERT（phantom）を防ぐ。
**READ COMMITTED (RC)** は文ごとに最新の確定値を読み（再読で変わりうる）、gap ロックを基本
取らないので**ロック競合は減る**が **phantom は起きうる**。

実装は [internal/isolationlab](../internal/isolationlab)、receipt は
[docs/results/exp-55](results/exp-55/exp-55-isolation-rr-vs-rc.md)。

## 結果

| 挙動 | REPEATABLE READ（既定） | READ COMMITTED |
| --- | --- | --- |
| tx 内の再読（別 tx が +100 commit） | **1 → 1**（変わらない・スナップショット） | **1 → 101**（最新が見える） |
| 範囲ロック読み中の隙間 INSERT | **ブロック(1205)**（gap ロック） | **通る**（gap ロックなし） |

- RR は `SELECT v WHERE k=10` を2回読んでも同じ（別 tx の commit が見えない）。
  RC は2回目に最新の確定値が見える（non-repeatable read）。
- RR は `SELECT ... WHERE k>10 AND k<30 FOR UPDATE` 中、隙間 `k=15` の INSERT を gap ロックで
  ブロックする（phantom 防止）。RC は gap を取らないので同じ隙間への INSERT が通る。

## 要点

- **既定の RR のままで多くは安全**。一貫性読み（非ロック SELECT）は MVCC スナップショットで、
  レポート・集計が tx 内でブレない。範囲を `FOR UPDATE` でロックすれば phantom も防げる。
- **RC を選ぶのは「ロック競合が問題」なとき**。gap ロックが減るぶん並行 INSERT に強い。ただし
  再読が変わる・phantom を許すので、**アプリ側で版（[EXP-47](event-ordering.md)）や一意制約で守る**必要がある。
- 選び方の順序: **まず RR。ロック待ち/デッドロックが実測で問題**（[EXP-49](retry.md)）**になり、かつ
  phantom をアプリで守れる**なら RC を検討。「なんとなく RC」にはしない。
- gap ロックは**索引レンジのロック読み**（`FOR UPDATE`・`LOCK IN SHARE MODE`）で発生する。
  ただの読み取りには不要（一貫性読みで足りる）。

## 保証しない範囲・未検証

- RR の一貫性読みは MVCC。ロック読み（FOR UPDATE/共有）は最新＋gap ロックで挙動が違う（本実験は両方に触れた）。
- デッドロック(1213)・ロック待ち(1205)・一意競合(1062)は別軸（[EXP-49](retry.md)/[EXP-2](fencing.md)）。ここは分離レベルの2挙動に集中。
- レプリケーションの binlog 形式（ROW/STATEMENT）と RC の相性は本実験外（RC は ROW 前提）。
