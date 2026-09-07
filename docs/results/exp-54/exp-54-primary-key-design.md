# EXP-54 連番 BIGINT は追記で密、ランダム UUID は散らばって index 肥大＋挿入が重い

| | |
| --- | --- |
| Experiment | EXP-54 / primary-key-design |
| Starting SHA | `81eda7f21f28` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 連番 BIGINT は木の右端に追記 → ページ分割ほぼ無し・挿入が最速・index 最小。 2) ランダム UUID は挿入位置が散らばる → ページ分割で index 肥大・挿入が遅い。 3) CHAR(36) は BINARY(16) より太く、二次索引が主キーを内包するぶん更に肥大。 4) 時刻順 UUID(BINARY16) は追記に戻り、ランダムより速く小さい。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=81eda7f21f28+dirty |
| Started / Ended | 2026-09-07T13:10:16Z / 2026-09-07T13:10:25Z |

## Results

### BIGINT AUTO_INCREMENT（追記） — OK

| 数えたもの | 値 |
| --- | --- |
| data_KB | 11792 |
| index_KB | 2576 |
| insert_ms | 1654 |

- 挿入 1654ms / data 11792KB / index 2576KB

### UUID v4 CHAR(36)（散らばる・太い） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| data_KB | 24144 |
| index_KB | 8752 |
| insert_ms | 3259 |

- 挿入 3259ms / data 24144KB / index 8752KB

### UUID v4 BINARY(16)（散らばる・細い） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| data_KB | 21040 |
| index_KB | 4624 |
| insert_ms | 2483 |

- 挿入 2483ms / data 21040KB / index 4624KB

### 時刻順 UUID BINARY(16)（追記に戻す） — OK

| 数えたもの | 値 |
| --- | --- |
| data_KB | 12848 |
| index_KB | 2576 |
| insert_ms | 1747 |

- 挿入 1747ms / data 12848KB / index 2576KB

## Verdict

InnoDB の表は主キーの B-tree。連番 BIGINT は追記で密・最速・index 最小。ランダム UUID は挿入位置が散らばりページ分割で挿入が遅く index が肥大する。さらに二次索引は主キーを内包するので、CHAR(36) はBINARY(16) より全索引が太る。UUID を使うなら BINARY(16)＋時刻順（v7 相当）にして追記の利点を取り戻す。『分散生成・推測されにくさ』が要る時だけ UUID を選び、その場合も肥大を抑える形にする。

## 適用範囲

- MySQL 8.0 InnoDB ROW_FORMAT=DYNAMIC / 各 10万行・chunk500 multi-row（往復は全種同じ）/ payload 80B
- 主キーの順序だけが違う。挿入時間の差はページ分割・断片化、index の差は主キー幅×二次索引の内包
- data/index サイズは ANALYZE TABLE 後の information_schema 値（近似・ページ単位で粗い）

## 保証しない範囲・未検証

- 絶対時間はこのホスト。差の向き（ランダム>連番）は buffer pool・行数で強弱が変わるが向きは安定
- UUID を主キーにしたいなら BINARY(16)＋時刻順（UUIDv7 相当）にすると追記の利点を保てる
- ランダム主キーは buffer pool に入り切る間は差が小さく、溢れると I/O で一気に開く（本実験は同一条件で相対比較）
- 『分散生成したい/推測されたくない』なら UUID の価値はある。その場合も時刻順＋BINARY で肥大を抑える

## 再利用できる成果物

- internal/pklab: 4種の主キーで挿入時間と data/index サイズを測る
- docs/primary-key.md: 主キー設計（連番 vs UUID・肥大の実測）

## 次の実験

- EXP-55 トランザクション分離レベル（RR vs RC）

