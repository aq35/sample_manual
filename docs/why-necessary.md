# なぜ必要になるか — 標準ワークロードのメモリ・ストレージ計算

各設計（hub・差分キャッシュ・batch・射影・保持期間…）が「なぜ必要か」を、**標準的な
ワークロードの数字**で示す。単価は [docs/reference-numbers.md](reference-numbers.md)
（[EXP-50](../internal/memlab)/[EXP-51](../internal/looplab)/[EXP-31](capacity.md)）から。
結論は各節末の **「素朴だと破綻 → だからこの設計」**。

---

## 標準ワークロードの定義（この計算の前提）

**標準ワーカー**（1行ごとの DB ポーリング・関連表4つ）:

| 項目 | 値 |
| --- | --- |
| テナント数 × 1テナントの台数 | 100 × 100 = **10,000 台** |
| 関連表 | `robot_state` / `robot_task` / `worker_lease` / `robot_state_history`（**4表**） |
| ポーリング間隔 | 1 秒 |
| 1台の状態遷移頻度 | 平均 10 秒に1回（＝まばら） |

**標準 Web**（典型的な Query / Subscription / Mutation）:

| 項目 | 値 |
| --- | --- |
| Query | 一覧 50 件/ページ（keyset） |
| Subscription | 同時 **5,000 接続**（TLS・状態＋タスクをライブ配信） |
| Mutation | 冪等コマンド **1,000 件/分** |
| タスク | 1 vCPU / 2GB（[EXP-31](capacity.md)） |

---

## 計算1: メモリ（接続と行）

**Subscription の接続メモリ**（単価は [EXP-50](reference-numbers.md)）:

```
goroutine だけ:   5,000 × 2KB   = 10MB     ← 余裕
TLS バッファ込み: 5,000 × 34KB  = 170MB    ← 2GB に収まる（EXP-31 の現実値）
```

接続数だけなら 2GB に載る。**問題はメモリではなく DB 接続**：

```
もし「SSE 1本 = DB 接続 1本」なら → 5,000 DB 接続
だが 1 vCPU タスクの DB プールは せいぜい 数十本（EXP-5 の膝）
→ 即枯渇・全体が待たされる
```

**一覧 Query のメモリ**（[EXP-21](column-projection.md)）:

```
narrow 50 行:            50 × ~48B      ≒ 2KB      ← 無視できる
SELECT * で 24KB の列も取る: 50 × 24KB   = 1.2MB/req
同時 100 リクエスト:      100 × 1.2MB    = 120MB    ← 太い列 × 同時数で膨らむ
```

> **素朴だと破綻**: SSE に DB 接続を 1:1 で持たせると 5,000 接続で DB が枯れる。
> `SELECT *` で太い列を取ると同時数ぶんメモリが膨らむ。
> **だから** → push は**共有 hub**（[EXP-38](sse-fan-in.md)）、DB は小さい共有プール。
> Query は**必要列だけ射影**（[EXP-21](column-projection.md)）。

## 計算2: DB 往復（ポーリング）

**素朴な per-row ポーリング**（1台ずつ・関連4表を join）:

```
10,000 台 × 4 表 ÷ 1 秒 = 40,000 クエリ/秒
本番 DB 往復: RTT 0.5ms・プール16 → ~32,000/秒 が上限（EXP-31 の外挿）
→ 40,000 > 32,000 = 飽和。ワーカーが追いつかない
```

**テナント単位に畳む**（[EXP-14](fanout.md)/[EXP-12](scheduling.md)）:

```
100 テナント × 1 クエリ ÷ 1 秒 = 100 クエリ/秒   ← 400x 削減
（1クエリで自テナントの変更分をまとめて取る＝batch）
```

**N+1**（一覧＋関連を1件ずつ・[EXP-23](graphql.md)）:

```
素朴: 50 + 50×4 = 250 クエリ/リクエスト
DataLoader: 1 + 4 = 5 クエリ/リクエスト   ← 50x 削減
```

> **素朴だと破綻**: 1行ごとのポーリングは 40,000 クエリ/秒で DB を飽和させる。N+1 は1リクエストで
> 250 往復。**だから** → **テナント単位に coalesce**（[EXP-14](fanout.md)）＋**batch ポーリング**
> （[EXP-12](scheduling.md)）＋**DataLoader**（[EXP-23](graphql.md)）。合計接続は
> [poolbudget](../internal/poolbudget) で天井確認（[EXP-30](redundancy.md)）。

## 計算3: ストレージ（履歴・冪等・outbox）

