# Go + MySQL リポジトリ層 — 実験して決めた設計

マルチテナントのアプリで、Go から MySQL を触る層をどう作るか。
**「気をつける」で守れないものを、仕組みで守る**ことを目的にしている。

すべての判断に実験がついている。再現:

```bash
eval "$(./scripts/mysql-up.sh --export)"
go test ./internal/repo/ -run TestExperiment_ -v    # 根拠になった実験
go test ./internal/repo/... -v                      # 層が事故を止めることの確認
```

測定環境は `docs/measurements.md` と同じ（Go 1.24.7 / MySQL 8.0.46 / 4 CPU / 同一ホスト）。

---

## 0. 使い方

```go
db, err := repo.Open(dsn, repo.Options{})   // プロセスに1つ
defer db.Close()

sc := db.Tenant(tenantID)                   // テナントに束縛したハンドル
p, err := profiles.Get(ctx, sc, robotID)    // リポジトリは *Scope しか受け取らない
```

リポジトリの実装はこう書く。**`tenant_id = :tenant` が SQL の唯一の作法**で、
値はアプリが渡さない（渡し忘れようがない）。

```go
func (ProfileRepo) Get(ctx context.Context, s *repo.Scope, robotID string) (Profile, error) {
    const q = `SELECT robot_id, name, version FROM robot_profile
                WHERE tenant_id = :tenant AND robot_id = ?`
    ...
}

func (ProfileRepo) Delete(ctx context.Context, s *repo.Scope, robotID string) error {
    const q = `DELETE FROM robot_profile WHERE tenant_id = :tenant AND robot_id = ?`
    _, err := s.Exec(ctx, "profile.Delete", q, repo.ExpectOne, robotID)  // ★1行のはず
    return err
}
```

---

## 1. 結論（守ること）

| | 守ること | 根拠（実験） | 仕組みでの担保 |
| --- | --- | --- | --- |
| 1 | **テナントの値をアプリが書かない** | tenant_id 忘れの UPDATE が他テナントの行を書き換えた | `:tenant` が無い SQL は実行しない |
| 2 | **更新は「何行に当たるか」を宣言する** | 1行のつもりの UPDATE が 5行に当たった。エラーは出ない | `ExpectOne` 等。違えばロールバック |
| 3 | **読んで書くなら version で守る** | 200回の read-modify-write のうち **175回が消えた** | `version = version + 1` と `AND version = ?` |
| 4 | **DB 側で計算できるなら読まない** | `balance = balance + 1` なら衝突ゼロで正しい | — |
| 4.5 | **適用済みマイグレーションを書き換えない** | 環境ごとに表の形がずれる | チェックサム照合で起動を止める |
| 5 | **主キーは (tenant_id, ...)** | テナント単位の読み出しが **2.6倍** 速い | schema.sql とレビュー |
| 6 | **ランダムな id を主キーにしない** | 表サイズ **2.0倍**、投入 **1.5倍** 遅い | 同上 |
| 7 | **ページ送りは OFFSET を使わない** | 4,000件目で **2.19ms vs 0.36ms** | `repo.Keyset` / `Paginate` |
| 8 | **N+1 を書かない** | 500件で **40〜64倍** 遅い | `GetMany` と `repotest.RequireQueries` |
| 9 | **上限の無い SELECT を書かない** | — | `LIMIT` が無ければ実行しない（明示すれば通る） |
| 10 | **実行計画をテストで固定する** | 索引が増えると計画が変わる（下記 4.6） | `repotest.RequireIndexed` |
| 11 | **デッドロックは前提。やり直せる形にする** | すれ違う更新で実際に発生 | `Scope.Tx` が自動でやり直す |
| 12 | **逃げ道は塞がず、目に見えるようにする** | — | `Unscoped(理由)` / `AllowUnbounded()` は記録される |

---

## 2. 事故を防ぐ

### 2.1 テナント指定を忘れる

**実験**（`TestExperiment_テナント指定を忘れる`）

```sql
-- 「自分のテナントの id=1」のつもりで tenant_id を書き忘れた
UPDATE exp_acct SET status=9 WHERE id=1;
```

→ **2行に当たり、他テナント(t-b)の行まで書き換わった。** エラーは出ない。

**対策** — SQL を出せるのは「テナントに束縛したハンドル」からだけにし、
テナントの値はアプリに渡させない。SQL には `:tenant` と書く。

```go
sc := db.Tenant("t-alpha")
sc.Exec(ctx, "op", "UPDATE robot_profile SET name=? WHERE robot_id=?", repo.ExpectOne, ...)
// → ErrMissingTenant: ":tenant" を書くこと
```

