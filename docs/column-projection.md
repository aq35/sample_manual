# 列を絞る・VARCHAR と TEXT・SELECT * の効き（EXP-21）

「列を減らすのは意味ある？」「VARCHAR と TEXT どっちが重い？」「`SELECT *` は
TEXT でも重い？」への実測の答え。EXP-19（[縦分割](column-split.md)）の続きで、
**行の太さがどこから来るか**を詰める。

実装は [internal/widthlab](../internal/widthlab)。receipt は
[docs/results/exp-21](results/exp-21/exp-21-varchar-vs-text.md)。

## 先に結論（chat での言い過ぎを訂正）

「TEXT にすれば舐めは軽い」は**サイズ次第で嘘**。重さは **VARCHAR か TEXT かでは
なく、値が行の中（inline）にあるか、行の外（off-page）にあるか**で決まる。InnoDB
（DYNAMIC）は、値が行に収まればその型が TEXT でも inline に置く。収まらないほど
大きい値だけを行の外へ追い出す。

## 結果（3万行 / MySQL 8.0 / ROW_FORMAT=DYNAMIC）

### ① 全体走査（COUNT・memo を SELECT しない）

| テーブル | memo | 保存場所 | p50 |
| --- | --- | --- | --- |
| w_varchar | 3KB VARCHAR | inline | 60ms |
| w_text_small | 3KB TEXT | **inline** | 125ms |
| w_text_big | 24KB TEXT | **off-page** | **10.7ms** |
| w_narrow | 無し | — | 10.9ms |

- **3KB は VARCHAR でも TEXT でも inline**。どちらも narrow（11ms）より大幅に重い。
  「TEXT にしただけ」では軽くならない。
- **24KB の TEXT は off-page**。行が細くなるので、memo を取らない走査は **narrow 並み
  （10.7ms）**。つまり「大きい方が舐めは軽い」という逆転が起きる。理由は行の外に
  出るから。

### ② off-page の大きい TEXT を `SELECT *`（本体込み）で読む

| 読み方 | p50 |
| --- | --- |
| memo 取らない（`SELECT id,status`） | 4ms |
| memo も取る（`SELECT *` 相当） | **163ms**（約 40x） |

off-page は「舐めは軽いが、読むと重い」。`SELECT *` すると 1 行ごとに 24KB を行の外へ
取りに行く。

## 指針

- **重さの正体は inline か off-page か**。「太い列＝重い」ではなく「太い値を行に
  載せる＝重い」。
- **`SELECT *` をやめる**のは、off-page の大きい列を毎回引きずり出さないため。ここは
  効く（163ms → 4ms）。GraphQL でも要求スカラーだけ投影し、大きい列は別リゾルバへ。
- **確実なのは別表に分けること**（[EXP-19](column-split.md)）。サイズに関係なく舐める
  表を narrow に保てる。同居のまま軽くできるのは「値が off-page になる大きさ **かつ**
  普段の一覧で `SELECT *` しない」場合に限る。中途半端に大きい inline 値（数 KB の
  VARCHAR/TEXT）は、舐めるだけで重いままなので分割が最も安全。

### まとめ表

| 状況 | 舐め（列を取らない） | SELECT *（列を取る） |
| --- | --- | --- |
| 小さい値（inline） | 重い（行が太る） | 重い |
| 大きい値（off-page） | 軽い | とても重い |
| 別表に分ける | 常に軽い | 別表を引くぶんだけ |

## 保証しない範囲・未検証

- inline/off-page の境目は行フォーマットとページサイズで動く（DYNAMIC・16KB ページ
  前提）。VARCHAR も 8KB 超級に巨大なら off-page になりうる。
- 絶対値はバッファプールに載った状態のもの。ディスクから読むと差はさらに広がる。
