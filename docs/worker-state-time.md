# 「欲しい状態」は時間指定が要るのか要らないのか（Worker クエリの切り分け）

- 関連実験: `internal/statuslab`（EXP-22・状態で表を分けるか / **EXP-64・回収は時間指定と CAS で書く**）/ `internal/lease`・`internal/assignlab`（EXP-63・lease）/ `internal/cadencelab`（EXP-12・命令キュー）/ `internal/doorbelllab`（EXP-62・crash 回収）
- 根拠: `調査` §2.2（3本のループ）/ §2.7（冪等）/ §2.8（担当決め）/ §4.4（表を分ける）/ §4.6（InnoDB 固有）

Worker が状態機械（`pending → in_progress → completed`）を DB で回すとき、
クエリを書く前に **「その状態そのものが合図なのか、状態になってから一定時間が経って初めて合図なのか」** を先に決める。
この一点が、**timestamp 列を持つか**・**索引の末尾を id にするか時刻にするか**・**冪等条件に時刻が要るか**を決める。

---

## 1. 「欲しい状態」は2種類ある

| | 合図 | 形 | 索引の末尾 | 時刻列 |
| --- | --- | --- | --- | --- |
| **① 時間関係なく欲しい** | 状態の**存在そのもの** | `WHERE status=? ORDER BY id LIMIT n` | `id` | 要らない |
| **② 時間指定が要る** | 状態に**なってからの経過時間** | `WHERE status=? AND <時刻> <op> NOW()` | 時刻列 | 必須 |

- ① は「その状態の行が在ること」自体が処理の合図。`internal/statuslab` の `FindPending`
  （`WHERE tenant_id=? AND status=0 ORDER BY id LIMIT ?`）がこれ。索引は `(tenant_id, status, id)`。
- ② は「状態」だけでは行動を決められず、**"状態 + 経過時間" で初めて合図になる**。
  `internal/lease` の `expires_at <= NOW(3)`、`internal/assignlab` の `lease_expires < NOW(3)` /
  `last_heartbeat > NOW(3) - INTERVAL ? SECOND` がこれ。索引の末尾が `id` ではなく時刻列になる。

**基準時刻は DB の `NOW(3)` に寄せる。** ② の判定を worker 側の `time.Now()` でやると、
コンテナ間のクロックスキューで「まだ生きている担当」を失効と誤判定して回収してしまう
（`調査` §4.6・`internal/lease` が `NOW(3)` を徹底しているのはこの理由）。

---

## 2. `pending → in_progress → completed` に当てはめる

| エッジ / クエリ | 種類 | 典型クエリ | 索引 |
| --- | --- | --- | --- |
| **pending → in_progress**（取得 / claim） | **①時間なし** | `WHERE status='pending' ORDER BY id LIMIT n` | `(tenant, status, id)` |
| └ 遅延実行・バックオフを足したら | **②時間あり** | `... AND run_after <= NOW(3)` | `(tenant, status, run_after)` |
| **in_progress → completed**（完了） | —（クエリで拾う状態ではない） | 処理した worker 自身が **CAS UPDATE** で落とす | — |
| **in_progress → pending**（回収 / reclaim） | **②時間あり（必須）** | `WHERE status='in_progress' AND heartbeat_at < NOW(3) - INTERVAL ? SECOND` | `(tenant, status, heartbeat_at)` |
| **completed の掃除**（retention） | **②時間あり** | `WHERE status='completed' AND finished_at < NOW(3) - INTERVAL ? DAY` | 日付 RANGE パーティション |

`internal/cadencelab`（EXP-12）の命令キューは、①（`state='pending'`）に
②のスケジュール（`scheduled_for<=?`）を足した合成型で、索引 `(tenant_id, state, scheduled_for)` を張っている。

---

## 3. いちばん危ないのは「回収」を①で書くこと

`in_progress → pending` の回収を、時間を見ずに `WHERE status='in_progress'` だけで拾うと、
**今まさに健全に処理している worker の行を奪って二重実行になる。**
状態だけでは「処理中」と「担当が落ちて放置」を区別できないからだ。
その区別をつけるのが時間（heartbeat / lease TTL）で、
`調査` §2.5「沈黙は情報ではない」・EXP-62「crash 回収はイベントでなく lease 失効の reconcile」と同じ話。

- **①で済むのに②にする**（不要な時刻列と索引を足す）→ 使わない列の維持コストだけ払う。害は小さい。
- **②が要るのに①で書く**（回収を無条件でやる）→ **二重実行**。害が最大。ここを間違えない。

