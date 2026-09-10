# 受信ペイロードのパース: v1 struct / map / encoding/json/v2（EXP-69）

- 実験: `internal/jsonlab` / `GOEXPERIMENT=jsonv2 go test ./internal/jsonlab/ -run TestEXP69 -v`
- 根拠: §6.1（`map[string]any` は重い）/ §6.2（大きい配列は stream で読む）

§6.1 で「`map[string]any` は構造体展開より重い」を実測した。Go 1.25/1.26 で実験的に入った
**`encoding/json/v2`**（`GOEXPERIMENT=jsonv2`）で、worker の受信パースが v1 とどう変わるかを
同じ入力で測った。

## 実測（Go 1.26・1,000件の JSON 配列・同一入力）

| 受け方 | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| v1 構造体（`[]Item`） | 1,767,963 | 433,068 | 4,763 |
| **v1 `map[string]any`** | 4,577,731 | 791,869 | **26,782** |
| v2 構造体（`encoding/json/v2`） | 1,482,590 | 433,057 | 4,763 |

読み取れること:
- **`map[string]any` は構造体の約 5.6 倍の割り当て**・約 2.6 倍の時間（§6.1 を再確認）。受信は必ず構造体で受ける。
- **`encoding/json/v2` は v1 構造体と同等**（この入力では時間 約 1.2 倍速・**割り当ては同じ 4,763**）。
  少なくとも桁で悪化しない。v2 に替えても map の代わりにはならない——**効くのは「構造体で受ける」ことそのもの**。

## こうあるべき（＋仮説）

- **受信は構造体で受ける**。これが一番効く（map は桁で重い）。v1/v2 の差はその次。
- **v2 は「同じ構造体展開なら v1 と同等〜微改善」**（実測）。移行の主目的は速度より、v2 の
  厳密さ・API（未知フィールドの扱い・大文字小文字・stream 統合）にある、というのが仮説（本実験は速度のみ）。
- **大きい配列は一括 Unmarshal でなく1件ずつ stream で読む（§6.2）**。本実験は一括の比較で、
  v2 の `jsontext` による stream は別途（未測定）。

> v2 は実験機能。`GOEXPERIMENT=jsonv2` でのみ有効で、将来 API・性能が変わりうる。
> この repo では v2 を build tag で分離し、未有効でも `go build`/`go test` は通る（v2 は skip）。

## 一行でまとめると

**受信 JSON は必ず構造体で受ける（`map[string]any` は約5.6倍の割り当て）。`encoding/json/v2` は同じ構造体展開なら v1 と同等で、map の代替にはならない——効くのは「構造体で受ける」ことそのもの。**
