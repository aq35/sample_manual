# 接続予算とオートスケール・ストーム（EXP-60）

オートスケールでコンテナを増やすと、DB への**総接続 = 台数 × プール本数**が膨らむ。これが
`max_connections` を超えると、MySQL は新規接続を **1040(Too many connections)** で拒否する
——**台数を増やすほど悪化する**（ストーム）。接続予算で per-container を絞れば総接続が収まり拒否 0。
さらに `Guard` は**接続を1本も張る前に**超過を検知して起動を止める（fail-fast）。

実装は [internal/budgetlab](../internal/budgetlab)（予算計算は [internal/poolbudget](../internal/poolbudget)）、
receipt は [docs/results/exp-60](results/exp-60/exp-60-connection-budget-autoscale.md)。
飽和の膝は [EXP-5](pool-saturation.md)、横割りの方針は [EXP-30](redundancy.md)。

## 結果（max_connections=151・予約 10・目標 15 台）

| 方式 | per-container | 要求接続 | 実際に開けた | **1040 拒否** | Guard |
| --- | --- | --- | --- | --- | --- |
| 素朴オートスケール（固定 15 本 × 15 台） | 15 | **225** | 150（＝ほぼ上限） | **75（拒否）** | ★止めた |
| 予算オートスケール（予算内に絞る） | **7** | **105** | 105 | **0** | 通した |

- **素朴**：per-container を固定したまま台数を増やすと、要求 225 が上限 151 を超え、**150 本開いた時点で
  残り 75 本が 1040 で拒否**された。オートスケールが**DB を枯らして自らを壊す**。
- **予算**：`RecommendPerContainer(budget, 15, 0.2)` で per-container を **15→7** に絞ると、要求 105 が
  予算（151−10＝141）内に収まり、**拒否 0**。
- **Guard**：素朴構成は「要求 225 > 予算 141」を**接続を1本も張る前に**検知して起動を止められる（一番安い防御）。

## 要点

- **接続は唯一の希少資源**。CPU/メモリが余っていても、`max_connections` を超えた瞬間に**新規接続が拒否**される。
  「縦より横」は正しいが、**横に割るときは合計接続を予算内に収める**のが必須条件。
- **総接続 = Σ(役割ごとの 台数 × プール)**。Web レプリカ・Worker・レプリカ読み取りプールを**全部足す**
  （1台ぶんで考えない・[EXP-30](redundancy.md)）。
- **per-container はオートスケールの目標台数から逆算**する：`per = 予算 × (1−余白) ÷ 目標台数`
  （`poolbudget.RecommendPerContainer`）。台数を増やすなら per を下げる。
- **起動時に `Guard` で fail-fast**：超過するなら**そもそも起動を失敗させる**。実際に 1040 を撒く前に止まる。
- **監視**：`db.Stats().WaitCount/WaitDuration`（プール飽和・[EXP-5](pool-saturation.md)）と、DB 側の
  `Threads_connected` / `Aborted_connects`（1040 の兆候）を出しておく。
- **RDS Proxy を挟むと**、DB 側接続は台数に比例しなくなる（`ProxyBackend` で頭打ち・[rds-proxy](rds-proxy.md)）。
  多重化で緩められるが、pinning を起こす操作では効かない。

## なぜ「増やすほど悪化」するのか（ストーム）

真因が DB（接続/飽和）なのに、レイテンシや CPU の上昇を見てオートスケールが台数を増やすと——
**レプリカ増 → 接続増 → 1040 拒否／DB 飽和 → さらに遅い → もっと台数増**、の悪循環に入る。
だから**スケール前に真因を切り分け**（プール待ちか CPU か・[concern-performance](concern-performance.md)）、
**接続予算を Guard で頭打ち**にしておく。

## 保証しない範囲・未検証

- `max_connections` はこのホストの既定(151)。本番の値・予約・レプリカ読み取りプールで数字は動く。
- 「予算に収まる」は接続が枯れないだけで、**スループットが良い保証ではない**（膝は [EXP-5](pool-saturation.md)）。
- RDS Proxy の多重化・pinning はこの repo では未実測（[rds-proxy](rds-proxy.md) に手順）。
