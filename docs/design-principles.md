# 関心ごと × 考え方 × どうあるべき（一覧）

常時稼働の Go+MySQL ワーカー/API で「何を・どう考え・どうあるべきか」を関心ごとに1行へ。
各行に裏づけ実験を付けた（すべて receipt つき。[docs/experiments.md](experiments.md) が一覧）。
末尾に**漏れチェック**（埋まっている／まだ開いている）を置く。

数値の早見表は [docs/reference-numbers.md](reference-numbers.md)。

---

## A. 正しさ（クラッシュ・重複・順序・外部作用）

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| クラッシュと外部作用 | 「やった」と「記録した」がズレる瞬間がある | 外部 effect の後にクラッシュしたら `OUTCOME_UNKNOWN`。後で照合・冪等再実行 | [EXP-1](../internal/expkit) |
| 二重実行 | 同じ命令が2回走りうる（再送・リース失効） | mutation は冪等キー（`uq_idem`）で二度目を弾く | [EXP-27](graphql.md) |
| 排他とゾンビ | 死んだと思ったワーカーが生きている | lease＋fence トークン。古い fence の書き込みを拒否 | [EXP-2](fencing.md)/[EXP-30](fencing.md) |
| 外部作用の exactly-once | at-least-once 配送 × 冪等消費 = 実質1回 | transactional outbox で「DB コミットと送信予約」を1トランザクションに | [EXP-44](outbox.md) |
| 無限リトライ | 必ず失敗する命令がキューを詰まらせる | 試行上限→dead-letter に隔離、良い命令は流す | [EXP-45](dead-letter.md) |
| エラー分類 | 何でも retry は恒久エラーを隠して DB を叩く | 恒久(1062等)は fail-fast、一時(1213/1205/接続断)だけ backoff | [EXP-49](retry.md) |
| 順序・重複 | 再送・並行で逆順/重複が届く | キーごとに版を持ち「新しい版だけ適用」（単調・冪等） | [EXP-47](event-ordering.md) |
| マイグレーション途中落ち | DDL の途中でクラッシュしうる | expand/contract で常に前方後方互換。途中状態でも動く | [EXP-6](migration-crash.md)/[EXP-32](zero-downtime-migration.md) |

## B. 性能・クエリ

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| 何が重いか | 総行数でなく**走査行数 × 行幅 × 取る列** | covering index で走査を絞る。`SELECT *` を避け射影 | [EXP-18](date-search.md)/[EXP-21](column-projection.md) |
| 太い列 | 型名(TEXT/VARCHAR)でなく inline/off-page で決まる | 太いメモ列は別表 or off-page。一覧の舐めから外す | [EXP-19](table-split.md)/[EXP-21](column-projection.md) |
| ページング | OFFSET は後ろほど重い | keyset ページング（`WHERE id > ?`） | [EXP-18](date-search.md) |
| N+1 | 1件ずつ引くと往復が爆発 | DataLoader でまとめて引く（バッチ） | [EXP-23](graphql.md) |
| 実行前ガード | スロークエリログに出る頃は手遅れ | 走査見込み（EXPLAIN rows）で実行前に弾く | [EXP-7](query-cost-gate.md) |
| データ偏り | 同じクエリでもテナントで実行計画が変わる | 偏りを測り、必要なら index/ヒント。統計を最新に | [EXP-7](query-plan-skew.md) |
| ステータス別表 | 状態でテーブルを分けるべきか | まず status index。分割は書き込み競合を測ってから | [EXP-22](status-table-split.md) |
| メモリ単価 | 「2GB あるから無限」ではない | 1件の単価 × 件数が予算内か。単価の 2〜3 倍を安全率 | [EXP-50](reference-numbers.md) |
| ループ・確保 | 同じ結果でも確保の仕方で ns/alloc が桁違い | 事前確保・Builder・map サイズ指定・boxing 回避 | [EXP-51](reference-numbers.md) |

## C. 容量・スケール

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| 1タスクの容量 | 律速は種類で違う（メモリ/DB/CPU） | SSE=メモリ、Worker=DB往復、Web=CPU で見積る | [EXP-31](capacity.md) |
| 縦 vs 横 | 大きい1台より小さい複数台 | 横に割る（GC 停止・障害影響・接続予算が有利） | [EXP-30](redundancy.md) |
| 接続予算 | 合計接続が DB の天井を超える | Web/Worker/レプリカの合計を poolbudget で確認 | [EXP-5](pool-saturation.md)/[EXP-29](web-worker-split.md) |
| ポーリング頻度 | 速すぎ＝空振り、遅すぎ＝遅延 | 1回で batch 件まとめる。`処理量 ≒ batch/interval` | [EXP-12](scheduling.md) |
| fan-out を畳む | テナント×ロボットで往復が乗算 | 共有クエリに畳む（テナント分離は保つ） | [EXP-14](fanout.md) |
| 担当テナント数 | IN の肥大で計画が崩れる | IN サイズに上限。超えたら分割 | [EXP-16](owned-tenant-limit.md) |

## D. リアルタイム配信（SSE / hub / subscription）

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| DB ファンイン | 購読者ごとに DB を引くと往復が爆発 | 1テナント1 poller→共有 hub から fan-out。購読者は DB を引かない | [EXP-38](sse-fan-in.md)/[EXP-39](sse-fan-in.md) |
| 何人つなげるか | メモリと fd で律速 | 1タスク 1万〜1.5万本。5千×複数タスクに割る | [EXP-40](capacity.md) |
| テナント分離 | hub の混線は情報漏洩 | hub/poller はテナント単位。cross-leak=0 を検証 | [EXP-42](sse-fan-in.md) |
| プロセス跨ぎ | 複数タスクに購読者が散る | pub/sub（topic=テナント）で無効化・更新を配る | [EXP-43](sse-fan-in.md) |
| キャッシュ無効化 | push する値が stale になる | イベント失効＋短め TTL。stampede は singleflight | [EXP-46](cache.md) |
| 死んだ接続 | 数万本が幽霊化する | ハートビート＋アイドルタイムアウトで掃除 | [EXP-39](sse-fan-in.md) |

