# EXP-38 同一テナントで多数が SSE 購読するときの DB ファンインとアプリ側の対応

| | |
| --- | --- |
| Experiment | EXP-38 / sse-fan-in |
| Starting SHA | `256f744385c5` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 300 接続が各自ポーリングすると、更新1回につき DB 読みが 300 回（ファンイン）。 2) テナントに1つの poller が1回引き hub で配ると、更新1回につき DB 読みは 1 回。 3) 一斉接続の初期スナップショットは singleflight で畳める（同時 in-flight を1つに。300 同時→ごく少数回）。 4) 遅い購読者は有界バッファで落とし、他 299 人の配信をブロックしない。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=256f744385c5+dirty |
| Started / Ended | 2026-09-07T10:28:45Z / 2026-09-07T10:28:47Z |

## Results

### 素朴: 300 接続が各自ポーリング（更新10回） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| db_reads | 3000 |

- 更新10回 × 300接続 = 3000 回の DB 読み（同じテナントに集中）

### hub: テナントに1つの poller＋fan-out（更新10回） — OK

| 数えたもの | 値 |
| --- | --- |
| db_reads | 10 |
| min_received | 10 |

- 素朴 3000 回 → hub 10 回（更新回数と同じ）。全員が受信

### stampede: 300 が一斉に初期取得 → singleflight でごく少数の DB 読みに畳む — OK

| 数えたもの | 値 |
| --- | --- |
| db_reads | 2 |

- 300 同時要求が 2 回の DB 読みに畳まれた（in-flight が重なったぶんを1つに）

### 遅い購読者: 有界バッファで落とし、速い購読者は全部受け取る — OK

| 数えたもの | 値 |
| --- | --- |
| dropped_total | 96 |
| fast_received | 100 |

- 速い購読者は 100/100 受信、遅い方は落とされた（96 件）。全体は止まらない

### hub 上限: fan-out コスト（購読者 300・in-memory） — OK

| 測ったもの | 値 |
| --- | --- |
| broadcasts_per_sec | 168419.540 |
| per_broadcast_us | 5.938 |
| per_sub_ns | 19.792 |

- 1配信 5.9µs（1購読者あたり 19.7ns）

### hub 上限: fan-out コスト（購読者 1000・in-memory） — OK

| 測ったもの | 値 |
| --- | --- |
| broadcasts_per_sec | 31155.193 |
| per_broadcast_us | 32.097 |
| per_sub_ns | 32.097 |

- 1配信 32.0µs（1購読者あたり 32.0ns）

### hub 上限: fan-out コスト（購読者 5000・in-memory） — OK

| 測ったもの | 値 |
| --- | --- |
| broadcasts_per_sec | 8553.481 |
| per_broadcast_us | 116.911 |
| per_sub_ns | 23.382 |

- 1配信 116.9µs（1購読者あたり 23.3ns）

## Verdict

同一テナントで多数が購読するとき、接続ごとに DB を引かせない。テナントに1つの poller が1回引き、in-memory hub で全員へ配る（300→1）。一斉接続の初期取得は singleflight で1回に畳む。遅い購読者は有界バッファで落として全体を止めない（最新版はまた来るので、差分でなく版/スナップショットを配る設計にする）。SSE 接続に DB 接続を1:1で持たせない（EXP-31）。

## 適用範囲

- MySQL 8.0 / 同一テナント 300 購読者・更新10回 / hub は in-memory fan-out・有界バッファ
- DB 読み回数を数える（Load を呼ぶたび +1）。hub は poller 1本ぶんだけ引く
- singleflight は golang.org/x/sync。stampede は 300 goroutine 同時要求で再現

## 保証しない範囲・未検証

- 実運用の SSE は接続ごとに goroutine とバッファを持つ（メモリは EXP-31）。ここは DB ファンインに集中
- 更新の押し出し方（毎回 push か coalesce か）は頻度次第。高頻度なら間引き/最新版のみ配る
- 複数プロセスに購読者が分かれると、各プロセスに poller が要る（プロセス数ぶんの DB 読み）。それでも接続数ではなくプロセス数で頭打ち。プロセス跨ぎの通知は pub/sub が要る（MySQL に LISTEN/NOTIFY は無い）
- 落とした更新は『最新版がまた来る』前提の設計（差分でなく版/スナップショット配信）

## 再利用できる成果物

- internal/ssehub: テナント単位 poller＋fan-out hub・singleflight・有界バッファ
- docs/sse-fan-in.md: 多数購読時の DB ファンイン対策

## 次の実験

- なし

