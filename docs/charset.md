# utf8mb4 と index 長・照合（EXP-57）

utf8mb4 は1文字**最大4バイト**（絵文字含む完全な UTF-8）。InnoDB(ROW_FORMAT=DYNAMIC) の
index キー上限は **3072 バイト**。だから索引可否は**型名でなくバイト長**で決まる：
`VARCHAR(768)×4 = 3072B` は張れるが `VARCHAR(769) = 3076B` は超えて失敗する。長い列は
**prefix 索引**で回避。照合(collation)は等価判定・ORDER BY・一意制約の意味を変える。

実装は [internal/charsetlab](../internal/charsetlab)、receipt は
[docs/results/exp-57](results/exp-57/exp-57-utf8mb4-index-collation.md)。

## 結果

### index 長の境界（utf8mb4・DYNAMIC・上限 3072B）

| 列 | 全長索引 | 備考 |
| --- | --- | --- |
| `VARCHAR(768)` | **OK**（768×4=3072B ちょうど） | 上限ぎりぎり |
| `VARCHAR(769)` | **失敗 1071**（3076B > 3072B） | 1字足すだけで超過 |
| `VARCHAR(1000)` に prefix `s(255)` | **OK** | 長い列は prefix で索引 |

### 照合(collation)の意味

| collation | `name='ABC'` が `'abc'` に一致 | `'abc'` の後に `'ABC'` を UNIQUE 挿入 |
| --- | --- | --- |
| `utf8mb4_0900_ai_ci`（既定・大小/アクセント無視） | **一致する** | **重複拒否(1062)** |
| `utf8mb4_bin`（厳密） | 一致しない | 別物として**両方入る** |

## 要点

- **索引を張る列のバイト長を意識する**。utf8mb4 は 4B/字なので、`VARCHAR(768)` を超える列に
  全長索引は張れない。**長い列は prefix 索引** `INDEX(col(255))` にする。
- **prefix 索引は前方一致・範囲には効くが、prefix より後ろの一意性は保証しない**。完全一致の一意制約が
  要るなら、**ハッシュ列（例: `SHA2(col)` の生成列）や生成列に索引**を張る。
- **照合は用途で選ぶ**：
  - 人が読む名前の検索・重複防止 → `utf8mb4_0900_ai_ci`（大小/アクセントを無視して自然に一致）。
  - ID・トークン・大小を区別したい値 → `utf8mb4_bin`（厳密）。
- **多言語不要の列をむやみに utf8mb4 にしない**。ASCII 主体の ID・コード列は `ascii`/`latin1`（1B/字）で
  索引長に余裕ができ、`VARCHAR` を長く取れる。列ごとに charset/collation を選ぶ。

## 保証しない範囲・未検証

- 3072B は DYNAMIC/COMPRESSED＋`innodb_large_prefix`（8.0 既定 on）の値。REDUNDANT/COMPACT だと 767B。
- 照合の比較コスト（`_ci` の照合計算 vs `_bin` のバイト比較）の速度差は本実験では測っていない（意味の違いに集中）。
- 実際に格納されるバイトは可変長（短い ASCII は utf8mb4 でも 1B）。上限は索引の**予約**に効く。
