# bulk INSERT の正規化（EXP-56）

同じ N 行を入れるのに、やり方で**往復数**と**コミット数（fsync）**が桁で変わる。
autocommit の単発 × N は「N 往復 × N コミット」で最悪。1トランザクションに包むとコミットが1回に。
multi-row INSERT（chunk 件を1文）は往復が N/chunk に。件/秒で正規化して比べる。

実装は [internal/bulklab](../internal/bulklab)、receipt は
[docs/results/exp-56](results/exp-56/exp-56-bulk-insert.md)。

## 結果（N=20,000・chunk=500・同ホスト）

| 方式 | 所要 | 往復 | 件/秒 | 対 single |
| --- | --- | --- | --- | --- |
| autocommit 単発 × N（N 往復・N コミット） | **22.2s** | 20,000 | 901 | 基準（最悪） |
| 1 tx に包む単発 × N（N 往復・1 コミット） | 5.5s | 20,000 | 3,606 | **4x** |
| prepared 再利用＋1 tx（パース1回） | 2.9s | 20,000 | 6,950 | **7.7x** |
| multi-row chunk=500（N/chunk 往復） | **0.31s** | **40** | **63,497** | **70x** |

- **autocommit 単発が最悪**：各文が即コミット＝毎回 fsync、しかも N 往復。
- **1 tx に包むだけで 4x**（コミットが1回に）。**prepared 再利用でパースも省いて 7.7x**。
- **multi-row が圧倒（70x）**：往復が 20,000→40 に激減（chunk 件を1文に）。

## 要点

- **まとめられる書き込みは multi-row INSERT ＋ 1 トランザクション**が基本。往復とコミットの両方を減らす。
- **chunk は数百〜数千**が実務的。大きいほど往復は減るが、1文が巨大だと `max_allowed_packet`・
  ロック保持時間・メモリに当たる。500〜1000 から始めて詰める。
- **往復 RTT が大きい本番ほど効く**（同ホストで 70x なら、RTT 0.5ms の本番では単発の不利が更に拡大）。
  [EXP-31](capacity.md) の「Worker 処理量 ≒ 並行度 × batch / 往復遅延」と同じ話。
- 単発しか選べない経路（1件ずつ来るイベント）でも、**最低限 1 tx にまとめる**か、
  短時間バッファして multi-row に畳む（[EXP-12](scheduling.md) の batch）。
- さらに速くするなら `LOAD DATA INFILE`。ただし権限・エラー処理・運用が変わるので用途を選ぶ。

## 保証しない範囲・未検証

- 絶対値は同ホスト（往復 ~0.1ms）。本番 RTT で単発の不利は拡大する（外挿）。
- `innodb_flush_log_at_trx_commit=2` 等でコミットコストは下がるが durability が緩む（別軸・[EXP-11](backup-restore.md) 系）。
- chunk の最適値は行幅・`max_allowed_packet`・並行度依存（本実験は 500 の一点）。
- multi-row は1文が1トランザクション扱い。巨大 chunk はロック保持・undo に注意。
