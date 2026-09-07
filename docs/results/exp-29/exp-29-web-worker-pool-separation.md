# EXP-29 Web と Worker で接続プールを共有 vs 分離したときの Worker レイテンシ

| | |
| --- | --- |
| Experiment | EXP-29 / web-worker-pool-separation |
| Starting SHA | `4107029732ff` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 総接続数を同じにしても、1プールを Web と Worker で共有すると、Web のバーストが接続を占有し、Worker の軽い問い合わせが acquire 待ちで p95 が跳ねる。 2) プールを役割ごとに分ける（Worker に専用の取り分）と、Worker は Web の影響を受けず p95 が低い。 3) 総接続数は変えていないので、効いているのは『分けたこと』そのもの。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4107029732ff+dirty |
| Started / Ended | 2026-09-07T08:20:45Z / 2026-09-07T08:20:53Z |

## Workload

- `total_conns` = 10
- `web_goroutines` = 16
- `web_sleep_ms` = 50

## Results

### 共有プール: Web と Worker が同じプール（Worker が待たされる） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| worker_p50_ms | 48.528 |
| worker_p95_ms | 99.911 |

- Web バーストが接続を占有し、Worker の SELECT 1 が acquire 待ちになる

### 分離プール: Worker 専用の取り分（Web の影響を受けない） — OK

| 測ったもの | 値 |
| --- | --- |
| worker_p50_ms | 0.253 |
| worker_p95_ms | 0.344 |

- 共有 p95 99.911145ms → 分離 p95 344.491µs（総接続数は同じ。効いているのは分けたこと）

## Verdict

Web と Worker は接続プールを分ける（総接続数が同じでも、共有すると Web バーストで Worker のdispatch 遅延が跳ねる）。役割ごとに取り分を持てば Worker は Web の影響を受けない。プロセス/コンテナを分ければ CPU・障害も隔離できる。接続予算の配分は poolbudget で。

## 適用範囲

- MySQL 8.0 / 総接続 10（Web 8・Worker 2）/ Web=SLEEP(50ms)×16並行 / Worker=SELECT 1×100
- レイテンシは Worker 側の『接続 acquire＋実行』。ここに待ちが乗る
- 生の *sql.DB を直接使う（プールの振る舞いを見るため。repo 層は経由しない）

## 保証しない範囲・未検証

- 絶対値はローカルのもの。Web の重さ・並行度・プール比で差は動く
- 別プロセス/別コンテナに分ければ CPU・メモリ・障害も隔離できる（ここはプールの隔離のみ）
- 読み取りをレプリカへ回す（EXP-20）とさらに primary の負担を分けられる

## 再利用できる成果物

- internal/procseplab: 共有/分離プールでの Worker レイテンシ測定
- docs/web-worker-split.md: Web と Worker の分離（接続予算は poolbudget）

## 次の実験

- EXP-30 冗長化（複数レプリカ）でアプリはどうあるべきか