## E. セキュリティ

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| テナント分離 | 全クエリがテナント境界を越えない | `repo.Scope` で `:tenant` を強制。ctx から取得 | [EXP-24](security-layers.md) |
| フィールド認可 | 見せてよい列はロールで違う | `@auth` ディレクティブでフィールド単位 | [EXP-25](security-layers.md) |
| 行レベル認可 | この対象を操作してよいか | `canOperate`/grant 表で対象ごとに判定 | [EXP-28](security-layers.md) |
| DoS（複雑度） | 重いクエリで殺される | 複雑度上限・深さ制限・レート制限 | [EXP-24](graphql.md)/[EXP-26](graphql.md) |
| 受付制御 | 任意クエリを受けない | 永続化クエリ allowlist（sha256） | [EXP-26](graphql.md) |
| エラー秘匿 | 内部エラーを漏らさない | ErrorPresenter で秘匿。内観は本番 off | [EXP-24](graphql.md) |
| 秘密の扱い | 資格情報の平文・無期限は危険 | 有効期限つき秘密・テナント分離・ローテーション | [EXP-13](credential-rotation.md)/[secrets](secrets.md) |

## F. 回復性・障害

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| DB 切断/フェイルオーバ | 接続はいつか切れる | 切断を検知して張り直し・retry（接続断は一時エラー） | [EXP-33](db-resilience.md) |
| 過負荷 | 全部受けると全部倒れる | backpressure（キュー上限・早期 429） | [EXP-4](backpressure.md) |
| graceful shutdown | 途中の仕事を殺さない | 受付停止→in-flight 完了→クローズ | [EXP-3](shutdown.md) |
| タイムアウト伝播 | 遅いクエリが接続を占有 | context でクエリをキャンセル。伝播を確認 | [EXP-36](query-timeout.md) |
| ノイジーネイバー | 1テナントが全体を巻き込む | テナント公平性（1テナントの暴走を隔離） | [EXP-34](tenant-fairness.md) |
| バックオフ | 一斉再試行で雪崩 | 指数＋ジッタ。上限回数＋全体タイムアウト | [EXP-17](adaptive-backoff.md) |

## G. 運用・保守

| 関心 | 考え方 | どうあるべき | 実験 |
| --- | --- | --- | --- |
| 保持期間 | 履歴は増え続ける | 日パーティションの DROP で古い日を丸ごと捨てる | [EXP-48](retention.md) |
| バックアップ/復旧 | 壊れてからでは遅い | backup/restore/破損検知を演習しておく | [EXP-11](backup-restore.md) |
| 時刻・DST | ローカル時刻は飛ぶ/重なる | 保存は UTC。境界は明示的に扱う | [EXP-35](timezone.md) |
| 可観測性 | メトリクスが高カーディナリティで爆発 | ラベルの値域を絞る（テナント id を直接ラベルにしない） | [EXP-37](observability.md) |
| 静的解析 | 規約は人手だと漏れる | sqllint で rawdb/loopquery/nocontext 等を機械検査 | [EXP-8](sql-guard-fuzz.md)/[EXP-9](static-analysis.md) |
| 移植性 | ロジックを DB に固定しない | domain 契約＋言語非依存ベクタ（SQLite companion） | [EXP-10](sqlite.md) |

---

## 漏れチェック

**埋まっている**（上表・EXP-0〜51 の 52 本、すべて receipt つき）:
正しさ / 性能・クエリ / メモリ・確保 / 容量・スケール / リアルタイム配信 / セキュリティ /
回復性・障害 / 運用・保守 — 主要な関心はカバー済み。

**まだ開いている（未実験・今後の候補）**:

| 候補 | なぜ要るか | 既存で近いもの |
| --- | --- | --- |
| hub 内の複数ロボットのキャッシュ破棄 | 1 hub に複数対象がいるとき、どの粒度で失効させるか | [EXP-46](cache.md)＋[EXP-42](sse-fan-in.md)（別途要実験） |
| hub のセキュリティ全体像 | 接続時認可・購読スコープ・切断など hub 固有の面 | [EXP-42](sse-fan-in.md)/[EXP-24](security-layers.md)（別途要整理） |
| 状態 × タスク状態の購読設計 | 2 種類の状態を1つの購読で返すか分けるか | [EXP-47](event-ordering.md)/[EXP-41](graphql.md)（別途要設計） |
| PK 設計（BIGINT vs UUID）の index 肥大 | ランダム PK は B-tree を分断し書き込みが重い | 未実験 |
| トランザクション分離レベル（RR/RC）とファントム | 既定 RR の挙動・ロック範囲を測っていない | [EXP-2](fencing.md) が近い |
| bulk INSERT の正規化（単発 vs まとめ vs LOAD） | seed で使っているが単価を測っていない | [EXP-51](reference-numbers.md) が近い |
| charset/collation（utf8mb4）と index 長制限 | 多言語で index 長・照合コストが効く | 未実験 |

> このうち上の3つ（hub キャッシュ破棄・hub セキュリティ・状態×タスクの購読設計）は
> 直近の設計質問。考え方は既存実験から答えられるが、証明が要るものは実験を足す。
