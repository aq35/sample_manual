# EXP-8 SQL 検査の fuzz — どこまで守れて、どこから守れないか

再現:

```bash
# 種と回帰入力だけ（CI 向け・数十ミリ秒）
go test ./internal/repo/ -run 'TestGuardProperties|TestEXP8'

# fuzz を回す
go test ./internal/repo/ -run FuzzCheckStatement -fuzz FuzzCheckStatement -fuzztime 90s
```

結果ファイル: [`docs/results/exp-8/20260903-151142-sql-guard-fuzz.md`](results/exp-8/20260903-151142-sql-guard-fuzz.md)

---

## 0. 前提

`internal/repo` の SQL 検査は **文字列を見ているだけ**で、パーサではない。
この実験の目的は「安全だと証明する」ことではなく、
**どこまでは守れて、どこから守れないのかを機械的に確かめる**こと。

## 1. 確かめる性質

fuzz でも通常テストでも、同じ性質を確認する。

```
① どんな入力でも panic しない
② :tenant を（文字列やコメントの外に）持たない UPDATE/DELETE は必ず弾く
③ 複数の文を1回で投げる形は通さない
④ 複数の表に触れる UPDATE/DELETE は通さない
⑤ 通した文は、束縛後に :tenant が残らない（引数の数も合う）
```

## 2. 見つかった抜け道

### 2.1 手で書いた種（34個）だけで 6 件

| 通ってしまった形 | なぜ危険か |
| --- | --- |
| `UPDATE ... WHERE tenant_id = :tenant AND id = ?; DROP TABLE u` | DSN に `multiStatements=true` が付いた瞬間に実行される |
| `SELECT 1 WHERE tenant_id = :tenant LIMIT 1; SELECT 2` | 同上 |
| `UPDATE a JOIN b ON a.id = b.id SET b.v = 1 WHERE a.tenant_id = :tenant` | `:tenant` は a にしか掛からない。**b は無条件に書き換わる** |
| `UPDATE a, b SET b.v = 1 WHERE a.tenant_id = :tenant AND a.id = b.id` | 同上 |
| `DELETE a FROM a JOIN b ON ... WHERE a.tenant_id = :tenant` | 同上 |
| `DELETE a, b FROM a JOIN b ON ... WHERE a.tenant_id = :tenant` | 同上 |

**「`:tenant` が書いてある」ことは、テナント境界の保証にならない。**

### 2.2 fuzz が追加で 2 件（手では思いつかなかった形）

| 入力 | 何が起きていたか | 発見までの時間 |
| --- | --- | --- |
| `:tenant":tenant0` | 引用符が閉じていないため、2つめの `:tenant` が束縛されずに driver へ渡っていた | 約 10 秒 |
| `:tenAnt':tenant'0` | 検査は小文字化して「トークンあり」と判定するのに、置換は大文字小文字を区別するため置換されない | 約 60 秒 |

どちらも **性質⑤（束縛後に `:tenant` が残らない）** の違反として出た。
「危険な形を列挙する」のではなく「壊れてはいけない性質」を書いておくと、
思いつかなかった形が自動的に出てくる。

## 3. 直したこと

- 引用符・ブロックコメントが閉じていない SQL を拒否する
- 1回の呼び出しに複数の文が入っている形を拒否する
- 複数の表に触れる `UPDATE` / `DELETE` を拒否する
- **`:tenant` の判定は大文字小文字を区別し、キーワードの判定は区別しない**、と分けた

修正後、**90秒・1,226,829 実行（約 10,000 exec/秒）で新たな違反は出ていない。**
見つかった入力は `internal/repo/testdata/fuzz/FuzzCheckStatement/` に残り、
以後は毎回のテストで回る。

## 4. 測定器自身が穴だった

検査を「`:tenant` は区別する／キーワードは区別しない」に分けたとき、
**性質テストのほうが小文字化を合わせていなかった。**
その状態では大文字の SQL（`UPDATE T SET ...`）が性質テストを素通りする。

fuzz は「実装の穴」だけでなく「テストの穴」も作りうる。
性質テストと実装が同じ前提を共有しているか、毎回確かめる必要がある。

## 5. 残っている既知の穴（直していない）

- **`INSERT ... SELECT` の元表がテナントで絞られているかは判定できない。**
  `INSERT INTO t (tenant_id, id) SELECT :tenant, id FROM u` は通るが、
  `u` 側にテナントの条件が無い
- 動的に組み立てた SQL（文字列連結）は、**組み立て後の形しか見ていない**
- MySQL の方言を網羅していない（パーサではないため）
- **security boundary ではない。** 悪意のある入力を止める仕組みとしては使えない。
  厳格なテナント分離が要るなら、DB を分ける・専用ユーザ＋VIEW を併用する
  （`docs/repository-layer.md` 6 と同じ結論）