止まるもの（`TestScope_テナント指定の無いSQLは実行しない`）

| 書き方 | 結果 |
| --- | --- |
| `WHERE robot_id = ?`（tenant_id 無し） | `ErrMissingTenant` |
| `SET name = :tenant WHERE robot_id = ?`（SET 側にだけある） | `ErrMissingTenant`（WHERE 句を見る） |
| `UPDATE ... `（WHERE 無し） | `ErrUnsafeStatement` |
| `SELECT ...`（LIMIT 無し） | `ErrTooManyRows` |
| 文字列リテラルやコメントに `:tenant` と書いた | 認めない（正規化してから判定する） |

**この検査は文字列を見て判断している**ので万能ではない。狙いは「うっかり」を止めることで、
悪意を止めることではない。テナント分離の最終的な保証は、この層を通す運用と、下の 2.4。

### 2.2 WHERE の書き間違い

**実験**（`TestExperiment_WHEREの書き間違い`）— 1行のつもりの UPDATE が **5行**に当たった。

**対策** — 影響行数を宣言させ、違ったらロールバックする。

```go
n, err := sc.Exec(ctx, "profile.rename", q, repo.ExpectOne, args...)
// → ErrUnexpectedRowCount: ちょうど1行 のはずが 5 行
```

トランザクションの外から呼んだ場合も、暗黙のトランザクションで包んで巻き戻す
（`TestScope_影響行数が想定と違えばロールバックする` で、5行とも元のままであることを確認）。

宣言は3種類だけにしてある。`ExpectOne` / `ExpectAtMostOne` / `ExpectAtMost(n)`。
`ExpectAny`（検査しない）は、バッチのように行数が読めないときだけ。

### 2.3 更新が消える（lost update）

**実験**（`TestExperiment_更新が消える`）— 8並列 × 25回の read-modify-write:

| やり方 | 結果 |
| --- | --- |
| 読んで、+1 して、書く | 期待 300 → **実際 125**（175回ぶんが消えた。エラーは出ない） |
| `version` を条件に入れる（楽観ロック） | 300（衝突 1,122回はやり直し） |
| `balance = balance + 1`（読まない） | 300、所要 184ms |

**対策の優先順位**

1. **そもそも読まない。** DB 側で計算できるなら `SET x = x + ?` にする。衝突しない
2. 読まないと決められない場合だけ `version`。`ErrOptimisticLock` を返し、呼び出し側は読み直してやり直す
3. `SELECT ... FOR UPDATE` は最後の手段。詰まるうえ、デッドロックの主因になる

> 楽観ロックはタダではない。上の実験では 200回の更新に対して 1,122回のやり直しが起きた。
> **競合が激しい行に対しては、そもそも設計が違う**（加算にする、キューに逃がす）。

### 2.4 MySQL 側の安全装置（sql_safe_updates）

**実験**（`TestExperiment_safe_updatesは何を止めるか`）。
`repo.Open` は DSN に `sql_safe_updates=1` を必ず付ける
（go-sql-driver は未知の DSN パラメータをシステム変数として送るので、これだけで有効になる）。

| SQL | 結果 |
| --- | --- |
| `UPDATE t SET x=1`（WHERE 無し） | **拒否**（1175） |
| `UPDATE t SET x=1 WHERE status=1`（キー列でない） | **拒否** |
| `UPDATE t SET x=1 WHERE status=1 LIMIT 100` | **通る**（LIMIT があれば通ってしまう） |
| `UPDATE t SET x=1 WHERE tenant_id='t-a'`（主キー先頭列） | 通る |
| `UPDATE t SET x=1 WHERE id=1`（主キーの後半だけ） | **拒否** |

**アプリの検査をすり抜けた分の最後の網**として有効。ただし LIMIT で抜けられるので、
**テナント分離をこれに頼ってはいけない。**

### 2.5 デッドロックは前提にする

`Scope.Tx` はデッドロック（1213）とロック待ちタイムアウト（1205）を自動でやり直す
（`TestTx_デッドロックは自動でやり直す` で、すれ違う順序の更新が両方成功することを確認）。

**やり直せるのは、トランザクションの中で外部 API を呼んでいないから。**
呼んでいると、やり直しが二重送信になる。`MaxTxDuration`（既定 1秒）を超えたトランザクションは
警告として数える（`Stats.LongTxs`）ので、混入に気づける。

入れ子の `Tx` は新しいトランザクションを開かず、外側に参加する
（`TestTx_入れ子は同じトランザクションになる`）。リポジトリを組み合わせたときに、
知らないうちに2重コミットになるのを防ぐため。

---

## 3. 性能を崩しにくくする

### 3.1 主キーは (tenant_id, ...)

