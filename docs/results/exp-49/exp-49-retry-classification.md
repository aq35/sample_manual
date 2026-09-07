# EXP-49 恒久エラーは fail-fast・一時エラーだけバックオフ再試行する

| | |
| --- | --- |
| Experiment | EXP-49 / retry-classification |
| Starting SHA | `174e88c41208` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 恒久エラー（重複キー 1062）は Retryable=false → Do は1回で返る（fail-fast、無駄叩きしない）。 2) 一時エラー（ロック待ちタイムアウト 1205）は Retryable=true → Do はバックオフして再試行し、ロックが解けたら成功する（attempts=2）。 3) 何でも retry する素朴版は、恒久エラーで maxAttempts 回まで無駄に叩く。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=174e88c41208+dirty |
| Started / Ended | 2026-09-07T12:11:06Z / 2026-09-07T12:11:07Z |

## Results

### 恒久エラー(1062): 分類で即失敗 → 1回だけ — OK

| 数えたもの | 値 |
| --- | --- |
| attempts | 1 |
| is_1062 | 1 |
| naive_hits | 5 |

- 分類あり=1 回で確定 / 素朴に何でも retry すると 5 回無駄叩き

### 一時エラー(1205): retry して成功 — OK

| 数えたもの | 値 |
| --- | --- |
| attempts | 2 |
| first_err_retryable | 1 |
| succeeded | 1 |

| 測ったもの | 値 |
| --- | --- |
| total_ms | 1303.000 |

- attempt1 で 1205（retryable）→ backoff → attempt2 で成功。計 2 回・1.303737473s

### 分類器 Retryable: 1213/1205=true・1062=false・nil=false — OK

| 数えたもの | 値 |
| --- | --- |
| c_1062 | 0 |
| c_1205 | 1 |
| c_1213 | 1 |
| c_nil | 0 |

## Verdict

リトライは『分類してから』。恒久エラー（重複キー・構文）は何度叩いても失敗する＝即失敗し、一時エラー（デッドロック 1213・ロック待ち 1205・接続断）だけバックオフ再試行する。分類器 Retryable で判定し、恒久は1回・一時はロックが解けたら成功（attempts=2）。何でも retry すると恒久エラーを隠して DB を無駄に叩く。backoff は指数＋ジッタ、max回数と全体タイムアウトで上限を。

## 適用範囲

- MySQL 8.0 / retry_row 1行 / victim 接続は innodb_lock_wait_timeout=1 / backoff 300ms・max5
- 恒久=1062 は再 INSERT で必ず起きる。一時=1205 は別 tx が X ロック保持中に UPDATE して起こす
- 接続断(driver.ErrBadConn)も retryable（張り直し・EXP-33）。ここでは 1205 で代表

## 保証しない範囲・未検証

- デッドロック 1213 は片方が犠牲になり自動ロールバック。tx 丸ごと retry する（部分再実行は不可）
- 一時と恒久の境界は文脈次第（一意制約違反は基本恒久だが『後勝ちにしたい』設計なら upsert に）
- backoff は固定でなく指数＋ジッタが実務的（雪崩防止）。max回数と全体タイムアウトの両方で上限を
- 『何でも retry』は恒久エラーを隠して DB を無駄に叩く。分類してから retry するのが要点

## 再利用できる成果物

- internal/retrylab: Retryable 分類器と Do リトライループ
- docs/retry.md: 一時 vs 恒久エラーの分類とリトライ

## 次の実験

- （このセットの最後）

