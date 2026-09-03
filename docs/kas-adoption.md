# KAS への採用方針（最終健全性レビュー #5・#7）

`sample_manual` の実装を**そのまま KAS へコピーしない。**
各結果を次のいずれかに分類する。

- `ADOPT_AS_KAS_CONTRACT` — domain の契約として採用（言語非依存の意味）
- `ADOPT_AS_TEST_VECTOR` — 言語・DB 非依存のテストベクタとして採用
- `ADOPT_AS_REVIEW_RULE` — レビュー観点（静的検査・チェックリスト）として採用
- `REFERENCE_ONLY` — 参考。移植はしない
- `LIVE_ENV_REQUIRED` — 本番相当環境でないと実証できない
- `NOT_APPLICABLE_TO_SQLITE` — SQLite では成り立たない
- `REJECTED_WITH_REASON` — 採用しない（理由つき）

## KAS の Go 移植で最初に採る順

1. **EXP-10 → SQLite 契約とテストベクタ**（`internal/kascontract`）。KAS は SQLite 前提なので、
   ここが土台。ドライバの戻り値ではなく domain の結果型（`LeaseOutcome` など）を採る。
2. **EXP-11 → backup/restore の運用契約**。成功条件は「別環境へ復元して指紋一致・アプリ起動」。
   ただし採るのは水準 `SAME_SERVER_DIFFERENT_SCHEMA` まで。それ以外は `LIVE_ENV_REQUIRED`。

**MySQL 固有の SQL・ドライバ挙動・接続プール値は KAS へ直接持ち込まない。**

## 分類表

### EXP-10（SQLite companion）由来

| 結果 | 分類 | 備考 |
| --- | --- | --- |
| 影響行数を identity に使わない（同値 UPDATE で 0/1 が割れる） | `ADOPT_AS_KAS_CONTRACT` | `CASOutcome` へ正規化。`kascontract` に実装 |
| UPSERT の insert/update は存在確認で決める | `ADOPT_AS_KAS_CONTRACT` | `UpsertOutcome`。影響行数の 1/2/0 対 1/1/1 を吸収 |
| lease は担当・期限・fence を読んで正規化 | `ADOPT_AS_KAS_CONTRACT` | `LeaseOutcome`（ACQUIRED/HELD_BY_OTHER/STALE_FENCE/NOT_FOUND） |
| 日時は RFC3339 UTC millis の文字列で保存・比較 | `ADOPT_AS_KAS_CONTRACT` | SQLite に日時型が無い。混在すると辞書順比較が壊れる |
| NULL と空文字の区別 / 整数境界の往復 | `ADOPT_AS_TEST_VECTOR` | `contract-vectors.json` |
| CAS 成功/競合、missing row | `ADOPT_AS_TEST_VECTOR` | 両 engine で同じ domain 結果を返すことを検査 |
| PRAGMA は接続ごと（DSN に書く） | `ADOPT_AS_REVIEW_RULE` | KAS の DB オープン処理のレビュー観点。`db.Exec("PRAGMA ...")` を弾く |
| BEGIN IMMEDIATE で書き込みを直列化 | `ADOPT_AS_REVIEW_RULE` | 読んで書く処理は DEFERRED では busy_timeout が効かない |
| WAL + busy_timeout を既定にする | `ADOPT_AS_REVIEW_RULE` | 読み手と書き手を同時に動かすため |
| busy/lock の文言差（SQLITE_BUSY vs lock wait） | `ADOPT_AS_KAS_CONTRACT` | `classifyBusy` で正規化 |
| append-only を trigger で守る | `NOT_APPLICABLE_TO_SQLITE`（書き方が違う） | MySQL は SIGNAL、SQLite は RAISE(ABORT)。意味は移植可、DDL は書き直し |
| pure Go ドライバ（modernc）で cgo 不要 | `ADOPT_AS_REVIEW_RULE` | 配布容易。cgo 版は 25〜30% 速いが CGO_ENABLED でハマる |
| ドライバ間で TEXT 列の time.Time 格納形が違う | `ADOPT_AS_REVIEW_RULE` | 時刻は必ず自分で文字列化。`time.Time` を直接束縛しない |
| electric power 断への耐性（synchronous） | `LIVE_ENV_REQUIRED` | プロセス死では測れない。電源断が要る |

