# EXP-66 Web と Worker のレースを状態 CAS と version（楽観ロック）で止める

| | |
| --- | --- |
| Experiment | EXP-66 / web-worker-race |
| Starting SHA | `d5beaeb87c79` |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 状態遷移は遷移元を WHERE に入れた CAS(affected_rows=1 で勝者確定)で守る。claim も complete も同じ。 入力の同時編集は version(楽観ロック)で守る。worker は読んだ version を WHERE に入れて書き、 Web が先に編集していたら affected_rows=0 で結果を捨てて読み直す。 CAS/version が無いと、二重 claim・cancel の消失・stale result が起きる。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=d5beaeb87c79 |
| Started / Ended | 2026-09-10T22:49:44Z / 2026-09-10T22:49:47Z |

## Results

### A claim: 無条件 UPDATE（読取り→書込みの隙間） — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| double_claims | 200.000 |
| total_claims | 1049.000 |

- 同じ pending を複数 worker が掴む＝二重処理。掴んだ総数が行数 200 を超える

### A claim: 状態 CAS（WHERE status='pending'・affected_rows=1） — OK

| 測ったもの | 値 |
| --- | --- |
| double_claims | 0.000 |
| total_claims | 200.000 |

- 1行は1人にしか渡らない。掴んだ総数=行数ちょうど・二重0

### B complete: 状態を見ない UPDATE ... WHERE id — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| lost_cancels | 200.000 |

- Web の cancel を worker の完了が上書き。200 件の cancel が消えて completed に

### B complete: 状態 CAS（WHERE status='in_progress' AND owner） — OK

| 測ったもの | 値 |
| --- | --- |
| lost_cancels | 0.000 |

- cancel 済みには一致しない(affected_rows=0)ので worker は結果を破棄。消えた cancel 0

### C 結果書込: version を見ない UPDATE ... WHERE id — **事故あり**

| 測ったもの | 値 |
| --- | --- |
| stale_writes | 200.000 |

- Web が編集した後の行に、古い input で計算した結果を書く。200 件が stale

### C 結果書込: version 楽観ロック（WHERE version=読んだ値） — OK

| 測ったもの | 値 |
| --- | --- |
| stale_writes | 0.000 |

- version 不一致で affected_rows=0 → 結果を捨てて読み直す。stale 0

## Verdict

Web と Worker が同じ行を触るなら、状態遷移は遷移元を WHERE に入れた CAS(affected_rows で勝者確定)、入力の同時編集は version(楽観ロック)で守る。claim も complete も『状態を見ずに id だけで UPDATE』は禁止。CAS/version が無いと、二重 claim・cancel の消失・stale result が起きる。

## 適用範囲

- MySQL 8.0 / race_job 200件 / claim は concurrency=8 で奪い合い
- A は並行(goroutine)、B・C は Web→Worker の順を決定的に起こして上書き/stale を測る
- 状態 CAS=遷移元を WHERE に入れる、version=読んだ値を WHERE に入れる

## 保証しない範囲・未検証

- A の無条件版の二重数は並行タイミング依存（環境で変わる）。CAS の不変量(二重0・total=n)だけが確定的
- SELECT ... FOR UPDATE SKIP LOCKED でも claim は守れる（別解）。ここでは楽観 CAS を測った
- version は競合が激しいとやり直しが増える（repository-layer §2.3・楽観ロックはタダではない）

## 再利用できる成果物

- internal/racelab: ClaimRace(A) / CancelThenComplete(B) / StaleInputRace(C)
- docs/web-worker-race.md: Web と Worker のレースコンディション対策

## 次の実験

- なし

