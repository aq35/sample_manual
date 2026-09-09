# EXP-62 doorbell は起こす信号・正しさは DB CAS。floor(poll/reconcile)の上に event を載せる

| | |
| --- | --- |
| Experiment | EXP-62 / worker-doorbell-vs-floor |
| Starting SHA | `4288f85700e3` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | ① doorbell+backoff は tight poll より DB 接触が激減（仕事があれば即起床・遅延0）。 ただし event だけ(floor 無し)は、落ちたドアベルの仕事を永久に拾えない（floor が要る）。 ② 完了は DB CAS で exactly-once：重複/並行ドアベルでも complete_count=1。素朴 read-then-write は二重完了。 ③ crash 回収は event でなく lease 失効の reconcile：担当が死ぬとイベントは来ない。DB 時計の期限＋sweep で拾う。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4288f85700e3+dirty |
| Started / Ended | 2026-09-09T09:52:36Z / 2026-09-09T09:52:37Z |

## Results

### tight poll(毎秒): 拾い漏れ0だが DB 接触が多い — OK

| 数えたもの | 値 |
| --- | --- |
| db_touches | 61 |
| max_latency_s | 0 |
| stuck | 0 |

### doorbell のみ(floor なし): ドアベル欠落の仕事を永久に拾えない — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| db_touches | 2 |
| max_latency_s | 0 |
| stuck | 1 |

- 接触は最少だが stuck=1（落ちたドアベルの仕事が残る）

### doorbell + backoff floor: 接触激減・拾い漏れ0 — OK

| 数えたもの | 値 |
| --- | --- |
| db_touches | 13 |
| max_latency_s | 6 |
| stuck | 0 |

- tight 61→ 13 接触。落ちたドアベルも floor が拾う(最大遅延 6s)

### 完了 DB CAS: 重複/並行ドアベル 50 でも complete_count=1（exactly-once） — OK

| 数えたもの | 値 |
| --- | --- |
| cas_won | 1 |
| complete_count | 1 |
| wakes | 50 |

### 素朴 read-then-write: 並行で二重完了（事故） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| complete_count | 50 |
| wakes | 50 |

- complete_count=50（>1＝二重完了。配信でなく CAS が正しさを持つべき理由）

### crash 回収: lease 失効の reconcile でのみ拾える（event は来ない） — OK

| 数えたもの | 値 |
| --- | --- |
| a_claimed | 1 |
| b_completed | 1 |
| b_reclaimed | 1 |
| complete_count | 1 |
| reconcile_after_expiry | 1 |
| reconcile_before_expiry | 0 |

- 失効前 sweep=拾えない / 失効後 sweep=拾える → B が完了。complete_count=1

## Verdict

worker のイベント駆動は『doorbell（起こす信号）＋DB が authority』。① doorbell+adaptive backoff は tight poll より DB 接触が激減し、仕事があれば即起床（遅延0）。ただし event だけだと落ちたドアベルの 仕事を永久に拾えず、poll/reconcile の floor が要る。② 完了は DB CAS で exactly-once（重複/並行 ドアベル 50 でも complete_count=1）。素朴 read-then-write は二重完了する。③ crash 回収は event で なく DB 時計の lease 失効＋reconcile sweep でのみ成立（担当が死ぬとイベントは来ない）。＝event は floor の上に載せる遅延最適化であって、正しさ（完了・lease・fence）は DB CAS のまま。

## 適用範囲

- MySQL 8.0 / lease は DB 時計(NOW(3))・ttl=1s / ①は純ロジック 60秒・仕事 2,5,30・30 のドアベル欠落
- doorbell=起こす信号（poll のトリガ）、完了/lease は DB CAS。event は floor の上の遅延最適化
- ②complete_count=完了が走った回数（exactly-once なら 1）。③reconcile=失効 lease の sweep

## 保証しない範囲・未検証

- ①の論理秒・backoff cap は説明用。実運用は SQS 可視性タイムアウト≒lease、指数＋ジッタ（EXP-17）
- 跨プロセス通知は MySQL に LISTEN/NOTIFY が無いので SQS/EventBridge で代替（doorbell）。DB は依然 authority
- 実 SQS/EventBridge での failover 跨ぎ fence 単調性・重複/順序入替配信下の exactly-once は LIVE_ENV_REQUIRED（未実測）
- crash 回収の速さは lease ttl と reconcile 周期で決まる。短いほど速いが誤失効の危険（clock skew・EXP-2）

## 再利用できる成果物

- internal/doorbelllab: doorbell(poll トリガ) vs floor、完了 CAS、lease 失効 reconcile
- docs/event-driven-worker.md: イベント駆動の適用範囲（floor は poll/reconcile、event は doorbell）

## 次の実験

- （実 SQS/EventBridge は LIVE_ENV_REQUIRED）

