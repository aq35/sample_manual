# 無停止スキーマ変更（expand / contract）（EXP-32）

ローリングデプロイ中は、**旧アプリ(v1)と新アプリ(v2)が同じ DB を同時に触る**時間が必ずある。
このとき「列を一気に張り替える」と、その瞬間から旧アプリが壊れる。足す→両対応→切替→消す
（expand/contract）にすれば、どの瞬間も両バージョンが動く。列 `status` を `state` に改名する例。

実装は [internal/deploylab](../internal/deploylab)、receipt は
[docs/results/exp-32](results/exp-32/exp-32-expand-contract.md)。

## 何が壊れるか（実測）

| やり方 | 結果 |
| --- | --- |
| 一気に `ALTER TABLE ... CHANGE status state`（改名1発） | 改名直後から **v1 の `SELECT status` が即エラー**（Unknown column） |
| expand（`state` を足して backfill） | v1 は `status` のまま**無傷**、v2 は `state` を使える |
| 移行期（v2 は dual-write＋`state` 読み） | v1 も v2 も**両方 OK** |
| contract（`status` を落とす） | v2 は無傷、v1 は壊れる（**が、退役後なので問題ない**） |

一気張り替えは「デプロイ完了までの数分間、旧タスクが全部 500 を返す」ことを意味する。

## 手順（改名・列削除・型変更に共通）

1. **expand**: 新しい形を**足すだけ**（新列は nullable、または既定値つき）。既存アプリは無傷。
   必要なら backfill（旧→新へ値を写す。大きい表はチャンク分割）。
2. **dual-write デプロイ**: 新アプリは**旧と新の両方に書く**。読みはまだ旧でも新でもよい。
   この時点で旧アプリと新アプリが同居しても、どちらも読める。
3. **読み切替**: 新アプリの読みを新列へ。全タスクが新版になるまで待つ。
4. **contract**: 旧タスクが**全て退役してから**、旧列を落とす（または NOT NULL 化）。

## やってはいけない

- **列/テーブルをいきなり RENAME・DROP**（旧アプリが即壊れる）。
- **追加列をいきなり NOT NULL（既定値なし）**（旧アプリの INSERT が壊れる）。→ まず nullable。
- **contract を旧退役前にやる**（＝先に消す）。順序が命。
- アプリのデプロイと DDL を**同一リリースに束ねて同時に**当てる（片方だけ先に効く瞬間で壊れる）。

## 適用範囲・保証しない範囲

- MySQL 8.0 の `ADD`/`DROP COLUMN` は多くが online DDL だが、**大きな表の所要とロックは表サイズ・
  版・操作で変わる**（`ALGORITHM=INSTANT/INPLACE` の可否、必要なら gh-ost/pt-osc）。
- dual-write の一貫性は「書き込み経路が1つ（このアプリ）」が前提。複数書き手なら別途。
- backfill は小表で一括。1回で終わらない規模ではチャンク分割・進捗管理・再開性が要る（[EXP-6](migration-crash.md)）。
