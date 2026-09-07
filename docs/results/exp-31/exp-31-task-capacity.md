# EXP-31 1 vCPU / 2GB タスクの SSE 本数・Worker 処理量の見積もり

| | |
| --- | --- |
| Experiment | EXP-31 / task-capacity |
| Starting SHA | `7801e7b8f294` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) SSE は idle でも 1接続 = goroutine + バッファのメモリを食う。メモリ予算 ÷ 1接続 が上限の目安。 2) SSE は DB 接続を1:1で持たない（持てば接続予算ですぐ枯れる）。push は共有ハブから。 3) Worker は短い DB 往復の繰り返し。処理量 ≒ 並行度 × (1/往復遅延)。CPU でなく DB で頭打ち。 4) CPU 律速の rps はホスト依存。ここでは測らずモデルで扱う。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=7801e7b8f294+dirty |
| Started / Ended | 2026-09-07T08:39:06Z / 2026-09-07T08:39:11Z |

## Workload

- `sample_conns` = 20000

## Results

### 1接続メモリ: goroutine のみ（下限） — OK

| 測ったもの | 値 |
| --- | --- |
| bytes_per_conn | 2670.002 |
| kib_per_conn | 2.607 |

### 1接続メモリ: + 8KB バッファ — OK

| 測ったもの | 値 |
| --- | --- |
| bytes_per_conn | 10455.132 |
| kib_per_conn | 10.210 |

### 1接続メモリ: + 32KB バッファ（TLS 現実的） — OK

| 測ったもの | 値 |
| --- | --- |
| bytes_per_conn | 34961.745 |
| kib_per_conn | 34.142 |

### 見積もり: SSE 本数（接続予算 1.2GB ÷ 1接続） — OK

| 数えたもの | 値 |
| --- | --- |
| sse_bare_floor | 471269 |
| sse_tls_realistic | 35990 |

- 下限バッファなら約 471269 本、TLS 現実的なら約 35990 本
- 実運用は fd 上限(ulimit)・GC 余白・安全率で、この 1/2〜1/3 を上限に設定するのが無難

### Worker 往復/秒: 並行度 1 — OK

| 数えたもの | 値 |
| --- | --- |
| concurrency | 1 |
| ops_per_sec | 11617 |

### Worker 往復/秒: 並行度 4 — OK

| 数えたもの | 値 |
| --- | --- |
| concurrency | 4 |
| ops_per_sec | 28207 |

### Worker 往復/秒: 並行度 16 — OK

| 数えたもの | 値 |
| --- | --- |
| concurrency | 16 |
| ops_per_sec | 44851 |

### Worker 往復/秒: 並行度 32 — OK

| 数えたもの | 値 |
| --- | --- |
| concurrency | 32 |
| ops_per_sec | 52762 |

## Verdict

1タスクの上限は『CPU』より先に『メモリ（SSE 本数）』と『DB（Worker 往復）』で決まりやすい。SSE はメモリ律速: 予算 ÷ 1接続（TLS 込みで数万本の上限、安全率で 1/2〜1/3）。DB 接続は 1:1 で持たない。Worker は DB 律速: 並行度 × (1/往復遅延)。足りなければ縦に上げるより横に増やす（EXP-30）。

## 適用範囲

- Go1.25 / メモリは HeapAlloc+StackInuse の増分を実測 / 往復は同ホスト MySQL への SELECT 1
- 接続予算は 2GB - 約800MB(ランタイム/GC/アプリ) ≒ 1.2GB と仮定した見積もり
- 往復/秒はこのホストの DB のもの。本番 DB・ネットワークでは往復遅延が変わる

## 保証しない範囲・未検証

- CPU 律速の rps（JSON 整形・圧縮・暗号）は 1 vCPU の実機で測ること。ここでは扱わない
- SSE の実メモリは TLS 実装・書き込みバッファ・アプリの per-conn 状態で動く（±数十KB）
- 往復遅延は本番の RTT 次第。Worker 処理量 = 並行度 × (1/往復遅延) で往復遅延に強く依存
- 横に増やす（レプリカ）ときの上限は接続予算（EXP-5/poolbudget）と DB 自体（EXP-30）

## 再利用できる成果物

- internal/capacitylab: 1接続メモリと DB 往復スループットの実測
- docs/capacity.md: 1 vCPU/2GB タスクの容量見積もり（SSE 本数・Worker 処理量）

## 次の実験

- なし

