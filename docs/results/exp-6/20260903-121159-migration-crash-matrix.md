# EXP-6 マイグレーションの各段階で落ちたとき、半端な状態を成功扱いしないか

| | |
| --- | --- |
| Experiment | EXP-6 / migration-crash-matrix |
| Starting SHA | `ee5dbd402930` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) どの段階で落ちても、実際には当たっていないマイグレーションが done として記録されることはない。 2) DDL を当てたのに done を記録する前に落ちた場合、次の起動は黙って進まず、    『途中で終わっている』と言って止まる。 3) 8プロセス同時起動でも、同じ DDL が2回走ることはない。 4) 適用済みファイルの改変は checksum で拒否される。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=ee5dbd402930+dirty |
| Started / Ended | 2026-09-03T12:11:59Z / 2026-09-03T12:12:00Z |

## Workload

- `database` = workerdb2（本番用とは別）
- `migrations` = 3

## Failure injection

- `concurrency` = 8
- `stages` = [before_lock after_lock after_started during_ddl after_ddl_before_done after_done after_all]

## Results

### 各段階で SIGKILL したあとの、次の起動 — OK

started → DDL → done の二相記録と checksum で、半端な状態を成功扱いしないか

| 数えたもの | 値 |
| --- | --- |
| false_done | 0 |
| stages | 7 |

- before_lock            直後=記録なし → 次の起動=完了
- after_lock             直後=記録なし → 次の起動=完了
- after_started          直後=1:started（成果物 0/4） → 次の起動=停止: 前回のマイグレーションが途中で終わっている: [0001_robot_profile]
- during_ddl             直後=1:done,2:done,3:started（成果物 3/4） → 次の起動=停止: 前回のマイグレーションが途中で終わっている: [0003_profile_retired]
- after_ddl_before_done  直後=1:started（成果物 1/4） → 次の起動=停止: 前回のマイグレーションが途中で終わっている: [0001_robot_profile]
- after_done             直後=1:done（成果物 1/4） → 次の起動=完了
- after_all              直後=1:done,2:done,3:done（成果物 4/4） → 次の起動=完了

### 8プロセス同時起動（crash なし） — OK

| 数えたもの | 値 |
| --- | --- |
| failed | 0 |
| migrations_recorded | 3 |
| succeeded | 8 |

- 成功 8 / 失敗 0 / 記録されたマイグレーション 3 件
- GET_LOCK で直列化しているので、同じ DDL が2回走ることはない（docs/locking.md）

### 8プロセス同時起動 ＋ 1つが DDL 適用後・done 記録前に落ちる — OK

| 数えたもの | 値 |
| --- | --- |
| failed | 0 |
| succeeded | 8 |
| unfinished_rows | 0 |

- 成功 8 / 失敗 0 / 途中のまま残った記録 0 件
- 落ちたプロセスのロックは接続が切れて解放され、次のプロセスが入る
- 次のプロセスは『途中で終わっている』を見つけて止まる。**同じ DDL を勝手に流し直さない**

### 適用済みマイグレーションの改変 — OK

| 数えたもの | 値 |
| --- | --- |
| exit_code | 3 |

- 起動を止めた: MIGRATE_ERROR 適用済みのマイグレーションが書き換えられている: 0001_robot_profile（適用済みのファイルは書き換えず、新しい番号を足すこと）

## Verdict

7段階すべてで、成果物が無いのに done と記録されたものは 0 件だった。DDL を当てたあと done を記録する前に落ちた場合、次の起動は黙って進まず、『途中で終わっている』と言って止まる（自動で流し直さない）。8プロセス同時起動でも同じ DDL は2回走らず、適用済みファイルの改変は checksum で拒否された。

## 適用範囲

- MySQL 8.0 / 実験用データベース workerdb2 / マイグレーション3本（うち1本は2文）
- 排他は GET_LOCK（接続固定）。プロセスが死ねば接続が切れてロックは解放される
- 『同時に走らせない』と『途中で落ちる』を別々の実験として測っている

## 保証しない範囲・未検証

- MySQL の DDL は暗黙コミットするため、複数文のマイグレーションを巻き戻す方法は無い
- バックアップからの復元は EXP-11 で扱う（ここでは未検証）
- オンライン DDL（ALGORITHM=INPLACE）の途中で落ちた場合の残骸は未検証
- レプリカへの伝播中に落ちた場合は未検証

## 再利用できる成果物

- repo.Options.MigrationHook: 本番のマイグレーション処理をそのまま使って段階ごとに落とせる
- cmd/migratelab -report: crash 後の状態（記録と実際の成果物）を JSON で出す

## 次の実験

- EXP-11 backup/restore: 半端な状態から復元できることの証明

