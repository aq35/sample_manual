# EXP-37 メトリクスのカーディナリティ制御と記録オーバーヘッド

| | |
| --- | --- |
| Experiment | EXP-37 / observability |
| Starting SHA | `abeb03baf583` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 有界ラベル(tenant 30 × status 6)なら時系列は 180 程度で収まる。 2) robot_id を混ぜると時系列がリクエストの種類数まで爆発する。 3) cap を設ければ、超過は __over__ に畳まれ、実系列数は cap 以下に保たれる。 4) 記録コストは 1回あたりサブマイクロ秒。DB 往復に比べ無視できる。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=abeb03baf583+dirty |
| Started / Ended | 2026-09-07T09:52:58Z / 2026-09-07T09:52:58Z |

## Results

### 有界ラベル: tenant×status（robot_id を入れない） — OK

| 数えたもの | 値 |
| --- | --- |
| over | 0 |
| series | 180 |

- 10万リクエストでも時系列は 180（=tenant×status）

### 高カーディナリティ: robot_id をラベルに（cap 無し）→ 時系列が爆発 — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| series | 100000 |

- 時系列が 100000 まで増える（メモリと監視基盤を圧迫）

### cap=1000: 超過は __over__ に畳む → 実系列は上限以下 — OK

| 数えたもの | 値 |
| --- | --- |
| over | 99000 |
| series | 1000 |

- 実系列 1000（≤cap）/ 畳んだ回数 99000

### 記録コスト: Inc / Observe（1回あたり） — OK

| 測ったもの | 値 |
| --- | --- |
| inc_ns | 84.985 |
| inc_ops_per_sec | 11766846.687 |
| observe_ns | 17.700 |

- Inc 85.0ns / Observe 17.7ns（DB 往復 ~100000ns に比べ桁違いに安い）

## Verdict

可観測性は RED を境界で取り、ラベルは有界なもの（tenant・status・種別）だけにする。robot_id/command_id のような高カーディナリティをラベルにすると時系列が爆発する。cap を最後の安全弁に、超過は __over__ へ畳む。記録コスト自体はサブマイクロ秒で、DB 往復に比べ無視できる。

## 適用範囲

- 純 Go（DB 不要）/ 10万回の Inc・200万回の記録ベンチ / 8並行の安全性チェック
- 時系列数 = ラベル値の直積。ラベルは有界なものだけにする（tenant・status・種別）
- cap は最後の安全弁。設計としては高カーディナリティをそもそもラベルにしない

## 保証しない範囲・未検証

- ns/op はこのホストのもの。桁（サブマイクロ秒）が要点で、絶対値は環境で動く
- 本物の監視基盤（Prometheus 等）では系列ごとにメモリ・スクレイプ負荷がかかる。cap はその保険
- 分散環境では各レプリカの系列が合算される。ラベル設計は全レプリカ横断で効く
- トレース（web→worker の連結）は本実験外（ラベル/コストのみ）

## 再利用できる成果物

- internal/metrics: カーディナリティ上限つきラベルカウンタと遅延ヒストグラム
- docs/observability.md: 何を測るか・ラベル設計・記録コスト

## 次の実験

- なし（EXP-32..37 完了）

