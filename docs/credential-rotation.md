# DB 資格情報のローテーション（EXP-13）

- 実験: `internal/credlab` / `MYSQL_DSN=... go test ./internal/credlab/ -run TestEXP13 -v`
- 結果: [docs/results/exp-13/exp-13-credential-rotation.md](results/exp-13/exp-13-credential-rotation.md)

## 事実（EXP-5 と地続き）

- プール内の**既に張られた接続は、パスワードを変えても切れない**（サーバが切るまで有効）。
- 期限が効くのは**新しい接続を張るとき**。だから `ConnMaxLifetime` を**資格情報の期限より短く**して、
  期限が来る前に接続を自然に張り替える。

## 測ったこと

負荷（8並行の `SELECT 1`）をかけながら、途中でパスワードをローテーションし、
接続エラー（Error 1045 Access denied）を数えた。`ConnMaxLifetime=150ms` で張り替えを短時間に起こす。

| 方式 | ops | 認証エラー |
| --- | --- | --- |
| **abrupt**（古いプールのまま `ALTER USER`） | 3,515 | **570** |
| **graceful**（二重パスワード + 新プールへ差し替え） | 11,407 | **0** |

## abrupt がダメな理由

古いプールを持ったままパスワードだけ変えると、`ConnMaxLifetime` で接続が張り替わるたびに
**旧パスワードで認証されて弾かれる**。570 件の Access denied。

## graceful（ゼロダウンタイム）

鍵は **MySQL 8.0 の二重パスワード**:

```sql
ALTER USER 'u'@'%' IDENTIFIED BY 'new' RETAIN CURRENT PASSWORD;  -- 古いも新しいも両方有効
```

手順:

1. `RETAIN CURRENT PASSWORD` でローテーション → **古い pw も新しい pw も両方有効**な期間を作る。
2. 新パスワードで**新プールを開き、ping で使えることを確かめてから、原子的に差し替える**（`Holder.Swap`）。
3. 古いプールは**即閉じない**。使用中のクエリが終わるまで**ドレインしてから閉じる**（一斉に切ると EXP-5 の飽和事故）。
4. 移行が終わったら `ALTER USER ... DISCARD OLD PASSWORD` で古いパスワードを無効化。

両方有効な期間があるので、古いプールの接続（古い pw）も弾かれず、**接続エラー 0**。

## 秘密の更新と繋ぐ

`config.ManagedSecret`（`docs/secrets.md`）が期限前に先回りで新パスワードを取得し、
`onRotate(newPw)` を呼ぶ。そこから `credlab.GracefulSwap` を呼べば、
**資格情報の期限切れを、接続エラー 0 で自動的に乗り切れる**。

## 未検証

- 実際の SecretManager 連携（onRotate → GracefulSwap の結線）は本実験の外
- レプリカ・複数ホストの同時ローテーション
- 一斉に接続を張り替えたときの DB 側負荷（EXP-5 の飽和）
