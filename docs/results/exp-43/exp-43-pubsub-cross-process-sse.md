# EXP-43 複数ゲートウェイ跨ぎの SSE fan-out（pub/sub）とテナント分離

| | |
| --- | --- |
| Experiment | EXP-43 / pubsub-cross-process-sse |
| Starting SHA | `e266062c23a5` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) テナントごとに1 poller が DB を読み broker(topic=テナント)へ publish。DB 読みは接続/ゲートウェイ数に無関係。 2) 各ゲートウェイは topic を購読しローカル hub で自分の接続へ配る（2段 fan-out）。 3) topic=テナント なので A の publish は A の接続だけに届く（プロセスを跨いでも混線しない）。 4) 実 Redis/NATS は差し替え（Broker interface）。ここは MemBroker で意味論を再現。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=e266062c23a5+dirty |
| Started / Ended | 2026-09-07T11:35:00Z / 2026-09-07T11:35:01Z |

## Results

### pub/sub 跨ぎ: DB 読みは poller の分だけ（接続/ゲートウェイに無関係） — OK

| 数えたもの | 値 |
| --- | --- |
| broker_dropped | 0 |
| conns | 12 |
| db_reads | 32 |
| gateways | 3 |
| naive_reads | 192 |

- 接続 12・ゲートウェイ 3 でも DB 読みは poller ぶん 32（素朴なら接続数×tick=192）

### テナント分離: A の publish は A の接続だけ・混線ゼロ（プロセス跨ぎでも） — OK

| 数えたもの | 値 |
| --- | --- |
| a_got | 1 |
| b_got | 1 |
| cross_leaks | 0 |

- topic=tenant で分離。A購読者にB値・B購読者にA値=0 件

## Verdict

SSE を複数プロセスに分けるときは、テナントごとに1 poller が DB を読み broker(topic=テナント)へpublish、各ゲートウェイが topic を購読してローカル hub で自分の接続へ配る（2段 fan-out）。DB 読みは poller の分だけ（接続・ゲートウェイ数に無関係）、topic=テナント でプロセスを跨いでも混線しない。Broker は interface にして Redis/NATS へ差し替える（at-most-once・永続要なら stream 型）。

## 適用範囲

- 純 Go（DB/Redis 不要）/ MemBroker で pub/sub を再現 / 3ゲートウェイ×(A2,B2)接続・poller 2本
- 2段 fan-out: broker(topic=テナント) → 各ゲートウェイのローカル hub → 接続
- DB 読みは poller のみカウント。topic=テナント でプロセス跨ぎでも分離

## 保証しない範囲・未検証

- 実 Redis/NATS は未実行（ネットワーク制限）。Broker interface 差し替えで実装可能・意味論は同じ
- 実運用はゲートウェイが topic をテナントにつき1本購読（接続ごとでない）。ここもその形
- broker のドロップ/順序/再接続は実装依存（Redis pub/sub は at-most-once）。永続が要るなら stream 型

## 再利用できる成果物

- internal/pubsub: Broker interface と MemBroker（Redis/NATS 差し替え可能）
- docs/sse-fan-in.md: 複数プロセス構成（poller＋ゲートウェイ＋pub/sub）

## 次の実験

- なし