### EXP-11（backup/restore/corruption）由来

| 結果 | 分類 | 備考 |
| --- | --- | --- |
| 成功条件は「別環境へ復元・指紋一致・アプリ起動」 | `ADOPT_AS_KAS_CONTRACT` | 「コマンド 0 終了」を成功にしない |
| 判定は行数でなく content hash | `ADOPT_AS_KAS_CONTRACT` | 行数一致・hash 不一致で古いバックアップを検出 |
| content hash の規則を固定（列順・NULL・collation 等） | `ADOPT_AS_KAS_CONTRACT` | `HashRules`。プロセス間安定をテスト |
| 切れた/破損/古い/スキーマ違いを拒否 | `ADOPT_AS_TEST_VECTOR` | SQLite 版は EXP-10 の VACUUM INTO / integrity_check で別途 |
| `SAME_SERVER_DIFFERENT_SCHEMA` の実証 | `ADOPT_AS_KAS_CONTRACT` | ここまでが実証済み |
| 別 host・別 version・PITR・物理・リージョン間 | `LIVE_ENV_REQUIRED` | `EvidenceMatrix` の未実証行 |
| mysqldump のオプション（--single-transaction 等） | `REFERENCE_ONLY` | SQLite では `VACUUM INTO`。手法は違う（NOT_APPLICABLE_TO_SQLITE 寄り） |

### その他の実験

| 実験 | 分類 | 備考 |
| --- | --- | --- |
| EXP-9 静的検査（rawdb/txhttp/rowsaffected 等） | `ADOPT_AS_REVIEW_RULE` | `go/analysis`。KAS のコードにもそのまま当てられる |
| EXP-8 SQL guard の fuzz | `ADOPT_AS_REVIEW_RULE` | security boundary ではない旨も含めて |
| EXP-1 outbox / idempotency / OUTCOME_UNKNOWN | `ADOPT_AS_KAS_CONTRACT` | 外部 effect の扱い。DB 非依存 |
| EXP-2 lease / fencing | `ADOPT_AS_KAS_CONTRACT` | `kascontract` の lease に反映済み |
| EXP-3 graceful shutdown | `ADOPT_AS_REVIEW_RULE` | 順序と goroutine リーク検査 |
| EXP-4 backpressure | `REFERENCE_ONLY` | 具体値はワークロード依存 |
| EXP-5 接続プール飽和・RDS Proxy | `REJECTED_WITH_REASON` | **MySQL 固有の接続プール値。KAS(SQLite) には持ち込まない。** SQLite は書き込み直列で話が別 |
| EXP-6 マイグレーション crash matrix | `ADOPT_AS_REVIEW_RULE`（一部 `NOT_APPLICABLE_TO_SQLITE`） | SQLite は DDL がロールバックできるので段階記録が単純化（EXP-10） |
| EXP-7 実行計画・データ偏り | `REJECTED_WITH_REASON` | MySQL の EXPLAIN 前提。SQLite は EXPLAIN QUERY PLAN で別物 |
| 接続プール値（MaxOpenConns 等） | `REJECTED_WITH_REASON` | **KAS へ直接持ち込まない。** SQLite ではプロセス内で 1 本より複数本が速い等、意味が違う（EXP-10 で反証） |

## KAS へ持ち込まないもの（明示）

- MySQL 固有の SQL（`ON DUPLICATE KEY UPDATE`・`GET_LOCK`・`SIGNAL`・`sql_safe_updates`）
- ドライバ挙動（`clientFoundRows`・`RowsAffected` の 0/1/2）— domain へ正規化してから使う
- 接続プールの絶対値（`MaxOpenConns` 等）— SQLite では意味が違う（EXP-5/EXP-10）
- EXPLAIN 前提の実行計画テスト（EXP-7）

## 成果物

- `internal/kascontract`: domain の結果型 + 両 engine の実装 + 契約テスト（両 engine で同一結果）
- `docs/kas/contract-vectors.json`: 言語非依存のテストベクタ（`EXP_RECORD=1` で再生成）
- `backuplab.EvidenceMatrix` / `HashRules`: バックアップ運用契約の水準と hash 規則
