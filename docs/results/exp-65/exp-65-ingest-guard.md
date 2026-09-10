# EXP-65 不正データの入口を DB 制約(ENUM/CHECK/FK)で塞ぐ（アプリのバリデーションは経路しか守れない）

| | |
| --- | --- |
| Experiment | EXP-65 / ingest-guard |
| Starting SHA | `d5beaeb87c79` |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | アプリのバリデーションはアプリ経路しか守らない。直接 DB 入力(手動 SQL・別ツール・移行)はそれを迂回する。 同じ不正 INSERT を、ガード無し表(ゆるい型・制約なし)には landing し、DB 制約(ENUM/CHECK/FK)有り表には 入口で弾かれる。よって直接 DB 入力に対する最後の砦は DB 制約であり、アプリ側チェックだけでは足りない。 ※不正値を丸めずに弾くには STRICT sql_mode が要る(MySQL 8 の既定で入る)。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=d5beaeb87c79 |
| Started / Ended | 2026-09-10T22:49:43Z / 2026-09-10T22:49:43Z |

## Workload

- `bad_case_kinds` = 3

## Failure injection

- `sql_mode` = ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION
- `strict` = true

## Results

### アプリ経路: 作成時バリデーションが不正を弾く — OK

| 測ったもの | 値 |
| --- | --- |
| rejected | 3.000 |

- アプリ経路では 3/3 が弾かれる。ただしこれはアプリを通ったときだけ

### 直接DB入力: ガード無し表（poison が landing） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| landed | 3.000 |
| rows_in_table | 3.000 |

- アプリを迂回した不正 INSERT が 3 件そのまま残る＝後で worker を止める poison

### 直接DB入力: DB制約有り表（入口で弾く） — OK

| 測ったもの | 値 |
| --- | --- |
| landed | 0.000 |
| rows_in_table | 0.000 |

- 弾いた不正: 未知の状態(frozen) / 負の金額(-1) / 存在しない参照(ref=999)（ENUM/CHECK/FK が経路に関係なく拒否）

## Verdict

不正データの入口は DB 制約(ENUM/CHECK/FK/NOT NULL)で塞ぐ。アプリのバリデーションはアプリ経路しか守らず、直接 DB 入力(手動 SQL・別ツール・移行)は迂回する。DB 制約だけが全経路の最後の砦。それでも DB で表せない業務前提違反は残るので、worker は読取り時にも検証し、EXP-45 で隔離してキューを止めない。

## 適用範囲

- MySQL 8.0 / 不正3種(未知状態・負値・存在しない参照)を直接 INSERT
- ing_open は VARCHAR/制約なし、ing_guarded は ENUM+CHECK(amount>=0)+FK(ref)
- STRICT sql_mode 前提（ENUM/範囲を丸めずに弾くため。MySQL 8 の既定）

## 保証しない範囲・未検証

- CHECK は MySQL 8.0.16+ で enforce（それ未満は無視される）
- STRICT が無い環境では ENUM が '' に、範囲外が丸められて landing しうる（この実験は STRICT 前提）
- 入口をすり抜けた分（DB では表せない業務前提違反）は worker の読取り時防御＋EXP-45 の隔離で対処する（本実験外）
- FK はマルチテナントでは参照先もテナント込みで設計する（越境防止・本実験は単純化）

## 再利用できる成果物

- internal/ingestlab: AppValidate(アプリ経路) / DirectInsert(迂回) / ENUM+CHECK+FK の入口ガード
- docs/data-integrity-ingest.md: 不正データの入口を塞ぐ（作成時バリデーション＋DB制約＋防御読取り）

## 次の実験

- EXP-45（すり抜けた poison を隔離してキューを止めない）と接続

