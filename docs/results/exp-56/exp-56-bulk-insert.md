# EXP-56 往復数とコミット数で INSERT の件/秒が桁で変わる

| | |
| --- | --- |
| Experiment | EXP-56 / bulk-insert |
| Starting SHA | `81eda7f21f28` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) autocommit 単発×N は N 往復＋N コミット(fsync) で最も遅い。 2) 1 tx に包むとコミットが1回になり大幅に速い（往復は N のまま）。 3) prepared 再利用はパースを省き tx 単発より速い。 4) multi-row（chunk 件を1文）は往復が N/chunk になり最速。件/秒で数倍〜桁。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=81eda7f21f28+dirty |
| Started / Ended | 2026-09-07T13:10:25Z / 2026-09-07T13:10:54Z |

## Results

### autocommit 単発×N（N 往復・N コミット） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| ms | 20332 |
| roundtrips | 20000 |
| rows_per_sec | 983 |

- 20.332326405s / 20000 往復 / 983 件/秒

### 1 tx に包む単発×N（N 往復・1 コミット） — OK

| 数えたもの | 値 |
| --- | --- |
| ms | 5469 |
| roundtrips | 20000 |
| rows_per_sec | 3656 |

- 5.469965473s / 20000 往復 / 3656 件/秒

### prepared 再利用＋1 tx（パース1回） — OK

| 数えたもの | 値 |
| --- | --- |
| ms | 2944 |
| roundtrips | 20000 |
| rows_per_sec | 6791 |

- 2.944838937s / 20000 往復 / 6791 件/秒

### multi-row chunk=500（N/chunk 往復） — OK

| 数えたもの | 値 |
| --- | --- |
| ms | 285 |
| roundtrips | 40 |
| rows_per_sec | 70147 |

- 285.115191ms / 40 往復 / 70147 件/秒

## Verdict

同じ N 行でも INSERT のやり方で件/秒が桁で変わる。最悪は autocommit 単発×N（N 往復＋N コミット＝毎回 fsync）。1 tx に包むとコミットが1回になり大幅に速い。prepared 再利用でパースも省ける。最速は multi-row（chunk 件を1文＝往復が N/chunk）。まとめられる書き込みは multi-row＋1 tx が基本。chunk は max_allowed_packet とロック時間を見て数百〜数千に。往復 RTT が大きい本番ほど効く。

## 適用範囲

- MySQL 8.0 InnoDB / N=20000・chunk=500 / 同ホスト（往復 ~0.1ms）/ payload ~30B
- 件/秒 = N ÷ 所要秒。往復数はやり方で決まる（単発=N、multi-row=N/chunk）
- autocommit はデフォルトで各文が即コミット＝毎回 fsync（durability 設定で強弱）

## 保証しない範囲・未検証

- 絶対値は同ホスト。本番は往復 RTT が効くほど単発の不利が拡大（multi-row/バッチの価値が増す）
- chunk は大きいほど往復が減るが、1文が巨大だと max_allowed_packet・ロック時間・メモリに注意（数百〜数千が実務的）
- 更に速くするなら LOAD DATA INFILE（本実験外）。ただし運用・権限・エラー処理が変わる
- innodb_flush_log_at_trx_commit=2 等でコミットコストは下がるが durability が緩む（別軸）

## 再利用できる成果物

- internal/bulklab: 4 方式の INSERT 速度（往復・コミットの効き）
- docs/bulk-insert.md: bulk INSERT の正規化（往復とコミットを減らす）

## 次の実験

- EXP-57 utf8mb4 と index 長

