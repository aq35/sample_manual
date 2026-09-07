# EXP-48 日パーティションの DROP で保持期間を適用（古い日を丸ごと捨てる）

| | |
| --- | --- |
| Experiment | EXP-48 / retention-policy |
| Starting SHA | `174e88c41208` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 5日ぶんのうち古い2日を DROP PARTITION で消すと一瞬（DELETE で同量消すより速い・EXP-15）。 2) 消えるのは古い日だけ。recent（保持内）は残り、current の書き込みは通る。 3) ロールフォワード（pmax を割って翌日パーティションを用意）でパーティション数は有界に保つ。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=174e88c41208+dirty |
| Started / Ended | 2026-09-07T12:11:08Z / 2026-09-07T12:11:08Z |

## Results

### 保持適用: DROP PARTITION（古い2日） — OK

| 数えたもの | 値 |
| --- | --- |
| old_day_rows | 0 |
| recent_day_rows | 3000 |

| 測ったもの | 値 |
| --- | --- |
| delete_ms_同量 | 46.736 |
| drop_ms | 19.343 |

- DROP 19.343063ms / 同量 DELETE 46.736951ms。古い日=0 行・recent=3000

### ロールフォワード: 翌日パーティションを用意 → current 書き込み OK・パーティション有界 — OK

| 数えたもの | 値 |
| --- | --- |
| parts_after_drop | 4 |
| parts_after_roll | 5 |
| parts_initial | 6 |
| write_ok | 1 |

- パーティション数 初期 6 → drop 後 4 → roll 後 5（初期以下＝増え続けない）

## Verdict

履歴は保持期間で『古い日を丸ごと DROP PARTITION』する（DELETE は undo 肥大で高い）。日ごとのRANGE パーティションにし、定期ジョブで『翌日を用意（ロールフォワード）＋保持超過を DROP』する。消えるのは古い日だけ、recent は残り、current は書け、パーティション数は有界に保たれる。

## 適用範囲

- MySQL 8.0 / 5日×3000行 / 日ごとの RANGE COLUMNS(d) パーティション / 保持3日
- DROP PARTITION は該当パーティションを丸ごと外す（undo 肥大・purge 遅延が無い）
- ロールフォワードは pmax を REORGANIZE して翌日パーティションを切り出す

## 保証しない範囲・未検証

- パーティションキーは全ユニークキーに含める必要（ここは PK に d を含めた）
- REORGANIZE pmax はデータが無ければ一瞬。pmax にデータが溜まっていると重い（先回りで用意する）
- パーティション運用は定期ジョブで（毎日: 翌日を用意し、保持超過を DROP）。DDL なので暗黙コミット
- 絶対時間はこのホストのもの。DROP vs DELETE の差は行数・undo 設定で動く（EXP-15）

## 再利用できる成果物

- internal/retentionlab: 日パーティションの DROP と REORGANIZE によるロールフォワード
- docs/retention.md: 保持期間の運用（パーティション DROP）

## 次の実験

- EXP-49 一時 vs 恒久エラーの分類