**状態履歴**（`robot_state_history`）:

```
1台の遷移: 86,400 秒/日 ÷ 10 秒 = 8,640 回/日
全体:      10,000 台 × 8,640    = 86.4M 行/日
行幅 ~100B → 8.6GB/日 → 30日で 260GB   ← 無制限に増え続ける
```

**冪等表**（`uq_idem`・[EXP-27](graphql.md)）:

```
1,000 件/分 × 60 × 24 = 1.44M 行/日   ← これも増え続ける
```

**保持期間の適用コスト**（同量を消す・[EXP-48](retention.md) 実測）:

```
DROP PARTITION: 20ms（古い日を丸ごと外す・undo 肥大なし）
DELETE 同量:    47ms＋undo 肥大・purge 遅延   ← 大量だと更に悪化
```

> **素朴だと破綻**: 履歴も冪等表も**放置すれば無制限**（履歴は 8.6GB/日）。DELETE で消すと
> undo が肥大して重い。**だから** → **日パーティション＋保持期間で DROP**（[EXP-48](retention.md)）、
> 冪等表・送信済み outbox も保持期間で刈る（[EXP-44](outbox.md)）。

## 計算4: hub のキャッシュ再読（複数ロボット）

**テナント丸ごと再読**（hub の snapshot を毎 poll 作り直す・[EXP-52](subscription-design.md)）:

```
hub poll 1秒ごと: 100 テナント × 100 台 = 10,000 行/秒 読む
（実際に変わったのは数台でも、毎回 全台を舐める）
```

**版(ver)で差分だけ**（[EXP-52](subscription-design.md) 実測: 400x）:

```
変わった台数だけ = 数十行/秒   ← まばらな変更では 数百倍少ない
```

> **素朴だと破綻**: 「1台変わったら全台読み直す」は台数に比例した無駄（10,000 行/秒）。
> **だから** → **版で差分だけ**引く（[EXP-52](subscription-design.md)）。破棄の合図は粗く、再取得は細かく。

---

## まとめ：計算 → 破綻 → 必要な設計 → 実験

| 計算（標準ワークロード） | 素朴だと | 必要な設計 | 実験 |
| --- | --- | --- | --- |
| SSE 5,000 接続 × DB 接続 1:1 | DB プール（数十本）が枯渇 | 共有 hub・DB は小プール | [EXP-38](sse-fan-in.md) |
| SSE 5,000 × 34KB | 170MB（可） / 1:1 だと破綻 | hub＋接続上限 1万〜1.5万本 | [EXP-40](capacity.md) |
| 一覧 `SELECT *` 太い列 50 行 × 同時100 | 120MB／舐めも重い | 射影・別表 or off-page | [EXP-21](column-projection.md) |
| per-row polling 40,000 クエリ/秒 | DB 飽和（>32k/s） | テナント coalesce＋batch | [EXP-14](fanout.md)/[EXP-12](scheduling.md) |
| N+1 250 クエリ/req | 往復爆発 | DataLoader（5 クエリ） | [EXP-23](graphql.md) |
| 履歴 8.6GB/日・冪等 1.44M/日 | ストレージ無制限 | 保持期間＋パーティション DROP | [EXP-48](retention.md) |
| hub 丸ごと再読 10,000 行/秒 | 台数に比例した無駄 | 版で差分だけ（400x 減） | [EXP-52](subscription-design.md) |
| 長寿命接続の失権 595 件漏洩 | 剥奪後も配信 | 定期 re-auth＋最大寿命 | [EXP-53](subscription-design.md) |
| 何でも retry（恒久エラー5回叩く） | DB を無駄に叩く | エラー分類し fail-fast | [EXP-49](retry.md) |

- **共通の型**: 「1件の単価 × 件数」または「1回の往復 × 頻度」が、メモリ予算・DB 往復上限・
  ストレージのどれかを超える瞬間に、その設計が**必要になる**。数字で超える点が見えれば、
  対策（横に割る・畳む・差分・射影・保持）が選べる。
- 数字は当たり付け。最後は必ず **1 vCPU/2GB 実機**で p95・rps・RSS を測る（[EXP-31](capacity.md)）。

## 保証しない範囲・未検証

- 台数・頻度・行幅は「標準」の仮定。実際のワークロードで置き換えて計算する（式は [reference-numbers](reference-numbers.md)）。
- DB 往復上限は同ホスト実測の外挿（本番 RTT 次第）。
- ストレージ量は行幅 100B の目安（索引・可変長で上下）。
