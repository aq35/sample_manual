# EXP-33 接続が切られたときのプール自動回復と、実行中クエリの扱い

| | |
| --- | --- |
| Experiment | EXP-33 / db-failover-resilience |
| Starting SHA | `4bb77e776e25` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) プールのアイドル接続が DB 側で切られても、次のクエリで database/sql が新しい接続を張り直し、成功する（自前の再接続コードは要らない）。 2) 実行中のクエリが切られたら、それは error になり自動リトライされない。冪等な読みだけをアプリ側で retry する。 3) ConnMaxLifetime を短めにしておくと、フェイルオーバ後に古い接続を掴み続けない。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4bb77e776e25+dirty |
| Started / Ended | 2026-09-07T09:39:58Z / 2026-09-07T09:39:59Z |

## Results

### アイドル接続を切断 → 次のクエリで自動回復 — OK

| 数えたもの | 値 |
| --- | --- |
| errs_after | 0 |
| errs_before | 0 |
| killed | 6 |

- 切断 6 本の後、50 クエリ中エラー 0（自動で張り直す）

### 実行中のクエリを切断 → その1本は error（自動リトライ無し） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| errs_after_recover | 0 |
| inflight_failed | 1 |

- 実行中クエリ error: invalid connection / 以降の新クエリは回復

## Verdict

アイドル接続が切られても database/sql が自動で張り直すので、自前の再接続は要らない。必要なのは (a) ConnMaxLifetime を短めにして古い接続を掴み続けない、(b) 実行中に切られたクエリは自動リトライされないので、冪等な読み/操作(EXP-27)だけをアプリ側で安全に retry する、の2点。

## 適用範囲

- MySQL 8.0 / 被験プール5本・ConnMaxLifetime 30s / 切断は worker スレッドの KILL で模す
- 『アイドル切断→次で回復』は database/sql が ErrBadConn を検知して新接続で retry する挙動
- 『実行中切断→error』は送信済みクエリなので retry されない（当然）

## 保証しない範囲・未検証

- 実 DB のフェイルオーバは DNS 切替・read-only 昇格など、KILL より複雑（ここは接続断のみ）
- 回復に要する時間・一時的な error 本数は、切断本数・プール・タイミングで動く
- トランザクション中の切断はロールバックされる。途中まで書いた副作用は冪等(EXP-27)で吸収する
- ConnMaxLifetime はフェイルオーバ後の『古い接続を掴み続ける』を短時間に抑えるための保険

## 再利用できる成果物

- internal/resiliencelab: 接続断とプール自動回復・実行中クエリの扱い
- docs/db-resilience.md: DB 切断/フェイルオーバへの耐性と安全なリトライ

## 次の実験

- EXP-34 ノイジーネイバー（テナント公平性）

