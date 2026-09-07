# EXP-41 gqlgen の subscription をテナント単位 hub で作る（接続ごとに DB を引かない）

| | |
| --- | --- |
| Experiment | EXP-41 / gqlgen-subscription-hub |
| Starting SHA | `4ee03b5c73be` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) subscription リゾルバはチャネルを返すだけ。DB は引かず hub（Registry）に相乗りする。 2) テナントの版が変わると、購読チャネルに流れる。 3) ctx が切れるとリゾルバは購読解除し、hub の最後の1人なら poller も止まる（漏れ防止）。 4) テナントは ctx から取る（subscription でも引数からは取らない）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=4ee03b5c73be+dirty |
| Started / Ended | 2026-09-07T10:51:39Z / 2026-09-07T10:51:39Z |

## Results

### subscription が hub から配信を受ける（tenant は ctx から） — OK

| 数えたもの | 値 |
| --- | --- |
| active_while | 1 |
| first | 0 |
| got5 | 1 |
| got9 | 1 |

- 版 0→5→9 が購読チャネルへ流れた。接続は hub に相乗り（DB 読みは poller の分だけ）

### ctx 切断 → チャネルが閉じ、購読解除（poller も止まる） — OK

| 数えたもの | 値 |
| --- | --- |
| active_after | 0 |
| channel_closed | 1 |

- active_tenants 1 → 0（最後の1人が抜け poller 停止）

## Verdict

gqlgen のサブスクリプションは『チャネルを返す』だけ。接続ごとに DB を引かず、テナント単位のhub（Registry）に相乗りさせる。版が変わると全接続へ流れ、ctx 切断で購読解除＋poller 停止（漏らさない）。テナントは ctx から取り、WebSocket は NewServer で AddTransport する。何人まで・何タスク要るかは EXP-40 の計算機で（hub あり＝メモリ/fd 律速）。

## 適用範囲

- 純 Go（DB 不要）/ in-memory ローダで版を模す / gqlgen subscription リゾルバを直接呼ぶ
- WebSocket トランスポートは NewServer 側で AddTransport 済み。ここは hub 配線の意味論
- tenant は ctx（WithTenant）。subscription でも引数からは取らない

## 保証しない範囲・未検証

- 実配信は WebSocket（or graphql-sse）トランスポート越し。ここはリゾルバ→hub の配線を検証
- 接続数の上限はメモリ/fd（EXP-31/40）。fan-out は激安（EXP-38）
- 複数プロセスに購読者が分かれると poller はプロセスごと → プロセス跨ぎは pub/sub（EXP-38）

## 再利用できる成果物

- internal/gql: subscription リゾルバ（ssehub.Registry に相乗り）＋ WebSocket トランスポート配線
- docs/graphql.md / docs/sse-fan-in.md: gqlgen サブスクリプションと hub

## 次の実験

- なし

