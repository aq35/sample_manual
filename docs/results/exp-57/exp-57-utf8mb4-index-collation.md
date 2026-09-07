# EXP-57 utf8mb4 の index 長上限(3072B)と照合(_ci/_bin)の意味を実挙動で示す

| | |
| --- | --- |
| Experiment | EXP-57 / utf8mb4-index-collation |
| Starting SHA | `81eda7f21f28` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) utf8mb4 は 4B/字。VARCHAR(768)=3072B は全長索引を張れる。 2) VARCHAR(769)=3076B は 3072B 上限を超えて索引作成に失敗(1071)。 3) 長い列でも prefix 索引 (col(255)) なら張れる。 4) _0900_ai_ci は 'abc'='ABC'（大小無視）で一意制約も重複扱い。_bin は厳密で別物扱い。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=81eda7f21f28+dirty |
| Started / Ended | 2026-09-07T13:10:55Z / 2026-09-07T13:10:55Z |

## Results

### VARCHAR(768) utf8mb4: 全長索引 OK（768×4=3072B・上限ちょうど） — OK

| 数えたもの | 値 |
| --- | --- |
| full_index_ok | 1 |

### VARCHAR(769) utf8mb4: 全長索引 失敗(1071)（3076B>3072B） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| full_index_ok | 0 |
| key_too_long_1071 | 1 |

- 1文字でも足すと 4B 増えて上限超過。型名でなくバイト長で決まる

### VARCHAR(1000) でも prefix 索引 (s(255)) なら OK — OK

| 数えたもの | 値 |
| --- | --- |
| prefix_index_ok | 1 |

- 長い列の索引は prefix で。等価一意が要るなら生成列やハッシュ列を別途

### utf8mb4_0900_ai_ci: 'abc'='ABC'（大小無視）・一意制約も重複扱い — OK

| 数えたもの | 値 |
| --- | --- |
| match_ABC_to_abc | 1 |
| unique_dup_rejected | 1 |

### utf8mb4_bin: 'abc'≠'ABC'（厳密）・別物として両方入る — OK

| 数えたもの | 値 |
| --- | --- |
| match_ABC_to_abc | 0 |
| unique_dup_rejected | 0 |

## Verdict

utf8mb4 は最大 4B/字で、InnoDB(DYNAMIC) の index キー上限 3072B に対し VARCHAR(768) は張れるが 769 は超えて失敗(1071)＝『型名でなくバイト長』で決まる。長い列は prefix 索引 (col(255)) で回避し、完全一意が要るならハッシュ列/生成列を別に持つ。照合は _0900_ai_ci が大小/アクセント無視で人名検索や重複防止に自然、_bin は厳密で ID・トークン向き。多言語不要の列をむやみに utf8mb4 にすると索引長で詰まるので、列ごとに charset/collation を選ぶ。

## 適用範囲

- MySQL 8.0 InnoDB ROW_FORMAT=DYNAMIC / index キー上限 3072B / utf8mb4=最大4B/字
- 『型名でなくバイト長』で索引可否が決まる（VARCHAR(768) と (769) の境界）
- 照合は列の COLLATE で決まり、等価比較・ORDER BY・一意制約すべてに効く

## 保証しない範囲・未検証

- 3072B は DYNAMIC/COMPRESSED＋innodb_large_prefix(8.0 既定 on)の値。REDUNDANT/COMPACT だと 767B
- prefix 索引は前方一致・範囲には効くが、prefix より後ろの一意性は保証しない（完全一意はハッシュ列/生成列）
- _ci は人が読む名前の検索・重複防止に自然。ID・トークン・大小を区別したい値は _bin
- latin1/ascii は 1B/字で上限に余裕。多言語不要の列をむやみに utf8mb4 にすると索引長で詰まる

## 再利用できる成果物

- internal/charsetlab: TryFullIndex / TryPrefixIndex / Collation
- docs/charset.md: utf8mb4 の index 長と照合

## 次の実験

- （未実験候補セットの最後）

