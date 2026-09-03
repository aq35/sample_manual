# EXP-2 止まった worker・ずれた時計・同時 claim のもとで、古い担当の書き込みを止められるか

| | |
| --- | --- |
| Experiment | EXP-2 / lease-fencing-clock-skew |
| Starting SHA | `29c844e064c2` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) lease だけでは、停止していた worker が再開したときの書き込みを止められない。 2) fence（担当が変わるたびに増える番号）を書き込み条件に入れると止まる。 3) 期限の判定を各プロセスの時計で行うと、時計が進んだプロセスが生きた lease を奪える。    DB の時計で判定すれば奪えない。 4) 同時に claim したとき、勝者は1つになる（lease 方式のいずれでも）。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=29c844e064c2+dirty |
| Started / Ended | 2026-09-03T13:19:53Z / 2026-09-03T13:20:05Z |

## Workload

- `modes` = [no_lease lease_local_clock lease_db_clock lease_db_clock_fencing]
- `ttl` = 1.5s

## Failure injection

- `clock_skew` = worker の自己申告時刻を +60s ずらす
- `concurrent_claim` = 8 プロセス同時起動
- `pause` = SIGUSR1 待ちで worker を書き込み直前に停止（GC の長い停止・OS による停止の相当）

## Results

### 停止していた worker の再開 / lease_db_clock — **事故あり**

A を書き込み直前で止め、lease 切れ後に B が担当。そのあと A を再開させる

| 数えたもの | 値 |
| --- | --- |
| accepted_writes_by_stale | 1 |
| fence_after_takeover | 2 |
| fence_before | 1 |
| final_state_fence | 1 |
| final_state_value | 2 |

- 担当: A(fence=1) → B(fence=2)
- 最後に書いたのは A（fence=1）
- A の終了: exit=3 duration=105ms / B の終了: exit=0 duration=217ms
- ★lease を失っていても書けてしまう。停止と再開の間に担当が変わったことを、書き込み側は知らない

### 停止していた worker の再開 / lease_db_clock_fencing — OK

A を書き込み直前で止め、lease 切れ後に B が担当。そのあと A を再開させる

| 数えたもの | 値 |
| --- | --- |
| accepted_writes_by_stale | 0 |
| fence_after_takeover | 2 |
| fence_before | 1 |
| final_state_fence | 2 |
| final_state_value | 3 |

- 担当: A(fence=1) → B(fence=2)
- 最後に書いたのは B（fence=2）
- A の終了: exit=3 duration=104ms / B の終了: exit=0 duration=220ms
- 古い fence の書き込みは DB 側で拒否された（WHERE fence <= ?）

### 時計が 60 秒進んだプロセスの割り込み / lease_local_clock — **事故あり**

A が正常に担当を保持している間に、時計がずれた B が担当を取りに来る

| 数えたもの | 値 |
| --- | --- |
| accepted_writes_by_skewed | 1 |
| stolen | 1 |

- A の終了: exit=3 duration=196ms / B の終了: exit=0 duration=110ms
- ★期限を自分の時計で判定すると、時計がずれたプロセスが生きた lease を奪える

### 時計が 60 秒進んだプロセスの割り込み / lease_db_clock — OK

A が正常に担当を保持している間に、時計がずれた B が担当を取りに来る

| 数えたもの | 値 |
| --- | --- |
| accepted_writes_by_skewed | 0 |
| stolen | 0 |

- A の終了: exit=0 duration=801ms / B の終了: exit=4 duration=1.029s
- 期限の判定を DB の時計に寄せると、プロセスの時計がずれても奪えない

### 8 プロセス同時 claim / no_lease — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| winners | 8 |

- 担当を取れたのは [W7 W2 W0 W3 W1 W6 W5 W4]
- ★担当を決めなければ全員が書ける（二重稼働）

### 8 プロセス同時 claim / lease_db_clock — OK

| 数えたもの | 値 |
| --- | --- |
| winners | 1 |

- 担当を取れたのは [W1]

### 8 プロセス同時 claim / lease_db_clock_fencing — OK

| 数えたもの | 値 |
| --- | --- |
| winners | 1 |

- 担当を取れたのは [W7]

## Verdict

lease の期限判定を DB の時計に寄せると、時計がずれたプロセスによる乗っ取りは止まる。しかし『停止していた worker が再開して書く』のは lease だけでは止まらない。書き込み条件に fence を入れて初めて、古い担当の書き込みが DB 側で拒否された。

## 適用範囲

- MySQL 8.0 / 単一インスタンス / 同一ホストの複数プロセス
- 停止は SIGUSR1 待ちで再現（SIGSTOP と同じく、その間プロセスは何もしない）
- fence は DB の1行で単調増加させている（外部の採番サービスは使っていない）

## 保証しない範囲・未検証

- ネットワーク分断（DB へは届くが worker 間では見えない）は未再現
- DB のフェイルオーバーで fence 行が巻き戻る場合は未検証
- NTP による時刻の飛び（時計が後ろ向きに跳ぶ）は未再現
- GET_LOCK 方式は担当の表現としては測っていない（接続占有と期限なしの問題は docs/locking.md）

## 再利用できる成果物

- internal/fencelab: 4方式（lease 無し / 自己時計 / DB 時計 / DB 時計+fence）の実装
- cmd/fencelab: 書き込み直前で止まり、SIGUSR1 で再開する worker
- fence_audit 表: 受理・拒否の両方を残すので、事後に「誰の書き込みが通ったか」を数えられる

## 次の実験

- EXP-3 graceful shutdown: 担当を持ったまま終了するときの手順

