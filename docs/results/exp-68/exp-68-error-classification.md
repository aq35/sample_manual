# EXP-68 エラー分類を bool でなく3値(Transient/Permanent/Unknown)で持ち、取りこぼしを検出する

| | |
| --- | --- |
| Experiment | EXP-68 / error-classification |
| Starting SHA | `abf645a59574` |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | bool(retry可否)だと Permanent と Unknown が同じ false に潰れ、『知らない』を『恒久』と偽る （下流の扱い＝DLQ+アラート vs 不明として surface を分けられない）。 3値の型で持てば両者を区別でき、かつ『恒久以外は retry』の素朴設計が Unknown を無限 retry する事故も避けられる。 Kind を足したときの switch 取りこぼしは EXP-67 の exhaustive 検査が拾う。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=abf645a59574 |
| Started / Ended | 2026-09-10T23:33:07Z / 2026-09-10T23:33:07Z |

## Results

### bool 分類（EXP-49）: Permanent と Unknown が潰れる — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| false_bucket_size | 2.000 |

- retry不可バケツ = [permanent, unknown]。恒久と不明が同居し、下流で分けられない

### 3値分類（EXP-68）: 3つに分かれる — OK

| 測ったもの | 値 |
| --- | --- |
| distinct_kinds | 3.000 |

- transient→transient / permanent→permanent / unknown→unknown（Unknown を恒久と偽らない）

### Unknown への retry: 素朴『恒久以外 retry』 — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| attempts | 5.000 |
| retries_unknown | 1.000 |

- Unknown を Transient 扱いして maxAttempts=5 回まで無駄に叩く

### Unknown への retry: 3値 ShouldRetry（fail-safe） — OK

| 測ったもの | 値 |
| --- | --- |
| attempts | 1.000 |
| retries_unknown | 0.000 |

- Unknown は retry しない（1回で返す）。下流で『不明』として surface する

## Verdict

エラー分類は bool でなく3値(Transient/Permanent/Unknown)の型で持つ。bool は恒久と不明を潰し、『知らない』を『恒久』と偽る（下流を分けられない・素朴設計は Unknown を無限 retry）。3値なら retry 判断と下流の扱いを分けられ、Kind の取りこぼしは EXP-67 の exhaustive が拾う。

## 適用範囲

- エラー値を直接構築して分類（MySQL 1213/1062・未知の error）。DB は不要
- 3値 = Transient / Permanent / Unknown。retry 判断と下流の扱いを分けるための型
- 取りこぼし（Kind を足して switch 未更新）は EXP-67 の exhaustive 検査が担保

## 保証しない範囲・未検証

- どの MySQL 番号を Permanent とみなすかは運用依存（ここでは代表的な数種）
- Unknown の下流処理（surface / 限定 retry / DLQ）は業務要件。ここでは『retry しない＋区別できる』までを示す
- 実 DB 由来のエラーでの分類網羅は EXP-49（retrylab）側で実測済み

## 再利用できる成果物

- internal/errclass: Kind(3値)・Classify・ShouldRetry(exhaustive な switch)・Naive(事故側)
- EXP-67 exhaustive: Kind を足すと ShouldRetry の未網羅を検出

## 次の実験

- EXP-69 encoding/json/v2 のパースコスト

