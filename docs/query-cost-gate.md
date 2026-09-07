# クエリコストゲート（実行前に走査見込みで弾く）

外から来る検索は、条件次第で索引が効かず全表を舐める。件数が増えれば一発で
DB を圧迫する（DDoS 的な問い合わせ）。`ErrTooManyRows`（`MaxRows`）は「**返す**
行数」の天井だが、返す前に**走査**で潰されることがある。ここでは「**走査**する
行数」に天井をかけ、重いクエリを**実行する前に**弾く。

根拠は [EXP-18](date-search.md)：重さは総行数ではなく「走査した行数・返した行数」
で決まる。だから走査見込み（EXPLAIN の見積もり）にゲートを置く。

## 使い方

```go
db, _ := repo.Open(dsn, repo.Options{MaxScanRows: 50000}) // 既定 50000
sc := db.Scope(ctx, tenantID)

// EXPLAIN してから Query する。走査見込みが上限を超えたら ErrTooCostly。
rows, err := sc.GuardedQuery(ctx, "profile.search", 5000, query, args...)
if errors.Is(err, repo.ErrTooCostly) {
    // 索引で絞れていない検索。400 で返す（500 ではない・こちらは無実）
}
```

`CheckCost` だけ呼んで判定に使うこともできる。EXPLAIN のぶん往復が 1 回増えるので、
ホットパスではなく「外から来る検索」に使う。

## 何を弾いて、何を通すか

EXPLAIN の `rows` は「範囲の見積もり」であって、実際に読む行数とは限らない。
keyset 一覧（`WHERE id > ? ORDER BY id LIMIT 50`）は見積もりが大きくても LIMIT で
止まる。だから見積もりが大きいだけで弾くと、健全なページングまで誤爆する。
弾くのは、見積もりがそのまま読まれる次の場合に限る：

| 条件 | 判定 | 理由 |
| --- | --- | --- |
| 全表走査（`type=ALL`） | **拒否** | 索引が全く効いていない |
| `Using filesort` / `Using temporary` かつ 見積もり > 上限 | **拒否** | 範囲全体を materialize してから並べ替え・LIMIT |
| `LIMIT` が無く 見積もり > 上限 | **拒否** | 索引で止められない |
| keyset ページング（索引順・LIMIT あり）で見積もりが大きい | 通す | LIMIT で早く止まる |

`ErrTooCostly` は「無実の 500」ではなく「クライアントの検索条件が広すぎる」ことを
指す。索引で絞る・範囲を狭める・キーセットにする、で通るようになる。

## 適用範囲・保証しない範囲

- MySQL 8.0。EXPLAIN の見積もりはヒストグラム・統計に依存する（`ANALYZE TABLE`
  が古いとズレる）。ゲートは「明らかに舐めるもの」を止める安全弁で、精密な原価計算
  ではない。
- 上限は用途で変える（一覧検索は小さく、バッチ集計は大きく）。`GuardedQuery` の
  第 3 引数 `maxRows` が 0 なら `Options.MaxScanRows`（既定 50000）。
- テスト: `internal/repo/costgate_test.go`（keyset LIMIT は通る／`LIKE '%...%'`
  の無界検索は `ErrTooCostly`）。
