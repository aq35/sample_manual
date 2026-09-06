# EXP-13 DB 資格情報のローテーション: abrupt と graceful で切り替え中の接続エラーを比べる

| | |
| --- | --- |
| Experiment | EXP-13 / credential-rotation |
| Starting SHA | `76c575d62944` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 既に張られた接続はパスワードを変えても切れない。期限が効くのは新しい接続を張るとき。 2) abrupt（古いプールのまま新パスワードへ変える）と ConnMaxLifetime が短いと、    接続が張り替わるたびに旧パスワードで認証され、接続エラーが出る。 3) graceful（二重パスワードで両方有効にし、新プールを作り原子的に差し替え、古いをドレイン）なら、    切り替え中も接続エラー 0 で移行できる。移行後に古いパスワードを無効化する。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=76c575d62944+dirty |
| Started / Ended | 2026-09-06T23:32:04Z / 2026-09-06T23:32:08Z |

## Workload

- `concurrency` = 8
- `conn_lifetime` = 150ms

## Failure injection

- `rotate` = 負荷の途中で ALTER USER でパスワードを変える

## Results

### abrupt（古いプールのまま ALTER USER） — OK

ConnMaxLifetime で接続が張り替わると、旧パスワードで認証され弾かれる

| 数えたもの | 値 |
| --- | --- |
| auth_err | 579 |
| ops | 3585 |
| other_err | 0 |

- 接続が張り替わるたびに Error 1045（Access denied）。プールを作り直さないとこうなる

### graceful（新プールへ原子的に差し替え＋ドレイン） — OK

二重パスワードで両方有効な間に swap。新プールを ping で確かめ、古いプールは使い終わってから閉じる

| 数えたもの | 値 |
| --- | --- |
| auth_err | 0 |
| ops | 11588 |
| other_err | 0 |

- 切り替え中も接続エラー 0。config.ManagedSecret の onRotate からこれを呼べば自動化できる

## Verdict

資格情報のローテーションは「古いプールのままパスワードを変える」と接続エラーになる。新パスワードで新プールを作り、ping で確かめてから原子的に差し替え、古いプールをドレインすれば、切り替え中も接続エラー 0 で移行できる。ConnMaxLifetime は資格情報の期限より短くする。

## 適用範囲

- MySQL 8.0 / 単一ホスト / exp13 ユーザーを root(socket) で作成・ALTER
- 接続エラー = Error 1045 (Access denied)。新接続の認証失敗を数える
- ConnMaxLifetime=150ms で接続の張り替えを短時間に起こしている（本番の期限はもっと長い）

## 保証しない範囲・未検証

- 実際の SecretManager 連携（config.ManagedSecret.onRotate → GracefulSwap）の結線は本実験外
- レプリカ・複数ホストの資格情報同時ローテーションは未検証
- 接続数を一斉に張り替えたときの DB 側負荷（EXP-5 の飽和）は本実験の対象外

## 再利用できる成果物

- internal/credlab: プールの原子的差し替え（Holder.Swap）と GracefulSwap
- config.ManagedSecret: 期限前に先回り更新し onRotate を呼ぶ → GracefulSwap に繋げる

## 次の実験

- なし

