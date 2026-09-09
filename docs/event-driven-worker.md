# イベント駆動 worker の適用範囲：event は doorbell、正しさは DB CAS（EXP-62）

SQS/EventBridge のようなイベントは **at-least-once・順序保証なし・重複あり**。だから
**「起こす・配る・周期」には最適だが、「決める（完了・lease・fence）」に使うと壊れる**。
結論：**イベントは "ドアベル（起こす信号）" として使い、"正しさの根拠" にしてはいけない。**
event は floor（poll/reconcile）の**上に載せる遅延最適化**であって、置換ではない。

実装は [internal/doorbelllab](../internal/doorbelllab)、receipt は
[docs/results/exp-62](results/exp-62/exp-62-worker-doorbell-vs-floor.md)。関連：
lease/fence [EXP-2](fencing.md)、ポーリング [EXP-12](scheduling.md)、backoff [EXP-17](adaptive-backoff.md)、
exactly-once [EXP-44](outbox.md)。

## 向く / 向かない

| イベント駆動に向く（doorbell / fan-out / schedule） | 向かない（DB CAS / lease / reconcile のまま） |
| --- | --- |
| 「仕事が READY」→ worker を起こす通知（tight poll の代替） | **完了の exactly-once**（重複配信で二重完了しない保証） |
| ジョブ配布（SQS ワークキュー・observer への fan-out） | **lease/fence の失効回収**（crash 時はイベントが来ない） |
| 「何か起きた」のルーティング（EventBridge） | **crash 復元**（DB からのみ復元） |
| 定期実行の tick（EventBridge Scheduler）・reconcile sweep 起動 | |

## 結果（EXP-62）

### ① doorbell + adaptive backoff vs tight 1秒ポーリング（論理60秒・仕事 2,5,30・30 のドアベルは欠落）

| 方式 | DB 接触 | 拾い漏れ | 最大遅延 |
| --- | --- | --- | --- |
| tight poll（毎秒） | **61** | 0 | ≤1s |
| doorbell のみ（floor なし） | **2** | **1（永久に拾えない）** | — |
| **doorbell + backoff floor** | **13** | **0** | 6s |

- doorbell+backoff は **接触 61→13（約 8 割減）**。仕事があれば即起床（遅延0）、静かなときは指数的に間隔を伸ばす。
- **doorbell だけ（floor 無し）は、落ちたドアベルの仕事を永久に拾えない**（stuck=1）＝**floor が必須**。
  event は「見に行け」の合図で、拾い漏れは poll/reconcile の floor が保証する。

### ② 完了は DB CAS で exactly-once（重複/並行ドアベル 50）

| 方式 | 完了回数(complete_count) |
| --- | --- |
| **DB CAS**（`UPDATE ... SET done=1 WHERE id=? AND done=0`） | **1** |
| 素朴 read-then-write（読んでから書く） | **50（二重完了・事故）** |

- 50 個の重複ドアベルが一斉に完了を試みても、**CAS なら成功は1回だけ**（complete_count=1）。
- 素朴に「done を読む→0なら書く」を並行でやると **50 回全部が完了**してしまう。
  **exactly-once は配信でなく完了 CAS が持つ**、の実証。

### ③ crash 回収は event でなく lease 失効の reconcile

| タイミング | reconcile が拾う? |
| --- | --- |
| lease 失効**前** | いいえ（担当 A のまま） |
| lease 失効**後**（DB 時計） | **はい** → B が取り直して完了（complete_count=1） |

- 担当 worker がクラッシュすると **「イベントが来ない」**。だから **DB 時計の lease 期限＋reconcile
  sweep でしか回収できない**。ここをイベント依存にすると**復旧不能**になる（[EXP-2](fencing.md)）。

## 設計の指針

- **floor（下限）**：poll/reconcile で「pending があるか／lease が切れたか」を必ず定期確認。ここは消さない。
- **doorbell（最適化）**：SQS/EventBridge で「見に行け」を送り、tight poll を **doorbell+adaptive backoff** に置換。
  DB は依然 authority（メッセージは合図で、中身は DB から読む）。
- **決めるのは DB CAS**：完了は `... WHERE done=0`、lease は DB 時計 CAS、順序は version（[EXP-47](event-ordering.md)）。
- **役割分担**：SQS＝耐久ワークキュー（retry/DLQ・visibility timeout ≒ lease）、EventBridge＝ルーティング＋Scheduler(cron)。
  定番：EventBridge(schedule＋routing) → SQS(durable＋DLQ) → worker。
- **1秒ポーリングは消す前に測る**：小さいテーブルへの indexed 1文はほぼ無コスト。真のコストは
  常駐 compute・worker N 本の掛け算・埋め込み全件 json.loads（[concern-performance](concern-performance.md)）。
- **鮮度が命の問い（pending・lease 切れ）はキャッシュしない**（古い値で lease 判定＝clock skew の穴を自作）。
  キャッシュしていいのは frozen contract・descriptor など**不変・低頻度**のものだけ（[cache](cache.md)）。

## 保証しない範囲・未検証

- ①の論理秒・backoff cap は説明用。実運用は SQS 可視性タイムアウト≒lease、指数＋ジッタ（[EXP-17](adaptive-backoff.md)）。
- MySQL に LISTEN/NOTIFY は無い → 跨プロセス通知は SQS/EventBridge で代替（[sse-fan-in](sse-fan-in.md) の pub/sub と同じ姿勢）。
- **実 SQS/EventBridge** での failover 跨ぎ fence 単調性・重複/順序入替配信下の exactly-once は **LIVE_ENV_REQUIRED（未実測）**。
  実環境を1回通して測るまで「確定」とは書かない（[rds-proxy](rds-proxy.md) と同じ扱い）。
- crash 回収の速さは lease ttl と reconcile 周期で決まる（短いほど速いが誤失効の危険・clock skew は [EXP-2](fencing.md)）。
