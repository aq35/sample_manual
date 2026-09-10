# Go に sum type が無い穴を、網羅検査と3値エラー型で埋める（EXP-67 / EXP-68）

- 実験: `internal/lint`（EXP-67・exhaustive アナライザ）/ `internal/errclass`（EXP-68・3値エラー型）
- 実行: `go test ./internal/lint/ -run TestEXP67 -v` / `go test ./internal/errclass/ -run TestEXP68 -v`
- 根拠: EXP-9（[static-analysis](static-analysis.md)）/ EXP-49（[retry](retry.md)）/ EXP-45（[dead-letter](dead-letter.md)）/ EXP-1（OUTCOME_UNKNOWN・[crash-effects](crash-effects.md)）

Go には **sum type / enum が無い**。だから「状態を全部処理したか」「エラーを全部分類したか」を
**コンパイラが保証してくれない**。`switch` の case を1つ書き忘れても通る。この穴を
「静的解析（網羅検査）」と「3値の型」で塞ぐ、というのがこの2実験。

---

## EXP-67 網羅を静的解析で強制する

`pending→in_progress→completed` に `completed` を足したのに、古い `switch status {...}` が
`completed` を処理しないまま黙って素通りする——これはコンパイルエラーにならない。

**こうあるべき**: enum 的な named 型（同じ型の定数が2つ以上）を tag に持つ `switch` が、
全メンバを網羅しているか（`default` 無しで）を **go/analysis（型情報つき）で検出**する。
`cmd/sqllint`（EXP-9）に `exhaustive` として組み込んだ。

- 見る: その named 型を tag に持つ switch と、定義パッケージにあるその型の定数集合。
- 見ない: `default` のある switch（意図的に「残りはまとめて」とみなす）、case が定数でない（範囲・式）もの、
  ネストした switch の絞り込み（外側 case で除外済みの値）。
- 逃げ道: `//smlint:allow exhaustive 理由: ...`（理由必須）。

**このリポジトリに当てた結果（実測）**: 2件検出。
- **本物1件** `internal/repo/guard.go`: SQL 種別の switch が `kindInsert`/`kindOther` を明示していなかった
  （挙動は正しいが意図が暗黙）→ 空の `case kindInsert, kindOther:` を足して明示。**新しい kind を足すと再び検出される**形に。
- **限界1件** `internal/pklab/lab.go`: ネスト switch が `BigintAuto` を欠くが、外側 case で処理済みで到達しない
  → 理由つき逃げ道で通す（構文では追えない絞り込みを正直に残す）。

対応後は repo 全体で exhaustive の指摘 0 件。

---

## EXP-68 エラー分類は bool でなく3値の型で持つ

EXP-49 の分類は **bool（retry してよいか）** だった。だが bool だと
**恒久エラー**と**分からないエラー**が同じ `false` に潰れる。「知らない」を「恒久」と言い切るのは嘘
（EXP-1 の OUTCOME_UNKNOWN と同じ）。

**こうあるべき**: `Kind` を **3値（Transient / Permanent / Unknown）** の型で持つ。

| | retry するか | 下流の扱い |
| --- | --- | --- |
| **Transient**（1213/1205/接続断） | する | バックオフ再試行（EXP-17/49） |
| **Permanent**（1062/制約違反/構文） | しない | DLQ + アラート（EXP-45） |
| **Unknown**（未知の番号・外部の未知失敗） | **しない（fail-safe）** | **「不明」として surface**（恒久と偽らない） |

**実測（EXP-68）**:
- **bool は Permanent と Unknown を区別できない**: `retry不可` バケツに `[permanent, unknown]` が同居
  → 下流で「既知の恒久」と「未知」を分けられない。
- **3値なら3つに分かれる**: transient/permanent/unknown が別ラベル。
- **素朴な『恒久以外は retry』設計は Unknown を無限 retry**: 試行 **5回**（maxAttempts）まで無駄に叩く。
  3値の `ShouldRetry`（Unknown→false）なら **1回**で返す。

さらに `ShouldRetry` は `Kind` を網羅した switch なので、**Kind を足すと EXP-67 の exhaustive が
「未網羅」を指摘する**＝取りこぼしが CI 前に分かる。EXP-67 が道具、EXP-68 がその利用者。

---

## 一行でまとめると

**Go は「全部の状態/エラーを処理したか」を型で保証しない。だから列挙は named 型＋定数で表し、網羅は `exhaustive` 静的検査で強制し、エラーは bool でなく3値（Transient/Permanent/Unknown）の型で持つ。「知らない」を「恒久」と偽らない。**