**実験**（`TestExperiment_主キー設計`）— 20テナント × 5,000行 = 10万行。

| 設計 | テナント1件ぶん(5,000行)の読み出し | 表サイズ |
| --- | --- | --- |
| `PRIMARY KEY (tenant_id, id)` | **3.8ms** | 16.5 MB |
| `PRIMARY KEY (id)` + `KEY (tenant_id)` | **9.9ms（2.6倍）** | 16.0 MB |

InnoDB は主キー順に行を物理配置する。テナント先頭の複合主キーなら、
同じテナントの行が隣接ページに並ぶ。二次索引だと「索引で見つけて、主キーを引き直す」が
1行ごとに起きる。

**差が出るのは列を取りに行くとき。** 索引だけで足りる問い合わせ（covering index）なら差は出ない。

### 3.2 ランダムな id を主キーにしない

**実験**（`TestExperiment_ランダムな主キー`）— 10万行。

| 主キー | 投入時間 | 表サイズ |
| --- | --- | --- |
| 連番 BIGINT | 2.05s | **13.5 MB** |
| ランダム CHAR(36)（UUIDv4 相当） | **2.99s（1.5倍）** | **26.6 MB（2.0倍）** |

挿入位置がばらけるとページ分割が起きて、ページが埋まりきらないまま増える。
外部に見せる ID が UUID である必要があるなら、**主キーは別に持ち、UUID には UNIQUE 索引**を張る
（あるいは時刻順に並ぶ UUIDv7 を使う）。

### 3.3 ページ送りは OFFSET を使わない

**実験**（`TestExperiment_ページ送り`）— 1テナント 5,000行、1ページ50件。

| 位置 | OFFSET | キーセット |
| --- | --- | --- |
| 先頭 | 447µs | 463µs |
| 1,000件目 | 781µs | 497µs |
| 4,000件目 | **2.19ms** | **363µs** |

OFFSET は捨てる行も読むので、深くなるほど比例して遅くなる（計画も `rows=8570` のまま）。
キーセット法は `id > ?` の range になり、**深さに依らない**。

```go
page, err := profiles.List(ctx, sc, repo.Keyset{After: cursor, Limit: 50})
// page.Next を次の After に渡す。page.HasMore で終端が分かる
```

### 3.4 N+1

**実験**（`TestExperiment_N1問題`）— 500件を取る。

| やり方 | 時間 |
| --- | --- |
| 1件ずつ引く | **131.7ms** |
| `IN (...)` でまとめて | 3.3ms |
| 範囲で | 0.85ms |

**N+1 は「遅いクエリ」ではなく「速いクエリを N 回」なので、スロークエリログには出ない。**
テストで回数を縛るのが確実。

```go
repotest.RequireQueries(t, db, 1, func() {
    _, _ = profiles.GetMany(ctx, sc, ids)   // 1回で済むこと
})
```

### 3.5 取る列を絞る

**実験**（`TestExperiment_必要な列だけ取る`）— 5,000行の一覧で **1.4倍**の差。
一覧画面が本文まで持ってくる必要がないなら取らない。転送量とバッファプールの両方に効く。

### 3.6 実行計画をテストで固定する（落とし穴つき）

```go
repotest.RequireIndexed(t, sc, "profile.List", listSQL, "", 10)      // 全表走査・filesort を落とす
repotest.RequireRowsScanned(t, sc, 100, "profile.Get", getSQL, id)   // 舐めすぎも落とす
```

**★実験中に踏んだ落とし穴。** 同じ問い合わせでも、行数によって計画が変わる。

| テナントの行数 | 選ばれた索引 | Extra |
| --- | --- | --- |
| 50行 | `uq_tenant_serial`（別の索引） | **Using filesort** |
| 5,000行 | `PRIMARY`（range） | Using where |

つまり **数十件のテストデータで「固定」した計画は、本番の計画とは別物**。
実行計画のテストは、本番に近い件数を入れてから行うこと。

---

## 4. 運用保守しやすくする

### 4.1 エラーを型で返す

driver のエラー番号（1062 / 1213 / 1205 / 1175）を業務コードに漏らさない。

| 返るもの | 意味 | 呼び出し側 |
| --- | --- | --- |
| `ErrNotFound` | 無い | 404 |
| `ErrConflict` | 一意制約違反 | 409 |
| `ErrOptimisticLock` | 先に更新された | 読み直してやり直す |
| `ErrUnexpectedRowCount` | 影響行数が宣言と違う | **バグ。ロールバック済み** |
| `ErrMissingTenant` / `ErrUnsafeStatement` / `ErrTooManyRows` | 書き方の問題 | **バグ。実行されていない** |

