# 全実験の早見表（大分類 × 実験内容 × こうあるべき）

これまでの実験（EXP-0〜64）を **大分類 / 実験内容 / こうあるべき** で一覧にしたもの。
「こうあるべき」は各実験の**結論の要約**で、**数字と条件つきの正典は右端リンク先**にある。
実験の登録簿（状態・結果リンク）は [experiments.md](experiments.md)、作る/レビューの手順は [checklist.md](checklist.md)。

| 大分類 | 実験内容 | こうあるべき | 文書 |
| --- | --- | --- | --- |
| 測定基盤 | EXP-0 測定器の自己検査 | 測る前に測定器を疑う。分位点・環境捕捉・仮説の事前固定を測定器側でテストしておく | `internal/expkit` |
| 状態同期・冪等・回収 | EXP-1 外部 effect・crash・OUTCOME_UNKNOWN | action の戻り値を receipt にしない。結果は**独立に観測**して追記。出したが不明は「不明」のまま（嘘をつかない） | [crash-effects](crash-effects.md) |
| 状態同期・冪等・回収 | EXP-47 順序・冪等消費 | **版(version)で単調適用**。逆順・重複が来ても新しい版だけ勝つ | [event-ordering](event-ordering.md) |
| 状態同期・冪等・回収 | EXP-44 トランザクショナル outbox | 外部作用は同一 tx で outbox に書き、別プロセスが送る。exactly-once は「DB commit＋冪等な再送」で作る | [outbox](outbox.md) |
| 状態同期・冪等・回収 | EXP-45 poison / dead-letter | 無限リトライしない。試行上限を超えたら DLQ に隔離して前進する | [dead-letter](dead-letter.md) |
| 状態同期・冪等・回収 | EXP-49 一時 vs 恒久エラー | 分類してから扱う。恒久は fail-fast、一時だけ backoff で再試行 | [retry](retry.md) |
| 状態同期・冪等・回収 | EXP-62 イベント駆動 worker | event は floor(poll/reconcile)の**上に載せる遅延最適化**で置換ではない。完了は DB CAS、crash は reconcile | [event-driven-worker](event-driven-worker.md) |
| 状態同期・冪等・回収 | EXP-64 回収(in_progress→pending) | **時間指定(heartbeat < lease)＋CAS(affected_rows で奪取確認)** で書く。状態だけの回収は生存担当を奪い二重実行 | [worker-state-time](worker-state-time.md) |
| 排他・担当決め | EXP-2 lease / fencing / clock skew | 排他は lease＋**fence 番号**。基準時刻は DB 側。古い担当の遅れた書き込みは fence で弾く | [fencing](fencing.md) |
| 排他・担当決め | EXP-63 テナント割り当て | DB lease で均等割り。担当が落ちたら survivor が引き継ぎ、二重所有0・fence 単調・静的ピンは orphan を生む | [tenant-assignment](tenant-assignment.md) |
| 排他・担当決め | EXP-16 担当テナント数(IN サイズ) | IN を無制限に伸ばさない。上限を決めて分割して引く | [owned-tenant-limit](owned-tenant-limit.md) |
| ライフサイクル | EXP-3 graceful shutdown | 受付停止→処理中を待つ→接続を返す、の順。SIGTERM で在庫(処理中)を落とさない | [shutdown](shutdown.md) |
| ライフサイクル | EXP-6 migration crash matrix | 途中で落ちても前進できる形にし、止まったときの手順を先に決めておく | [migration-crash](migration-crash.md) |
| ライフサイクル | EXP-32 無停止スキーマ変更 | **expand/contract**。読み書き両対応の期間を挟んでから古い形を縮める | [zero-downtime-migration](zero-downtime-migration.md) |
| ライフサイクル | EXP-13 DB 資格情報ローテーション | プールを graceful に差し替える。無停止で新資格へ移行 | [credential-rotation](credential-rotation.md) |
| 過負荷・リトライ | EXP-4 backpressure・過負荷 | 溢れる前に絞る/落とす。無制限バッファにしてメモリを溶かさない | [backpressure](backpressure.md) |
| 過負荷・リトライ | EXP-17 適応的バックオフ | 混み具合で間隔を伸縮させる。固定間隔で叩き続けない | [adaptive-backoff](adaptive-backoff.md) |
| 接続・プール | EXP-5 pool 飽和・RDS Proxy | `MaxOpen`/`MaxIdle`/`ConnMaxIdleTime` を必ず設定。`sql.Open` はプロセスに1つ | [pool-saturation](pool-saturation.md) / [rds-proxy](rds-proxy.md) |
| 接続・プール | EXP-29 Web/Worker プール分離 | 別プロセス・別プールに分ける。共有すると片方が飽和させて巻き込む | [web-worker-split](web-worker-split.md) |
| 接続・プール | EXP-33 DB 切断/フェイルオーバ耐性 | 切断を検知して張り直す。掴んだ死接続を使い続けない | [db-resilience](db-resilience.md) |
| 接続・プール | EXP-60 接続予算・オートスケールストーム | コンテナ数×プール ≤ サーバ上限。起動時にガードで越えさせない | [connection-budget](connection-budget.md) |
| クエリ・実行計画 | EXP-7 query plan・データ偏り | 偏ったデータで plan が崩れる前提で索引を検証する（均一データだけで見ない） | [query-plan-skew](query-plan-skew.md) |
| クエリ・実行計画 | EXP-18 日付範囲検索 | 重さは**総行数でなくヒット/走査行数**。範囲を索引で絞る | [date-search](date-search.md) |
| クエリ・実行計画 | EXP-36 タイムアウト・キャンセル伝播 | context でキャンセルを伝播し、クエリに必ず上限を付ける | [query-timeout](query-timeout.md) |
| クエリ・実行計画 | EXP-8 SQL guard の fuzz | 生成 SQL は fuzz で壊れない/越境しないことを担保する | [sql-guard-fuzz](sql-guard-fuzz.md) |
| クエリ・実行計画 | EXP-9 静的解析(保守性) | 層をまたぐ import・危険パターンは CI で機械的に弾く | [static-analysis](static-analysis.md) |
| テーブル/列/主キー設計 | EXP-15 テーブル分割・パーティションと競合 | 分割は行ロック競合やスループットの特効薬ではない。掃除は `DROP PARTITION` | [table-split](table-split.md) |
| テーブル/列/主キー設計 | EXP-19 メモ列 同居 vs 別表 | 太くて低頻度の列(失敗理由等)は一覧行から外す | [column-split](column-split.md) |
| テーブル/列/主キー設計 | EXP-21 太い列 inline/off-page・SELECT * | `SELECT *` を避け必要列だけ引く。重さは VARCHAR/TEXT でなく実サイズで決まる | [column-projection](column-projection.md) |
| テーブル/列/主キー設計 | EXP-22 状態でテーブル分割するか | 状態は遷移で動くので分けない。`status` に索引。分けるなら状態でなく hot/cold | [status-table-split](status-table-split.md) |
| テーブル/列/主キー設計 | EXP-48 保持期間の運用 | 日パーティションの `DROP` でロールフォワード。`DELETE` で消さない | [retention](retention.md) |
| テーブル/列/主キー設計 | EXP-54 主キー設計 | 連番 BIGINT。ランダム UUID はページ分裂で肥大する | [primary-key](primary-key.md) |
| テーブル/列/主キー設計 | EXP-55 トランザクション分離レベル | 要件で RR/RC を選ぶ。gap ロックの効き方を理解した上で | [isolation](isolation.md) |
| テーブル/列/主キー設計 | EXP-56 bulk INSERT | multi-row INSERT を1トランザクションにまとめる（単発 tx を避ける） | [bulk-insert](bulk-insert.md) |
| テーブル/列/主キー設計 | EXP-57 utf8mb4 と index 長・照合 | index 3072B 上限を意識し、照合(_ci/_bin)を用途で選ぶ | [charset](charset.md) |
| read replica・冗長化 | EXP-20 primary/replica 読み分け | 遅延を許せる読み(実績など)だけ replica へ。書いた直後の読みは primary | [read-replica](read-replica.md) |
| read replica・冗長化 | EXP-30 冗長化(複数レプリカ) | レプリカ遅延を前提にアプリを書く（読めた=最新とみなさない） | [redundancy](redundancy.md) |
| スケジューリング・fan-out | EXP-12 ポーリング頻度・予定/命令/実績 | 予定・命令・実績を別表で表現。ポーリング頻度は許容遅延から決める | [scheduling](scheduling.md) |
| スケジューリング・fan-out | EXP-14 fan-out を畳む | テナント分離を保ったまま複数テナントの poll をまとめる | [fanout](fanout.md) |
| スケジューリング・fan-out | EXP-35 タイムゾーン/DST | UTC で保存し、業務境界(日次等)は業務 TZ で判定する | [timezone](timezone.md) |
| GraphQL(gqlgen) | EXP-23 パフォーマンス | DataLoader で N+1 を潰す。射影・ページ上限を効かせる | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-24 セキュリティ | テナント分離・複雑度 DoS 対策・内観無効・エラー秘匿 | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-25 認可 | `@auth` ディレクティブでフィールド単位ロールを強制 | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-26 受付制御 | 永続化クエリ allowlist＋レート制限で受け口を絞る | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-27 mutation 冪等性・入力検証 | idem key で二重実行を防ぎ、入力を検証してから書く | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-28 行レベル認可 | 「この対象を操作してよいか」を対象ごとに確認する | [graphql](graphql.md) |
| GraphQL(gqlgen) | EXP-41 subscription を hub で | poller＋hub で1本の上流を多重配信する | [graphql](graphql.md) |
| SSE・購読・hub | EXP-38 同一テナント多数 SSE | hub で DB ファンインを畳み、hub 上限を決める | [sse-fan-in](sse-fan-in.md) |
| SSE・購読・hub | EXP-39 動く SSE エンドポイント | poller＋fan-out＋レジストリで1コンテナが多数へ配る | [sse-fan-in](sse-fan-in.md) |
| SSE・購読・hub | EXP-40 SSE は何人まで | hub 有無で容量を計算してから台数を決める | [sse-fan-in](sse-fan-in.md) |
| SSE・購読・hub | EXP-42 hub のテナント分離 | poller をテナント独立にして混線ゼロにする | [sse-fan-in](sse-fan-in.md) |
| SSE・購読・hub | EXP-43 複数プロセス跨ぎ fan-out | pub/sub で topic=テナントに分けて跨ぎ配信する | [sse-fan-in](sse-fan-in.md) |
| SSE・購読・hub | EXP-52 hub キャッシュ破棄粒度 | 版で差分更新する。丸ごと捨てると 400x 無駄になる | [subscription-design](subscription-design.md) |
| SSE・購読・hub | EXP-53 長寿命接続の途中失権 | 定期 re-authorization で途中の権限剥奪に追随する | [subscription-design](subscription-design.md) |
| SSE・購読・hub | EXP-59 subscription 実メモリ | 1本あたりの実メモリを実測して容量見積りを裏取りする | [concern-subscription-capacity](concern-subscription-capacity.md) |
| キャッシュ | EXP-46 キャッシュ無効化 | TTL＋イベント失効＋stampede 対策。真実は DB、キャッシュは配信用 | [cache](cache.md) |
| テナント公平性・分離 | EXP-34 ノイジーネイバー | 1テナントが資源を独占しないよう公平性を担保する | [tenant-fairness](tenant-fairness.md) |
| テナント公平性・分離 | EXP-58 共有ワーカーのスコープ強制 | プロセス分離でなく、スコープ強制・最小権限・監査で越境を止める（cross-leak 実測0） | [tenant-scope](tenant-scope.md) |
| 容量・コスト見積り | EXP-31 1 vCPU/2GB の容量 | SSE 本数・Worker 処理量を実寸で見積もってから台数を決める | [capacity](capacity.md) |
| 容量・コスト見積り | EXP-50 型ごとのメモリ単価 | 型の単価から「2GB に何件」を計算する。整数型は打ち間違いがコンパイルエラーになる利点も | [reference-numbers](reference-numbers.md) |
| 容量・コスト見積り | EXP-51 ループ/alloc の正規化コスト | ns/op・allocs/op で正規化し、桁で判断する | [reference-numbers](reference-numbers.md) |
| 容量・コスト見積り | EXP-61 月予算の縦/横/Aurora 優先順位 | 律速(lever)を見てから縦積み/横積み/Aurora を選ぶ | [cost-scaling-priority](cost-scaling-priority.md) |
| バックアップ・engine | EXP-11 backup/restore/corruption | 復旧手順を実測で確認しておく（取れる≠戻せる） | [backup-restore](backup-restore.md) |
| バックアップ・engine | EXP-10 SQLite companion | 「MySQL がこうだから」で流用せず、engine ごとに測る | [sqlite](sqlite.md) |
| 可観測性 | EXP-37 メトリクスのカーディナリティ | ラベル爆発を避ける。received/changed/touched/skipped と変化率を出す | [observability](observability.md) |

---

## 大分類ごとの「一段上の結論」

- **状態同期・冪等・回収** … イベントは差分、状態は取りに行く。書き込みは版で冪等に。回収は時間＋CAS。
- **排他・担当決め** … lease＋fence。基準時刻は必ず DB。二重稼働は「普段は動く」ので仕組みで殺す。
- **ライフサイクル** … 落ちる/切れる/移行する前提。前進できる形と手順を先に決める。
- **接続・プール** … `sql.Open` は1つ、プール3設定は必須、予算は掛け算で上限を守る。
- **テーブル/列/主キー設計** … 状態で割らずキー設計と索引と hot/cold で解く。掃除は DROP PARTITION。
- **SSE・購読・hub** … 上流1本を hub で多重配信し、テナントで分離、版で差分。
- **容量・コスト** … 絶対値でなく桁で見積もり、律速を見てから lever を選ぶ。

**全体を貫く一行**: 状態は取りに行き（同期）、書き込みは版で冪等に、排他は lease＋fence、時刻は DB 基準、負荷は「書かない>まとめる>分ける>DB に来させない」。絶対値でなく桁で判断する。
