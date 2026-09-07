# EXP-35 DST の現地時刻予定の罠と、DATETIME/TIMESTAMP のセッション tz 挙動

| | |
| --- | --- |
| Experiment | EXP-35 / timezone-dst |
| Starting SHA | `abeb03baf583` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 春の切替日、現地 02:30 は存在しない（02:00→03:00）。素朴に time.Date すると別の時刻にずれる（Go は 01:30 に正規化した。方向は実装依存だが 02:30 は保てない）。 2) 秋の切替日、現地 01:30 は二重に起きる。UTC で見ると1時間違う2つの瞬間になる。 3) DATETIME はセッション tz を変えても値が変わらない。TIMESTAMP は変わる（UTC 保存のため）。 4) だから予定・実績は UTC 保存し、現地の繰返し予定は tz 対応で gap/二重を明示的に解決する。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=abeb03baf583+dirty |
| Started / Ended | 2026-09-07T09:46:43Z / 2026-09-07T09:46:43Z |

## Results

### 春の切替: 現地 02:30 は存在せず、素朴 time.Date が別の時刻にずらす — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| got_hour | 1 |
| wanted_hour | 2 |

- time.Date(2026-03-08 02:30 NY) = 01:30 EST（02:30 の予定が別の時刻に。02:00–03:00 は存在しない）

### 秋の切替: 現地 01:30 が二重（UTC では1時間違う2つ） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| same_wallclock | 1 |
| utc_gap_hours | 1 |

- 2つの 01:30 は UTC で 05:30 と 06:30（1時間差）

### DATETIME/TIMESTAMP: +00:00 で入れ +09:00 で読む — OK

- 入れた値: 2026-06-01 12:00:00（tz +00:00）
- DATETIME 読み(+09:00): 2026-06-01 12:00:00（変わらない＝tz 無視の literal）
- TIMESTAMP 読み(+09:00): 2026-06-01 21:00:00（+9h ずれる＝UTC 保存で変換）

## Verdict

予定・実績は UTC で保存し（epoch か、UTC 固定運用の DATETIME/TIMESTAMP を一貫して）、スケジュール計算も基本 UTC で行う。『毎日 02:30 現地』のような現地繰返し予定だけ、tz 対応の計算で存在しない時刻(春)・二重の時刻(秋)を明示的に解決する（素朴な time.Date 任せにしない）。DATETIME はセッション tz で変わらず、TIMESTAMP は変わる——混在させず方針を1つに。

## 適用範囲

- Go time + 埋め込み tzdata（America/New_York）/ MySQL の DATETIME・TIMESTAMP をセッション tz +00:00/+09:00 で
- 春 2026-03-08・秋 2026-11-01 の US 切替日を使用
- 数値オフセット（+00:00/+09:00）を使い、名前付き tz テーブルの有無に依存しない

## 保証しない範囲・未検証

- 実アプリの tz は運用地域による。ここは US で代表
- MySQL の named time zone（'America/New_York'）は tz テーブル投入が要る。数値オフセットは常に可
- go-sql-driver の loc/parseTime とセッション tz の相互作用は設定依存（ここは DATE_FORMAT で表示値を比較）

## 再利用できる成果物

- internal/tzlab: DST の gap/二重の再現と DATETIME/TIMESTAMP のセッション tz 挙動
- docs/timezone.md: 時刻の保存と DST スケジューリングの指針

## 次の実験

- EXP-36 context キャンセルでクエリが止まるか