`repo.Retryable(err)` が true なら、そのままやり直してよい。

### 4.2 数字を持つ

`db.Stats()` で以下が取れる。テストからも使う（3.4 の N+1 検出）。

```
Queries / Execs / Txs / Retries / SlowQueries / LongTxs / Blocked / CrossTenant / RowsAffected
```

- `Blocked` が増えている＝検査で止めた回数。**0 でないなら、そのコードは本番で事故る書き方をしている**
- `CrossTenant` が増えている＝テナントを跨ぐ操作。業務コードから出ていたら設計を疑う
- `LongTxs` が増えている＝トランザクションが長い。中で外部 API を呼んでいないか

遅い問い合わせ（既定 100ms 超）と長いトランザクションは、SQL 付きで警告ログに出る。

### 4.3 マイグレーションは、運用で起きることを前提に組む

`internal/repo/migrations/0001_....sql` を番号順に当てる。当てた記録は `schema_migrations` に残る。

| 運用で起きること | 対策 | 確認 |
| --- | --- | --- |
| **複数コンテナが同時に起動する** | `GET_LOCK` で直列化。1つだけが当て、他は待って何もしない（**なぜ行ロックでは駄目か**は [locking.md](locking.md)） | `TestMigrate_同時に起動しても壊れない`（8並列で適用は1回） |
| **適用済みの .sql を後から書き換える** | チェックサムを保存して照合し、**起動を止める** | `TestMigrate_適用済みの書き換えを検出する` |
| **途中で失敗する** | そこで止める。以降は当てない | 半端な状態のまま先へ進まない |

**適用済みのファイルは書き換えず、新しい番号を足す。**
書き換えは「開発環境では動く（作り直したから）／本番だけ表の形が違う」という、
いちばん静かに壊れる事故になる。

**MySQL の DDL は暗黙にコミットされる**ので、複数の DDL をまとめてロールバックはできない。
1ファイル1変更にしておくと、失敗したときにどこまで進んだかが分かる。
（この性質のせいで「ロック用の行を `FOR UPDATE`」方式が使えない。[locking.md](locking.md) 3.2）

**「同時に走らない」と「途中で落ちる」は別の問題。** 前者はロック、後者は
`started` → `done` の二相記録で受ける（[locking.md](locking.md) 6）。

**索引を足すマイグレーションを入れたら、実行計画のテストを回すこと**（3.6）。
実際、`(tenant_id, serial)` の UNIQUE 索引があるせいで、行数の少ない環境では
一覧の問い合わせがその索引 + filesort を選んだ。

### 4.4 逃げ道は塞がない。ただし目に見えるようにする

| 逃げ道 | 用途 | 代償 |
| --- | --- | --- |
| `sc.AllowUnbounded()` | 全件同期など、本当に全部要るとき | コードに書いてあるので grep できる |
| `db.Unscoped(理由)` | 移行・管理業務・全社集計 | 理由つきで警告ログ＋`Stats.CrossTenant` |
| `db.SQL()` | どうしても層の外に出たいとき | レビューで見つける |

**塞ぐと、層ごと迂回されて終わる。** 出口を1本にして、通ったことが分かるようにするほうが続く。

---

## 5. この層の代金

`go test ./internal/repo/ -bench 'PointRead|CheckAndBind' -benchmem`

| | 時間 | メモリ |
| --- | --- | --- |
| 検査＋`:tenant` 束縛（キャッシュあり） | **147 ns** | 64 B / 2 allocs |
| 同（キャッシュなし＝毎回 SQL を舐める） | 4,800 ns | 2,072 B / 14 allocs |
| 主キー1件読み: 素の `database/sql` | 234 〜 243 µs | 856 B |
| 主キー1件読み: リポジトリ層ごし | 242 〜 249 µs | 984 B |

**検査は DB への往復の 0.06%。** 実測して初めて分かったのは、
SQL 文ごとに検査結果を覚えておくかどうかで **33倍** 違うこと。
SQL 文はコード中の定数なので種類は有限で、キャッシュが効く。

---

## 6. この層でも防げないこと

正直に書いておく。

- **SQL の検査は文字列一致**。動的に組み立てた SQL や、副問い合わせの中の条件までは見ていない
- **`db.SQL()` を使えば全部迂回できる**。レビューで見るしかない
- **テナント分離の最終保証ではない**。本当に厳格にするなら、テナントごとに DB を分ける、
  あるいは MySQL 8.0 の行レベルの仕組みや VIEW ＋ 専用ユーザーを併用する
- **実行計画のテストは、データ量が本番に近くないと意味がない**（3.6）
- **楽観ロックは競合が激しいと現実的でない**（2.3）。その場合は設計を変える
