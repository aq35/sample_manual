# EXP-34 1テナントの大量投入が他テナントの dispatch を待たせるか（公平スケジューリング）

| | |
| --- | --- |
| Experiment | EXP-34 / tenant-fairness |
| Starting SHA | `4bb77e776e25` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 到着順に捌くと、先に大量投入した hog の後ろで victim が待たされる（待ち位置 ≒ hog の件数）。 2) テナント round-robin で捌くと、victim は hog の件数に関係なく早い周で捌ける（待ち位置 ≒ テナント数）。 3) 総処理量は同じ。変わるのは『順序』＝ victim の待ち。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4bb77e776e25+dirty |
| Started / Ended | 2026-09-07T09:42:24Z / 2026-09-07T09:42:24Z |

## Workload

- `hog_items` = 2000
- `total_items` = 2005
- `victim_tenants` = 5

## Results

### 到着順に捌く（unfair）: victim は hog の後ろで待つ — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| victim_worst_position | 2004 |

- victim の最悪待ち位置 ≒ hog の件数（2004 件処理後）

### テナント round-robin（fair）: victim は hog の量に関係なくすぐ — OK

| 数えたもの | 値 |
| --- | --- |
| victim_worst_position | 5 |

- unfair 2004 → fair 5（テナント数ぶんで捌ける）

## Verdict

到着順に捌くと、1テナントの大量投入で他テナントが hog の件数ぶん待たされる。テナントを round-robin で回せば、victim は hog の量に関係なくテナント数ぶんで捌ける。総処理量は同じで、変えるのは『順序』。担当をワーカーで分ける（lease）と更に緩和できる。

## 適用範囲

- MySQL 8.0 / hog 2000件（先着）・victim 5テナント各1件（後着）/ キューは DB、順序を比較
- 待ち位置 = その命令が捌かれる前に処理された件数。待ち時間 ≒ 位置 × 1件の処理時間
- round-robin は1周に各テナント1件。テナントの順は初出順

## 保証しない範囲・未検証

- 実際の待ち時間は 1件の処理時間に依る（位置はその比例係数）
- round-robin は単純な公平化。重み付き（テナントのプラン別）や、hog に上限をかける方式もある
- 複数ワーカーで担当を分ける（lease・EXP-2/14）と、テナントを別ワーカーに散らして更に緩和できる
- 飢餓を完全に防ぐには、pending が古すぎる命令を優先する等の劣化対策も要る

## 再利用できる成果物

- internal/fairnesslab: 到着順 vs テナント round-robin の dispatch 順序
- docs/tenant-fairness.md: ノイジーネイバー対策（公平スケジューリング）

## 次の実験

- なし（EXP-32..34 上位バッチ完了）

