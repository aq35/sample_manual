# Web と Worker のレースコンディション対策（状態 CAS × version）（EXP-66）

- 実験: `internal/racelab` / `MYSQL_DSN=... go test ./internal/racelab/ -run TestEXP66 -v`
- スキーマ: `internal/racelab/schema.sql`
- 根拠: [repository-layer](repository-layer.md) §2.3（更新消失・楽観ロック）/ EXP-2（[fencing](fencing.md)）/ EXP-64（[worker-state-time](worker-state-time.md)・回収の CAS）

## 何が起きるか

Web（人が操作）と Worker（裏で処理）は**同じ行を同時に触る**。`SELECT` して Go で判断して `UPDATE`、
の「読取り→書込みの隙間」で、もう一方の書き込みが挟まると事故になる。代表は3つ。

| レース | 事故 |
| --- | --- |
| **A 二重 claim** | 複数 worker（や Web の再実行）が同じ pending を掴む → 二重処理 |
| **B cancel 中の complete** | Web が cancel した仕事を worker が完了で上書き → **cancel が消える** |
| **C 入力の途中編集** | worker が読んだ後に Web が入力を編集 → worker が**古い入力の結果**を書く（stale result） |

## こうあるべき

### 原則: 「id だけで UPDATE」を禁止し、遷移元を WHERE に入れる（CAS）

状態を進める書き込みは、**遷移元の状態（や owner・version）を必ず `WHERE` に入れ**、
`affected_rows = 1` で「自分が勝った」ことを確認する。0 なら誰かが先に変えた＝**結果を捨てて読み直す**。

| レース | 悪い（id だけ） | 良い（CAS / version） |
| --- | --- | --- |
| A claim | `SELECT` → `UPDATE ... WHERE id`（隙間で二重取得） | `UPDATE ... SET status='in_progress' WHERE id=? AND status='pending'`。`affected_rows=1` で claim 確定 |
| B complete | `UPDATE ... SET status='completed' WHERE id` | `... WHERE id=? AND status='in_progress' AND owner=?`。cancel 済みには一致せず破棄 |
| C 結果書込 | `UPDATE ... SET result=? WHERE id` | `... WHERE id=? AND version=?`（読んだ version）。編集後は不一致で破棄・読み直し |

- **状態遷移（A・B）は status の CAS**。claim も complete も回収（EXP-64）も、同じ「遷移元を WHERE に」パターン。
- **入力の同時編集（C）は version（楽観ロック）**。worker は読んだ version を持って書き、Web の編集は
  `version = version + 1` で表す。repository-layer §2.3 の「読んで書くなら version で守る」と同じ。
- **どちらも `affected_rows` を見る**のが肝。見ずに「UPDATE したから成功」と思い込むと事故に気づけない。

### 役割で「誰がどの遷移を書いてよいか」を決める

- **Worker が書いてよいのは処理の遷移**（pending→in_progress→completed/failed）だけ。
- **Web が書いてよいのはユーザ操作の遷移**（→cancelled、入力の編集）だけ。
- 両者が同じ列を奪い合わないよう分ける。奪い合う遷移（complete と cancel）は**状態 CAS が調停役**になる。

### 別解と注意

- claim は **`SELECT ... FOR UPDATE SKIP LOCKED`** でも守れる（悲観ロック）。本実験は楽観 CAS を測った。
  キューの取り合いが激しく空振り `SELECT` を減らしたいときは SKIP LOCKED が有利なことがある。
- **楽観ロックはタダではない**。競合が激しいとやり直しが増える（repository-layer §2.3 は 200 更新に
  1,122 回のやり直しを実測）。激しいなら設計（行の持ち方・粒度）を見直す。

## 仮説（未検証）

> EXP-66 は「CAS/version があれば二重0・cancel 消失0・stale0／無ければ事故が起きる」を測る。
> 以下は未計測の設計仮説。

- **H1: CAS の追加コストは小さい。** `WHERE` に列を1つ足すだけで、主キーで引く行に対する評価。
  claim のスループットは無条件版と大差ない見込み（空振り UPDATE ぶんだけ増える）。
- **H2: version 衝突率は「Web 編集頻度 × worker 処理時間」で決まる。** 処理が長いほど途中編集に
  当たりやすい。長い処理は version を**書き込み直前に取り直す**か、処理を短く分ける。
- **H3: SKIP LOCKED vs 楽観 CAS の分岐点は claim 競合の激しさ。** worker 台数が少なく空振りが
  少ないうちは CAS が単純で有利、台数が増えると SKIP LOCKED が空振りを減らす、と予想（要実測）。

## 一行でまとめると

**Web と Worker が同じ行を触るなら「id だけで UPDATE」は禁止。状態遷移は遷移元を WHERE に入れた CAS、入力の同時編集は version（楽観ロック）で守り、`affected_rows` で勝敗を確認して負けたら読み直す。**