回収そのものも `status='in_progress' AND heartbeat_at < NOW(3) - lease` を条件にした
**CAS UPDATE**（`UPDATE ... SET status='pending' WHERE <条件>`）で行い、
`affected_rows` を見て「本当に自分が奪えたか」を確認する（`調査` §2.7）。

---

## 4. どうあると良いか（仮説・未検証）

> 以下の索引・スキーマ推奨は設計上の仮説で、**このリポジトリではまだ計測していない**。
> ①側（`FindPending`）の索引効果は EXP-22 で実測済み。②側の索引末尾を時刻にした効果は未計測。

**仮説 H1: ジョブ表は「状態列 + 用途別の時刻列」を持ち、索引は用途ごとに分ける。**

```sql
CREATE TABLE job (
  tenant_id   VARCHAR(32) NOT NULL,
  id          BIGINT      NOT NULL,
  status      TINYINT UNSIGNED NOT NULL,  -- 0=pending 1=in_progress 2=completed ...
  run_after   DATETIME(3) NULL,           -- ② 遅延実行の due（無ければ即時）
  heartbeat_at DATETIME(3) NULL,          -- ② in_progress の生存印（回収の基準）
  finished_at DATETIME(3) NULL,           -- ② 掃除の基準
  PRIMARY KEY (tenant_id, id),
  KEY idx_claim   (tenant_id, status, run_after),    -- ① / 遅延ありの取得
  KEY idx_reclaim (tenant_id, status, heartbeat_at)  -- ② 回収
) ENGINE=InnoDB;
```

- **時刻列は「同じ status に対する別々の問い」ごとに分ける。** 「取得の due」と「回収の期限」を
  1本の `updated_at` で兼ねると、片方の索引がもう片方の並びに使えず、どちらかが全走査になる。
- **`run_after` が不要な設計（遅延ゼロ）なら列ごと持たない。** ①のままでよく、索引は `(tenant, status, id)`。
  「将来使うかも」で時刻列と索引を先に足さない（`調査` §4.5「索引は必要になってから」）。

**仮説 H2: `completed` は同じ表に溜めず、掃除は時間で切る。**
終端行が溜まると、状態で絞る①のクエリでも索引の葉が太り、`COUNT`/`GROUP BY` は全走査になる（EXP-22 の動機）。
掃除は `DELETE` ではなく **`finished_at` の日付 RANGE パーティション + `DROP PARTITION`**（`調査` §4.6・実測でパーティション DROP が DELETE より約15倍速い）。

**仮説 H3: ②の判定に使う時刻の「基準」と「粒度」を1箇所で決める。**
- 基準は常に **DB の `NOW(3)`**（クロックスキュー回避、§1）。
- lease/heartbeat の TTL は「何秒気づかなくて許されるか」という**業務要件**から決める
  （`調査` §2.5・技術判断ではない）。送信元の時刻が秒粒度しかないなら同一秒内の変化が消えるので連番を足す（§2.8）。

---

## 5. 検証

**(3) 二重実行の再現は EXP-64 で実装済み**（`internal/statuslab/exp64.go` / `exp64_test.go`）。
`MYSQL_DSN=... go test ./internal/statuslab/ -run TestEXP64 -v` で走る。

- 生存担当（heartbeat 新しい）500 + 落ちた担当（heartbeat 古い）500 + completed 10万 を入れ、
  回収①（`WHERE status='in_progress'` の全 UPDATE）と
  回収②（`... AND heartbeat_at < NOW(3) - INTERVAL ? SECOND` の CAS）を before/after で比較。
- 検証: ①は生存担当を奪う（`stolen_alive > 0` ＝二重実行）／②は奪取 0・`affected_rows` が stale 件ちょうど。
- あわせて回収候補の抽出を索引 `(tenant,status,heartbeat_at)` あり／なしで比較（**(1) の索引効果**も同実験に同梱）。

> このリポジトリの環境では MySQL に接続できないため**まだ実行して数字は取れていない**（`MYSQL_DSN` 未設定で skip する CI 安全形）。
> `scripts/mysql-up.sh` で MySQL を立ててから走らせると、上の検証（`t.Errorf`）が実測で確定する。

**未実施:**

2. **兼用列 vs 分離列**: `run_after` と `heartbeat_at` を1本の `updated_at` に兼ねた場合に、
   取得と回収のどちらが全走査に落ちるかを EXPLAIN で確認（H1 の裏付け）。

---

## 一行でまとめると

**Worker のクエリは「状態が現れた瞬間が合図（①）」か「状態になってから経過時間が合図（②）」かを先に決める。②の基準は必ず DB の `NOW(3)`。回収を①で書くと二重実行になる。**
