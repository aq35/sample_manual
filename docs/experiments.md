# 実験基盤（`internal/expkit`）

このリポジトリの実験は、**「良さそうな設計を書く」ためではなく「事故を再現して、
止まることを確かめる」ため**にある。そのための測定器がこのパッケージ。

## 実験が満たすこと

1. **修正前に事故を再現できる**（故障注入で、狙った地点で壊せる）
2. **防止策で事故が止まる**（同じ workload・同じ注入で before / after）
3. **防止策を外すとテストが落ちる**（`Variant.Accident` を持つ方式を必ず1つ残す）
4. **測定条件が保存される**（`Env`: Go / OS / CPU / MySQL 変数 / **実行 SHA**）
5. **適用範囲と未保証範囲が書かれる**（`Scope` / `Uncertainty`）
6. **他プロジェクトへ持ち込める**（`expkit` はこのリポジトリ固有の型に依存しない）

## 構成

| ファイル | 役割 |
| --- | --- |
| `env.go` | 測定条件の採取（Go・CPU・MySQL 変数・git SHA・作業ツリーの汚れ） |
| `latency.go` | 所要時間の記録と p50/p95/p99 |
| `sample.go` | goroutine 数・ヒープ・RSS・DB プールの時系列サンプリング |
| `recorder.go` | 結果の組み立てと保存（JSON + Markdown） |
| `killpoint.go` | **子プロセス側**の故障注入（名前つき地点で自分を SIGKILL / 一時停止） |
| `child.go` | **親（テスト）側**から子プロセスを起動・待ち合わせ・信号送出・終了判定 |

## 故障注入の考え方

外から時間を見計らって `kill` すると、実行ごとに落ちる場所が変わって再現しない。
そこで **プロセス自身が「この地点で死ぬ」と決める**。

```go
ks := expkit.NewKillSwitch()   // EXP_KILL_AT / EXP_PAUSE_AT を読む
...
ks.Point("after_external_effect")  // ここが指定されていれば自分に SIGKILL
```

- `EXP_KILL_AT=<地点>` … その地点で **SIGKILL**（defer も回復処理も走らない）
- `EXP_PAUSE_AT=<地点>` … その地点で止まり、親に印を出す（正確な位置へ SIGTERM を送るため）
- どの地点も通過時に `EXPPOINT <名前>` を標準出力へ出すので、**どこまで進んで死んだか**が残る

親側:

```go
bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/xxx")
c, _ := expkit.StartChild(ctx, bin, args, expkit.KillPointEnv+"=after_external_effect")
info, _ := c.Wait(30 * time.Second)   // info.Killed / info.Signal / info.Points
```

SIGKILL は捕捉できないので、自分で送っても外から送っても同じ。
一方 **graceful shutdown の実験（SIGTERM など）は捕捉されるので、
`EXP_PAUSE_AT` で正確な地点に止めてから親が送る**。

## 結果の保存

`docs/results/<unit>/<timestamp>-<name>.json` と `.md` の2つを書く。

- JSON は機械可読（後から比較・集計する）
- Markdown は報告フォーマット（Experiment / Starting SHA / Hypothesis / Environment /
  Workload / Failure injection / Results / Verdict / 適用範囲 / 未検証 / 再利用できる成果物）

**仮説は結果を見る前に固定する。** `Recorder.Freeze` を呼ぶ前に結果を足すと panic し、
`Freeze` を2回呼んでも panic する。数字が出てから仮説を書き換えることが、
構造的にできないようにしてある。

```go
r := expkit.NewRecorder("EXP-1", "outbox-crash", "外部effect途中のSIGKILL")
r.Env(expkit.CaptureEnv(ctx, db))
r.Freeze("naive retry は二重送信を出し、outbox + idempotency key は出さない")  // ★先に書く
r.Workload("requests", 200).Injection("kill_at", "after_external_effect")
r.Add(expkit.Variant{Name: "naive_retry", Accident: true, Counters: ...})
r.Add(expkit.Variant{Name: "outbox", Counters: ...})
r.Scope("MySQL 8.0 / 単一プロセス / 同一ホスト")
r.Uncertain("レプリカ構成・ネットワーク分断は未検証")
files, _ := r.Save("...")
```

## 実験テストの直列化（踏んだ罠）

`go test` は **パッケージを並列に実行する**（既定 `-p` = CPU 数）。
実験テストはグローバル設定（`innodb_flush_log_at_trx_commit`、`wait_timeout`）を変え、
同じ名前の実験用テーブルを作っては消すので、**並列に走ると互いを壊す**。

実際に踏んだ壊れ方:

- 別パッケージのテストが実験用テーブルを消し、`Table 'exp_uuid' doesn't exist` で落ちた
- 中断された実行が `wait_timeout=2` を残し、後続の実行が
  「接続が切られた（Error 4031）」で落ちた

**落ち方が実行のたびに変わるので、実装のバグに見えてしまう。** 対策は2つ。

1. `mysqltest.Serialize(t)` — MySQL を使う実験テストの先頭で呼ぶ。
   `GET_LOCK` で1つずつ実行する（接続を固定して取る。`docs/locking.md` 3.1）
2. `internal/mysqlfacts` の `TestMain` — 前回の残骸を戻してから始める
   （グローバル設定を変える実験は、開始時に既知の状態へ戻す）

## 測定器を疑う

`internal/expkit/expkit_test.go` は測定器そのものの検査。

- 分位点が既知の入力に対して正しいか
- 仮説の後出しができないか
- サンプラーが goroutine の山を捉えられるか
- 子プロセスが**指定地点で本当に SIGKILL され、その先へ進んでいないか**
- 指定地点で止めて信号を送れるか

**矛盾する数字が出たら、実装を説明する前にここを疑う。**

## 実験の単位

| Unit | 内容 | 状態 |
| --- | --- | --- |
| EXP-0 | 測定器の自己検査 | 済（`internal/expkit`） |
| EXP-1 | 外部 effect・crash・`OUTCOME_UNKNOWN` | 済（[docs/crash-effects.md](crash-effects.md)） |
| EXP-2 | lease / fencing / clock skew | 済（[docs/fencing.md](fencing.md)） |
| EXP-3 | graceful shutdown | 済（[docs/shutdown.md](shutdown.md)） |
| EXP-4 | backpressure・過負荷 | 済（[docs/backpressure.md](backpressure.md)） |
| EXP-5 | connection pool 飽和・RDS Proxy | 済（[docs/pool-saturation.md](pool-saturation.md)）／ Proxy は [rds-proxy.md](rds-proxy.md)（**LIVE_ENV_REQUIRED**） |
| EXP-6 | migration crash matrix | 済（[docs/migration-crash.md](migration-crash.md)） |
| EXP-7 | query plan・データ偏り | 済（[docs/query-plan-skew.md](query-plan-skew.md)） |
| EXP-8 | SQL guard の fuzz | — |
| EXP-9 | `go/analysis` による保守性検査 | — |
| EXP-10 | SQLite companion | — |
| EXP-11 | backup / restore / corruption | — |
