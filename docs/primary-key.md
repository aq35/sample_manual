# 主キー設計（連番 BIGINT vs UUID）（EXP-54）

InnoDB のテーブルは**主キーの B-tree そのもの**（clustered index）。行データは主キー順に並ぶ。
主キーが単調増加（連番 BIGINT）なら挿入は木の右端に**追記**され、ページ分割がほぼ起きず密に詰まる。
ランダム（UUID v4）だと挿入位置が散らばり、**ページ分割・断片化**で index が肥大し挿入も重い。
さらに**二次索引は主キーを内包**するので、太い主キー（CHAR(36)）は全二次索引を膨らませる。

実装は [internal/pklab](../internal/pklab)、receipt は
[docs/results/exp-54](results/exp-54/exp-54-primary-key-design.md)。

## 結果（各 10万行・chunk500 multi-row。往復は全種同じ＝主キーの順序だけが違う）

| 主キー | 挿入 | data | index | 備考 |
| --- | --- | --- | --- | --- |
| BIGINT AUTO_INCREMENT（追記） | **1.5s** | 11.8MB | **2.6MB** | 最速・最小（基準） |
| UUID v4 CHAR(36)（散らばる・太い） | **2.8s** | 24MB | **8.8MB** | 挿入 1.8x・index 3.4x |
| UUID v4 BINARY(16)（散らばる・細い） | 2.4s | 21MB | 4.6MB | 細くしても散らばりは残る |
| 時刻順 UUID BINARY(16)（追記に戻す） | **1.6s** | 12.8MB | **2.6MB** | BIGINT 並みに回復 |

- **ランダム UUID は連番 BIGINT より挿入が遅く index が肥大**（CHAR(36) で index 3.4 倍）。
- **CHAR(36) は BINARY(16) の倍以上 index が太い**（36B vs 16B の主キーが**全二次索引に内包**される）。
- **時刻順 UUID（BINARY16）は追記に戻り**、BIGINT 並みの速度・サイズに回復する。

## 要点

- **既定は連番 BIGINT AUTO_INCREMENT**。追記で密・最速・index 最小。迷ったらこれ。
- **UUID を使う理由があるときだけ使う**：分散生成（採番の中央集権を避ける）・推測されにくさ。
  その場合も **BINARY(16)＋時刻順（UUIDv7 相当）** にして、ランダム UUID の肥大を避ける。
- **CHAR(36) の UUID を主キーにしない**。二次索引すべてに 36B が乗って肥大する。どうしても
  文字列で扱いたいなら、格納は BINARY(16)、表示時に整形する。
- ランダム主キーは buffer pool に収まる間は差が小さく、**溢れると I/O 律速で一気に開く**。
  「今は平気」でも行数が増えると効いてくる。

## 保証しない範囲・未検証

- 絶対時間はこのホスト。差の向き（ランダム > 連番）は安定だが、buffer pool・行数で強弱が動く。
- data/index サイズは ANALYZE 後の information_schema 値（ページ単位で粗い近似）。
- 「連番は推測・列挙されうる」点は別の関心（露出する ID は別途ランダム化・[EXP-24](security-layers.md) 系）。
