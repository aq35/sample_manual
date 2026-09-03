# EXP-3 worker の各段階で終了信号を受けたときの、ack 済み消失・滞留・後始末

| | |
| --- | --- |
| Experiment | EXP-3 / graceful-shutdown |
| Starting SHA | `042cffb620a1` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) 片付けてから終わる worker は、どの段階で SIGTERM を受けても ack 済み消失を出さず、    未処理をキューへ戻す。 2) 片付けずに落ちる worker は、消失は出さない（キューの可視性タイムアウトが救う）が、    取り出したものが宙に浮き、次に触れるまで遅延する。 3) commit の前に ack する実装は、そこで落ちると **本当に消失する**。 4) 片付けて終わった場合、goroutine は起動時の水準に戻る。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=042cffb620a1+dirty |
| Started / Ended | 2026-09-03T12:07:18Z / 2026-09-03T12:07:21Z |

## Workload

- `batch` = 5
- `items` = 40
- `queue_visibility` = 1.5s

## Failure injection

- `method` = 指定地点で worker を止め、その位置で信号を送る（EXP_PAUSE_AT）
- `signal_points` = [idle fetching decoding enqueue batching in_tx before_commit after_commit lease_renew retry_sleep]
- `signals` = [SIGTERM SIGINT SIGHUP SIGQUIT]

## Results

### 片付けてから終わる / SIGTERM を全10地点で — OK

受付を先に止め、手元のバッチを commit し、未処理をキューへ戻してから終了する

| 数えたもの | 値 |
| --- | --- |
| acked_but_not_committed | 0 |
| left_inflight | 0 |
| points | 10 |
| runs_with_goroutine_leak | 0 |

- idle           exit=0 commit=0 ack=0 戻した=4 消失=0 goroutine=2(起動時 2)
- fetching       exit=0 commit=0 ack=0 戻した=4 消失=0 goroutine=2(起動時 2)
- decoding       exit=0 commit=0 ack=0 戻した=4 消失=0 goroutine=2(起動時 2)
- enqueue        exit=0 commit=0 ack=0 戻した=4 消失=0 goroutine=2(起動時 2)
- batching       exit=0 commit=21 ack=21 戻した=3 消失=0 goroutine=2(起動時 2)
- in_tx          exit=0 commit=21 ack=21 戻した=3 消失=0 goroutine=2(起動時 2)
- before_commit  exit=0 commit=21 ack=21 戻した=3 消失=0 goroutine=2(起動時 2)
- after_commit   exit=0 commit=21 ack=21 戻した=3 消失=0 goroutine=2(起動時 2)
- lease_renew    exit=0 commit=40 ack=40 戻した=0 消失=0 goroutine=2(起動時 2)
- retry_sleep    exit=0 commit=0 ack=0 戻した=0 消失=0 goroutine=2(起動時 2)

### 4種類の信号を同じ地点（トランザクション中）で受ける — OK

| 数えたもの | 値 |
| --- | --- |
| abnormal_exit | 0 |
| acked_but_not_committed | 0 |

- SIGTERM: exit=0 消失=0 戻した=3 goroutine=2(起動時 2)
- SIGINT: exit=0 消失=0 戻した=3 goroutine=2(起動時 2)
- SIGHUP: exit=0 消失=0 戻した=3 goroutine=2(起動時 2)
- SIGQUIT: exit=0 消失=0 戻した=3 goroutine=2(起動時 2)

### 片付けずに落ちる（signal で即 exit） — **事故あり**

取り出したものを戻さずに終了する。キューの可視性タイムアウトが切れるまで誰も触れない

| 数えたもの | 値 |
| --- | --- |
| acked_but_not_committed | 0 |
| committed | 0 |
| left_inflight | 24 |

- 終了 exit=1 killed=false
- 消失はしないが、宙に浮いた件は可視性タイムアウト（この実験では 1.5 秒）まで進まない
- 本番の可視性タイムアウトは分単位のことが多く、そのぶん遅延する

### commit の前に ack する — **事故あり**

ack してから書き込む実装。ack と commit の間で落ちると取り戻せない

| 数えたもの | 値 |
| --- | --- |
| acked_but_not_committed | 5 |
| committed | 0 |

- ★キューは『処理済み』と思っているのに、DB には入っていない。再配達もされない
- ack は commit のあと。順序を逆にすると、どんな shutdown 手順でも救えない

## Verdict

片付けてから終わる worker は、10地点すべてで ack 済み消失を出さず、未処理をキューへ戻し、goroutine も起動時の水準に戻った。片付けずに落ちると消失こそしないが、取り出した件が可視性タイムアウトまで滞留する。commit の前に ack する実装だけは、どんな shutdown 手順でも救えない（実際に消失した）。

## 適用範囲

- MySQL 8.0 / 同一ホスト / キューは同一マシンの HTTP サーバ（可視性タイムアウト 1.5 秒）
- worker は 1 プロセス。複数 worker の同時終了は未測定
- goroutine の残留は worker 自身が終了直前に数えた値で判定している

## 保証しない範囲・未検証

- SIGKILL（捕捉できない終了）は EXP-1 で扱っており、ここでは対象外
- 子プロセスを持つ worker（外部コマンド起動）の後始末は未実装・未検証
- shutdown 期限を超えた場合の挙動は、期限内に終わる負荷でしか確認していない
- OS のプロセスツリーやファイルディスクリプタの残留は測っていない

## 再利用できる成果物

- internal/queuesvc: pull / ack / nack と可視性タイムアウトを持つキューの代役
- cmd/shutdownlab: 受付→有界キュー→バッチ→トランザクション→ack の worker と、地点指定の信号受信

## 次の実験

- EXP-4 backpressure: 入力が処理能力を超えたときのメモリと遅延

