# EXP-11 バックアップを別環境へ復元し、中身のハッシュとアプリの起動まで確かめる

| | |
| --- | --- |
| Experiment | EXP-11 / backup-restore-corruption |
| Starting SHA | `83598d6b4d1e` |
| Hypothesis (frozen before result) | 1) mysqldump は書いている途中でも終了コード 0 を返すので、『コマンドが成功した』は復元できることを意味しない。2) 切れたバックアップ・バイト破損は、復元が失敗するか、復元後の指紋が違うことで拒否できる。3) 古いバックアップは行数が完全に同じでも、中身のハッシュが違うことで見分けられる。4) schema_migrations が『済』と言っていても、実際の表が足りないこと（スキーマ違い）は起こる。記録ではなく実物の表を指紋で見ないと分からない。5) 正常系は、別環境へ復元してアプリ（store + repo + lease）が起動し、lease・robot_state・マイグレーション状態を読み戻せる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=83598d6b4d1e |
| Started / Ended | 2026-09-03T15:09:35Z / 2026-09-03T15:09:36Z |

## Workload

- `robots` = 200
- `tables` = [robot_state robot_state_history worker_lease robot_profile schema_migrations]

## Failure injection

- `corrupt` = 中ほどのバイトを1つ反転
- `schema_mismatch` = robot_profile を含めずにバックアップする
- `stale` = バックアップ後に値だけ変える（行数は変えない）
- `truncate` = バックアップの末尾を 60% で切る

## Results

### 正常系（--single-transaction / 別環境へ復元 / 起動） — OK

バックアップ 106781 バイト・71ms

| 数えたもの | 値 |
| --- | --- |
| アプリ起動 | 1 |
| 復元できた | 1 |

- コマンド: mysqldump -h 127.0.0.1 -P 3306 -u worker --protocol=TCP --single-transaction --skip-lock-tables --no-tablespaces --set-gtid-purged=OFF workerdb robot_state robot_state_history worker_lease robot_profile schema_migrations
- スキーマ指紋: 元 e5ba2d2701af / 復元後 e5ba2d2701af
- robot_state 200 件 / lease owner=exp11-owner-A fence=1
- 指紋は完全に一致した

### 切れたバックアップ（末尾 40% を捨てた） — OK

転送が途中で切れた・ディスクが足りなかった状況

| 数えたもの | 値 |
| --- | --- |
| 拒否できた | 1 |

- 復元コマンドのエラー: 復元に失敗: exit status 1
ERROR 1064 (42000) at line 43: You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near ''2026-09-03 03' at line 1
- ★mysqldump は書いている途中でも終了コード 0 を返す。『取れた』を信じると、切れたダンプを保管し続けることになる

### 古いバックアップ（後から値だけ変えた） — OK

行数は完全に同じ。中身だけが古い

| 数えたもの | 値 |
| --- | --- |
| 指紋も一致 | 0 |
| 行数が一致 | 1 |

- ★行数で判定していたら『同じ』に見える。中身のハッシュだけが違いを捉える
- 差: 表 robot_state の中身が違う（行数 1703 → 1703）

### スキーマ違い（robot_profile を含めないバックアップ） — OK

schema_migrations は『0003 まで済』と言うのに、実際の表が足りない

| 数えたもの | 値 |
| --- | --- |
| アプリは起動してしまう | 1 |
| 指紋で拒否 | 1 |

- ★アプリは起動できてしまう（起動を成功条件にすると見逃す）。robot_state 200 件 / lease owner=exp11-owner-A fence=1
- schema_migrations の記録（0003 まで済）を信じると『当たっている』と誤認する。実物の表を指紋で見て初めて robot_profile の欠落が分かる
- 差: スキーマが違う（e5ba2d2701af → 211e72a5ea58）
- 差: 表 robot_profile が復元後に無い

### バイト破損（中ほどを1バイト反転） — OK

| 数えたもの | 値 |
| --- | --- |
| 拒否できた | 1 |

- 復元エラー: <nil>
- 指紋が違う: [表 robot_state の中身が違う（行数 1703 → 1703）]

## Verdict

バックアップの成功条件は『コマンドが 0 で終わった』ではなく『別環境へ復元して、中身のハッシュが一致し、アプリが起動して状態を読み戻せる』こと。切れた・破損・古い・スキーマ違いのいずれも、行数ではなく指紋（スキーマ定義＋中身のハッシュ）で捉えられる。特に古いバックアップは行数が同一でも中身で見分けられる。詳細は docs/backup-restore.md。

## 適用範囲

- MySQL 8.0 / mysqldump --single-transaction（InnoDB の一貫した断面）
- 復元先はテーブル単位の権限だけで作り直せる（DROP DATABASE を使わない）
- 指紋は information_schema のスキーマ定義ハッシュ + 表ごとの中身ハッシュ + schema_migrations
- 判定は**行数ではなく中身のハッシュ**

## 保証しない範囲・未検証

- 物理バックアップ（Percona XtraBackup・スナップショット）は未検証。ここは論理（mysqldump）のみ
- point-in-time recovery（binlog の適用）は未検証
- レプリカからのバックアップ・GTID の整合は未検証
- BLOB や生成列・トリガ・ビューを含む表での指紋は未検証（このアプリには無い）
- 『別環境』は同一サーバの別スキーマ。別ホスト・別バージョンの MySQL への復元は未検証

## 再利用できる成果物

- internal/backuplab: 取る／戻す／壊す／指紋を突き合わせる。指紋は行数ではなく中身のハッシュ
- backuplab.Fingerprint.Equal: 差の理由（スキーマ・表・マイグレーション）を文で返す
- 『別環境へ復元してアプリを起動する』を成功条件にした回帰テスト

## 次の実験

- なし（EXP-1..11 完了）

