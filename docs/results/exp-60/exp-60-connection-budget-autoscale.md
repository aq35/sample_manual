# EXP-60 台数×プールが max_connections を超えると 1040 拒否。予算で絞れば拒否0・Guard は事前に止める

| | |
| --- | --- |
| Experiment | EXP-60 / connection-budget-autoscale |
| Starting SHA | `e25bdc5b9f67` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 素朴オートスケール（per-container 固定 × 台数増）は、総接続が max_connections を超えると 新規接続が 1040(Too many connections) で拒否される＝台数を増やすほど悪化。 2) poolbudget.RecommendPerContainer で per-container を絞れば総接続が予算内 → 拒否 0。 3) Guard は接続を1本も張る前に超過を検知して fail-fast（起動時に止める）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=e25bdc5b9f67+dirty |
| Started / Ended | 2026-09-08T21:50:30Z / 2026-09-08T21:50:30Z |

## Results

### 素朴オートスケール（固定15本×15台=225要求）: DB が 1040 で拒否（ストーム） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| budget | 141 |
| demand | 225 |
| guard_blocked | 1 |
| max_connections | 151 |
| opened_ok | 150 |
| other_err | 0 |
| rejected_1040 | 75 |

- 要求 225 > 予算 141 / 実際に 75 本が 1040 拒否。Guard は事前に 止めた（fail-fast）

### 予算オートスケール（per-container を予算内に絞る）: 拒否 0 — OK

| 数えたもの | 値 |
| --- | --- |
| budget | 141 |
| demand | 105 |
| fits | 1 |
| guard_ok | 1 |
| opened_ok | 105 |
| other_err | 0 |
| per_container | 7 |
| rejected_1040 | 0 |

- per-container 15→7 / 要求 105 ≤ 予算 141 → 1040 拒否 0

## Verdict

オートスケールで台数を増やすと DB への総接続 = 台数 × プール が膨らみ、max_connections を超えると MySQL が 1040(Too many connections) で新規接続を拒否する＝増やすほど悪化するストーム（実測: 固定15本×15台=225要求で多数が 1040 拒否）。poolbudget.RecommendPerContainer で per-container を予算内に絞れば総接続が 収まり拒否 0。さらに Guard は接続を1本も張る前に超過を検知して起動を止める（fail-fast）。『縦より横』だが、横に割るときは合計接続が DB 予算を超えないことを Guard で担保する。

## 適用範囲

- MySQL 8.0 / max_connections=151 / 予約 10 / 目標 15 台
- 1040 拒否 = DB が『これ以上つなげない』と新規接続を断った回数（実測）
- poolbudget.Demand=台数×per、Budget=max-予約、RecommendPerContainer=余白20%で per を算出

## 保証しない範囲・未検証

- max_connections はこのホストの既定(151)。本番の値・予約・レプリカ読み取りプールで数字は動く
- 『予算に収まる』は接続が枯れないだけで、スループットが良い保証ではない（膝は EXP-5）
- RDS Proxy を挟むと DB 側接続はコンテナ数に比例しなくなる（ProxyBackend で頭打ち・rds-proxy.md）
- 実運用は Guard を起動時に呼び、超過なら起動を失敗させる（1本も張らずに止める）のが安い

## 再利用できる成果物

- internal/budgetlab: 実接続で 1040 拒否を再現し、poolbudget で防げることを実証
- docs/connection-budget.md: 接続予算とオートスケール・ストーム

## 次の実験

- （アーキテクチャ実測）

