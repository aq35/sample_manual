# EXP-20 予定/実績を primary とレプリカで読み分ける: 何が安全で何が壊れるか

| | |
| --- | --- |
| Experiment | EXP-20 / read-your-writes-replica-lag |
| Starting SHA | `3f35d91961ef` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 書いた直後にレプリカ（ラグあり）から読むと、自分の書き込みが見えない（read-your-writes 違反）。 2) 同じ読みを primary（Strong）から読めば、必ず見える。 3) ラグが解消した後の『過去の値』は、レプリカから読んでも一致する（不変な過去はレプリカで安全）。 4) worker の dispatch ポーリングは Strong（primary）で行う。レプリカだと未反映で取りこぼす。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=3f35d91961ef+dirty |
| Started / Ended | 2026-09-07T02:53:07Z / 2026-09-07T02:53:08Z |

## Workload

- `replica_lag` = 300ms

## Results

### 書いた直後: Eventual（レプリカ・ラグ中）で読む — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| 見えた | 0 |

- 自分の書き込みが見えない（read-your-writes 違反）。だから直後の読みは Strong にする

### 書いた直後: Strong（primary）で読む — OK

| 数えたもの | 値 |
| --- | --- |
| 見えた | 1 |

- 必ず見える（v=v1）

### ラグ解消後: Eventual（レプリカ）で過去の値を読む — OK

| 数えたもの | 値 |
| --- | --- |
| 見えた | 1 |

- 不変な過去はレプリカで一致（v=v1）。履歴・レポートはここへ回す

## Verdict

予定/実績の読みをレプリカへ回すと primary の負担は減るが、レプリカにはラグがある。書いた直後・read-your-writes・worker の dispatch ポーリングは primary（Strong）で行う。不変な過去（過去の実績・履歴・レポート・admin の横断）だけをレプリカ（Eventual）へ回す。優先度は低め: 30テナントでは primary に余裕があり、まず covering索引/有界化/キャッシュが効く。読み QPS が実測で primary を圧迫し始めたら、ラグ許容の読みだけをレプリカへ。

## 適用範囲

- MySQL 8.0 / primary=workerdb, レプリカ相当=workerdb2 / ラグを 300ms で手動模擬
- 実レプリケーションではなく、遅延つきで replica へ適用して振る舞いを再現

## 保証しない範囲・未検証

- 実運用のラグは負荷で変動する。ラグ量の測定・監視は別途（Seconds_Behind_Master 等）
- read-your-writes を厳密にやるなら、書き込み後しばらく Strong に固定する（セッションピン留め）
- レプリカのフェイルオーバ・整合（GTID）は本実験外

## 再利用できる成果物

- internal/readrouter: Freshness(Strong/Eventual) で primary/レプリカを振り分けるルータ

## 次の実験

- なし

