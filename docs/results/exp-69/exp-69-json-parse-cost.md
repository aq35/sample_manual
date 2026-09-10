# EXP-69 受信ペイロードのパースを v1 struct / map[string]any / json/v2 で比べる

| | |
| --- | --- |
| Experiment | EXP-69 / json-parse-cost |
| Starting SHA | `6d32b2156888` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | map[string]any は構造体展開より重い（§6.1）。encoding/json/v2 は同じ構造体展開で v1 と 同等〜改善のはず（少なくとも桁で悪化しない）。同じ入力で ns/op・B/op・allocs/op を比べる。 v2 は GOEXPERIMENT=jsonv2 のときだけ測る（未有効なら skip し、その事実を記録する）。 |
| Environment | go1.26.0-X:jsonv2 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=6d32b2156888+dirty |
| Started / Ended | 2026-09-10T23:29:05Z / 2026-09-10T23:29:09Z |

## Workload

- `items` = 1000
- `payload_bytes` = 127289

## Failure injection

- `jsonv2_available` = true

## Results

### v1 構造体展開（[]Item） — OK

| 測ったもの | 値 |
| --- | --- |
| allocs_per_op | 4763.000 |
| bytes_per_op | 433068.000 |
| ns_per_op | 1767963.000 |

### v1 map[string]any（§6.1 の重い受け方） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| allocs_per_op | 26782.000 |
| bytes_per_op | 791869.000 |
| ns_per_op | 4577731.000 |

### v2 構造体展開（encoding/json/v2） — OK

| 測ったもの | 値 |
| --- | --- |
| allocs_per_op | 4763.000 |
| bytes_per_op | 433057.000 |
| ns_per_op | 1482590.000 |

## Verdict

map[string]any は構造体展開より割り当てが多い（§6.1 を再確認・約5.6倍）。受信は必ず構造体で受ける。encoding/json/v2 は v1 struct と同等の桁（詳細は receipt の数値）。

## 適用範囲

- 同一入力（1000件の JSON 配列・127289B）を Unmarshal
- struct=[]Item / map=[]map[string]any / v2=encoding/json/v2 の struct
- Go go1.26.0-X:jsonv2 / testing.Benchmark（自動反復）

## 保証しない範囲・未検証

- 絶対値は環境依存。桁の関係が要点（§6.1 と同じ約束）
- v2 は実験機能。将来 API・性能が変わりうる（GOEXPERIMENT=jsonv2 前提）
- 実運用は「大きい配列は1件ずつ stream で読む（§6.2）」も併用する。ここは一括 Unmarshal の比較

## 再利用できる成果物

- internal/jsonlab: Sample / ParseStructV1 / ParseMapV1 / ParseStructV2（build tag 分離）
- docs/json-v2.md: v1/map/v2 のパースコスト比較

## 次の実験

- なし

