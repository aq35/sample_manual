# EXP-67 enum 的 named 型の switch 網羅を go/analysis で強制する（Go に sum type が無い弱点を補う）

| | |
| --- | --- |
| Experiment | EXP-67 / exhaustive-switch |
| Starting SHA | `6d32b2156888` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | Go は case の書き忘れをコンパイルで防げない（sum type / enum が無い）。 『同じ named 型の定数が2つ以上』を enum とみなし、その型を tag に持つ switch が全メンバを 網羅しているか（default 無しで）を型情報つきで検出できる。default のある switch は対象外。 ネストした switch の絞り込み（外側 case で除外済み）は追えないので、そこは理由つき逃げ道で通す。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=6d32b2156888+dirty |
| Started / Ended | 2026-09-10T23:25:22Z / 2026-09-10T23:25:23Z |

## Results

### 検出の正しさ（analysistest） — OK

- 未網羅（Completed 忘れ・default 無し）→ 検出
- 全メンバ網羅 → 検出しない
- default あり → 検出しない（意図的に『残りはまとめて』）
- //smlint:allow exhaustive 理由: ... → 通す

### このリポジトリ全体に当てた結果（対応前） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| findings | 2 |

- internal/repo/guard.go: SQL 種別(kind)の switch が kindInsert/kindOther を明示していなかった。挙動は正しい（テナントの目印は switch より前で全 kind に要求済み）が、意図が暗黙だった。→ 空の case kindInsert, kindOther を足して明示。新しい kind を足すと再び検出される形に。
- internal/pklab/lab.go: ネストした switch が BigintAuto を欠いていた。だが BigintAuto は外側 switch の case で処理済みで、この default 側には到達しない（構文では追えない絞り込み）。→ 理由つき逃げ道で通す（検出の限界を正直に残す）。

### 対応後 — OK

| 数えたもの | 値 |
| --- | --- |
| findings | 0 |

- repo 全体で exhaustive の指摘 0 件（本物は明示化・限界は理由つき逃げ道）

## Verdict

switch の網羅は Go の型システムでは強制できないが、go/analysis で検出できる。このリポジトリに当てて本物の暗黙 case 1件を明示化した。ネスト絞り込みの限界は逃げ道で正直に残す。

## 適用範囲

- Go の構文＋型情報だけ（go/analysis）。実行時は見ない
- enum の判定＝定義パッケージに同じ named 整数/文字列型の定数が2つ以上
- default のある switch は対象外。case が定数でない（範囲・式）ものは追わない

## 保証しない範囲・未検証

- ネストした switch の絞り込み（外側 case で除外済みの値）は追えない（pklab がその例・逃げ道で対応）
- 別パッケージの enum でも、その型の定数がエクスポートされていれば集められる。未エクスポートは同一パッケージ内のみ
- iota で歯抜けの値や、型変換で作った値（Status(99)）は case では追えない

## 再利用できる成果物

- internal/lint/exhaustive.go: 網羅性の検査。Doc に何を見て何を見ないかを明記
- cmd/sqllint に自動的に組み込まれる（Analyzers() に追加済み）

## 次の実験

- EXP-68 エラー分類（一時/恒久）の型付けと取りこぼし検出

