# EXP-3 graceful shutdown — 何を、どの順で止めるか

再現:

```bash
eval "$(./scripts/mysql-up.sh --export)"
go test ./internal/shutdownlab/ -run TestEXP3 -v -timeout 20m
```

結果ファイル: [`docs/results/exp-3/20260903-132203-graceful-shutdown.md`](results/exp-3/20260903-132203-graceful-shutdown.md)

---

## 1. 止める順番

```
1. 受付を止める        （pull を止める。処理はまだ止めない）
2. 手元のものを出し切る（有界キューを空にし、バッチを commit する）
3. commit してから ack する
4. 出し切れなかったものをキューへ戻す（nack）。戻したことを記録する
5. lease を明け渡す
6. ticker・goroutine・接続を閉じる
7. 期限を超えたら、何が残っているかを出して終わる
```

**1 と 2 の順序が逆だと、処理中のものを抱えたまま入力を受け続ける。**
**3 の順序が逆だと（ack が先）、どんな shutdown 手順でも救えない。**

## 2. 結果

worker の10地点（idle / fetching / decoding / enqueue / batching / in_tx /
before_commit / after_commit / lease_renew / retry_sleep）で信号を受けた。

| 方式 | ack 済み消失 | 宙に浮いたまま | goroutine 残留 |
| --- | --- | --- | --- |
| 片付けてから終わる（10地点 × SIGTERM） | **0** | **0** | **0** |
| 片付けずに落ちる（即 exit） | 0 | **24 件** | — |
| commit の前に ack する | **5 件（消失）** | — | — |

信号の種類（SIGTERM / SIGINT / SIGHUP / SIGQUIT）は、同じ地点で同じ挙動になった
（すべて exit=0、消失 0）。

### 2.1 片付けずに落ちると何が起きるか

消失はしない。キューの可視性タイムアウトが切れれば再配達される。
問題は **その間ずっと進まない** こと。この実験では 24 件が宙に浮いた。
本番の可視性タイムアウトは分単位のことが多く、そのぶん遅延がそのまま乗る。

**「消えないから大丈夫」ではない。** 戻したのか、抱えたまま落ちたのかを区別できるように、
nack した件数をログに残す。

### 2.2 commit の前に ack すると救えない

ack と commit の間で落ちると、キューは「処理済み」と思っているのに DB には入っていない。
再配達もされない。**この順序だけは shutdown の作り方で救えない。**

## 3. 測定器が見つけた本当の漏れ

最初の測定では、**10地点すべてで goroutine が起動時 +2 のまま**終わっていた。
原因は worker のバグで、HTTP クライアントの keep-alive 接続を閉じていなかった
（接続1本につき読み書きの goroutine が2つ残る）。

```go
w.http.CloseIdleConnections()   // これを足したら起動時の水準に戻った
```

テストの判定は「終了時の goroutine 数 ≤ 起動時 +1」。
**この一行を消すと、テストは必ず落ちる。**

## 4. 期限を超えたとき

出し切れなかった件は、期限（`-drain-deadline`）を超えた時点で

```
EXPPOINT shutdown_deadline_exceeded pending=<件数>
```

を出してからキューへ戻す。**黙って落ちない。**
「終われなかった」ことと「何が残っていたか」は、次の起動で必要になる。

## 5. 適用範囲と未検証

**当てはまる範囲**

- MySQL 8.0 / 同一ホスト / キューは可視性タイムアウト 1.5 秒の代役
- worker は1プロセス。複数 worker の同時終了（ローリング更新）は未測定
- goroutine の残留は worker 自身が終了直前に数えた値で判定

**保証しないこと**

- **SIGKILL は対象外**（捕捉できない。EXP-1 で別途扱っている）
- **子プロセスを起動する worker の後始末は未実装・未検証**
- 期限を超える負荷での挙動は、期限内に終わる負荷でしか確認していない
- OS のプロセスツリー・ファイルディスクリプタの残留は測っていない
