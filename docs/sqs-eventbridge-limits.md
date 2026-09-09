# EventBridge / SQS が苦手なこと（深掘り：別々に分解）

前提：[db-ecs-complete](db-ecs-complete.md) と [event-driven-worker](event-driven-worker.md)(EXP-62) で
「event は doorbell、正しさは DB CAS」を示した。ここでは **EventBridge と SQS を別物として**、
それぞれ何が苦手かを**機構レベルで**分解する。結論を先に：

- **EventBridge＝ルータ＋近似 cron**。苦手：*正確な時刻・順序・exactly-once・状態保持・不在検知・生 WS 宛先*。
  「起きたことを配る / 周期で叩く」専用。**保持（hold）しない・pull できない・approximate に発火**。
- **SQS＝耐久ワークキュー**。苦手：*業務的 exactly-once・fence・スケジュール・状態照会・単体 fan-out・長期履歴*。
  「貯める・retry・DLQ・背圧」専用。**visibility timeout は lease でなく timeout・dedup は 5 分窓だけ**。

共通の欠け（両方に無い DB の4性質）：**exactly-once（CAS）/ 不在検知（poll・reconcile）/ 厳密順序（version）/
現在状態の照会**。だが**欠け方が違う**ので分けて設計する。

> ローカル実証：重複配信での二重実行・失効回収・順序は [EXP-62](event-driven-worker.md)②③ / [EXP-2](fencing.md) /
> [EXP-47](event-ordering.md) / [EXP-44](outbox.md) が示す。**AWS 実サービスの発火遅延・重複率・failover 跨ぎの
> 単調性は LIVE_ENV_REQUIRED（未実測）**。数値は AWS 公式仕様ベース。

---

## A. EventBridge が苦手なこと（ルータ＋近似 cron としての限界）

EventBridge は「EventBus（pub/sub ルーティング）＋ Scheduler/Rules（cron）」。ターゲットは AWS サービスや
API destination。**push 型で、メッセージを保持して pull させる仕組みではない**。ここから限界が出る。

### A-1. 時刻ちょうどに必ず、が無い（approximate fire）
- **機構**：Rules/Scheduler は**近似発火**。最小粒度 **1分**（秒は無い）。負荷時に**分オーダで遅延**しうる。
  one-time schedule が**システム停止中に来たぶんを後から backfill しない**（rate/cron は次の回を撃つだけ）。
- **壊れ方**：「T に必ず 1 回」を EventBridge の発火そのもので保証しようとすると、遅延・取りこぼし・
  二重発火に直撃する。
- **DB 側で補う**：真実は DB の `run_at<=NOW()`。EventBridge は「見に行け」の hint に留め、**取りこぼしは
  poll/reconcile の floor が拾う**（[EXP-62](event-driven-worker.md)① doorbell だけだと stuck=1・floor 必須）。

### A-2. exactly-once が無い（at-least-once・重複発火）
- **機構**：配信は at-least-once。**同じイベントが複数回**ターゲットに届きうる。dedup 機構は無い。
- **壊れ方**：完了・課金・カウントをイベント受信回数で数えると**二重実行**（[EXP-62](event-driven-worker.md)②：
  素朴 read-then-write は 50 並行で 50 回完了）。
- **DB 側で補う**：完了は `UPDATE ... WHERE done=0` の CAS で成功1回に畳む（同②：CAS は complete_count=1）。

### A-3. 順序保証が無い
- **機構**：イベントは**順不同**で届きうる。
- **壊れ方**：「古い更新が新しい更新を上書き」。
- **DB 側で補う**：`version` で単調適用（[EXP-47](event-ordering.md)：降順は棄却）。

### A-4. 「今の状態」を持てない（通知であって状態ストアでない）
- **機構**：イベントは「起きた事実」の通知。**クエリできない・溜めて読めない・更新できない**。
- **壊れ方**：「今 pending は何件」「この robot の担当テナントは」をイベントから答えようとして破綻。
- **DB 側で補う**：状態は DB から読む／窓（from,to）から派生（[db-ecs-complete](db-ecs-complete.md)）。

### A-5. 不在を検知できない（crash・stuck）
- **機構**：プロデューサが**死ぬとイベントが来ない**。EventBridge は「来なかったこと」を教えない。
- **壊れ方**：`in_progress` のまま止まった行・担当ワーカーの crash を event 依存だと**永久に拾えない**。
- **DB 側で補う**：DB 時計の lease 失効＋reconcile sweep（[EXP-62](event-driven-worker.md)③：失効後に B が取り直し／
  [EXP-2](fencing.md)）。

### A-6. DB 書込と同時に発火を原子化できない
- **機構**：「行を書く」AND「イベントを publish」を**1トランザクションにできない**（publish 後に commit 失敗＝幽霊通知、
  commit 後に publish 失敗＝通知欠落）。
- **DB 側で補う**：transactional outbox（[EXP-44](outbox.md)）。DB に書くのは outbox 行、relay が後から publish。

### A-7. 生の WebSocket 接続へ直接 push できない
- **機構**：ターゲットは AWS サービス/API destination。**worker がメモリに握っている特定の WS 接続**は宛先にできない。
- **DB 側で補う**：自前 hub / pub-sub（[sse-fan-in](sse-fan-in.md)）。EventBridge は「hub を起こす」まで。

### A-8. データ本体の運搬・長期履歴に向かない
- **機構**：1イベント **256KB**。archive/replay はあるが replay は**再順序・再タイムスタンプ**の癖があり監査原本にならない。
- **DB 側で補う**：参照（id/version）だけ送り本体は DB から読む。履歴は日パーティション保持（[EXP-48](retention.md)）。

**EventBridge 一言**：*「何かが起きた→配る／周期で叩く」だけ任せ、「いつ・何回・どの順・今どう・落ちてないか」は
DB に残す。* 保持も pull も正確な時刻保証も無い、push 型ルータだから。

---

## B. SQS が苦手なこと（耐久ワークキューとしての限界）

SQS は「貯める・retry・DLQ・背圧」が強い耐久キュー。Standard（高スループット/順不同/重複あり）と
FIFO（グループ内順序/5分 dedup/スループット上限）がある。**強いぶん誤解されやすい点**を分解する。

### B-1. visibility timeout は lease であって fence ではない（最大の罠）
- **機構**：取り出したメッセージは visibility timeout（既定30s・最大12h）の間だけ隠れる。**処理が timeout を
  超えると再表示**され、別コンシューマが**同じメッセージを取る**。heartbeat で延長しないと二重処理。
- **壊れ方**：**遅いが生きている worker**＋**新しい worker**が同時に同じ仕事を進める＝split brain。
  SQS は**単調増加する fencing token を持たない**ので、古い worker の遅延書込を弾けない。
- **DB 側で補う**：**fence は DB**（lease に単調 token、書込時に `WHERE fence<=:token`・[EXP-2](fencing.md)）。
  visibility timeout ≒ lease ttl と揃えるが、正しさの最終判定は DB。

### B-2. exactly-once は「5分 dedup 窓」だけ（業務 exactly-once ではない）
- **機構**：Standard は重複あり。FIFO の exactly-once は**MessageDeduplicationId の 5 分窓**に限る。
- **壊れ方**：同じ論理ジョブを**5分超空けて**2回入れると**2回走る**。「同じ請求を二度やらない」を SQS dedup に
  委ねると窓の外で破れる。
- **DB 側で補う**：`UNIQUE(tenant_id, idem_key)`＋完了 CAS（[EXP-62](event-driven-worker.md)②）。exactly-once は
  **配信でなく DB が持つ**。

### B-3. スケジュール（run_at）が無い
- **機構**：持てるのは per-message delay **最大15分**か delay queue だけ。**任意未来時刻・cron は不可**。
- **壊れ方**：「9:00 に実行」を SQS 単体では表現できない。
- **DB 側で補う**：`run_at` 列＋poll（[scheduling](scheduling.md)）。定時の tick が要れば EventBridge Scheduler を前段に。

### B-4. 状態照会ができない（キューは状態ストアでない）
- **機構**：メッセージは**消費して初めて処理**。中身を**覗き見・更新・検索できない**（おおよその件数しか見えない）。
- **壊れ方**：「tenant X の保留は何件」「この仕事の今の状態」をキューから答えられない。
- **DB 側で補う**：状態は DB の `status`。キューは「やれ」の合図だけ運ぶ。

### B-5. 単体では fan-out できない
- **機構**：1メッセージは**1コンシューマが取って消す**。複数の購読者に同じものを配れない。
- **壊れ方**：「1イベントを worker と監査と通知へ」を SQS だけでやろうとすると取り合いになる。
- **補う**：前段に SNS / EventBridge（1→N で各 SQS へ複製）。SQS は各レーンの耐久受け皿。

### B-6. 長期履歴・監査・replay に向かない
- **機構**：保持**最大14日**（既定4日）。消えたら戻らない。replay 基盤ではない。
- **DB 側で補う**：監査・履歴は DB の日パーティション保持（[EXP-48](retention.md)）。

### B-7. データ本体の運搬に向かない
- **機構**：1メッセージ **256KB**（拡張クライアントで S3 ポインタ化すれば大きくできるが、その時の実体は S3）。
- **補う**：参照だけ送り本体は DB/S3 から読む。

### B-8. 不在検知はできない
- **機構**：キューが空 ≠「何も起きるべきでない」。プロデューサが enqueue 前に死ねばキューは空のまま。
- **DB 側で補う**：A-5 と同じ。lease 失効＋reconcile（[EXP-62](event-driven-worker.md)③）。

### B-9. FIFO の順序はグループ単位で、スループットとホットキーの制約
- **機構**：順序保証は **MessageGroupId 内だけ**。1グループは直列化＝**詰まった1件が同グループを止める**。
  FIFO スループットも Standard より上限が低い（バッチ/高スループットモードで緩和はする）。
- **壊れ方**：強い順序を1グループに集めると**並列度が死ぬ**（ホットパーティション）。
- **DB 側で補う**：順序は version（[EXP-47](event-ordering.md)）。SQS はグループを細かく割って並列を確保。

### B-10. poison message は DLQ 設定必須
- **機構**：`maxReceiveCount` ＋ DLQ を設定しないと、失敗メッセージが**無限リトライでループ**。
- **補う**：DLQ＋分類（[dead-letter](dead-letter.md)/[retry](retry.md)：恒久失敗は隔離・一時失敗は再試行）。

**SQS 一言**：*「貯めて・retry して・DLQ に逃がして・背圧をかける」までが仕事。*
「二度やらない・古い奴を弾く・いつ・今どう・落ちてないか」は DB に残す。visibility timeout を lease と
取り違えない。

---

## C. 並べて比較（どちらも "正しさ" は DB に戻る）

| 苦手なこと | EventBridge | SQS | DB 側の置き場所 |
| --- | --- | --- | --- |
| 業務 exactly-once | ✗ 重複発火 | ✗ dedup は5分窓だけ | 完了 CAS・UNIQUE（[EXP-62](event-driven-worker.md)②） |
| fence（古い worker 排除） | — | ✗ visibility=timeout≠fence | 単調 token・`WHERE fence<=:t`（[EXP-2](fencing.md)） |
| 厳密順序 | ✗ 順不同 | △ グループ内のみ/詰まる | version 単調適用（[EXP-47](event-ordering.md)） |
| 正確な時刻・run_at | △ 近似・1分・遅延 | ✗ delay 15分まで | DB `run_at<=NOW()`＋poll |
| 不在検知（crash/stuck） | ✗ 来ない | ✗ 空≠異常 | lease 失効＋reconcile（[EXP-62](event-driven-worker.md)③） |
| 現在状態の照会 | ✗ 通知のみ | ✗ 覗けない | DB `status`／窓から派生 |
| fan-out（1→N） | ○ 得意 | ✗ 単体不可（SNS前段） | — |
| 耐久バッファ/retry/DLQ/背圧 | △ retry/DLQ有 | ○ 得意 | — |
| DB 書込と原子発火 | ✗ | ✗ | transactional outbox（[EXP-44](outbox.md)） |
| 生 WS へ push | ✗ AWS宛先のみ | ✗ work queue | 自前 hub（[sse-fan-in](sse-fan-in.md)） |
| 長期履歴/監査/replay | ✗ archive癖 | ✗ 14日 | 日パーティション保持（[EXP-48](retention.md)） |
| データ本体運搬 | ✗ 256KB | ✗ 256KB | 参照だけ送り本体は DB/S3 |

定番の積み方：**EventBridge(schedule＋routing) → SQS(durable＋DLQ) → worker、その worker は DB CAS で決める**。
EventBridge は「いつ・どこへ」、SQS は「貯めて確実に渡す」、**正しさ（二度やらない・古いの弾く・順序・現在状態・
落ちたの拾う）は DB**。

## まとめ

- **EventBridge の弱点は "保持しない・pull できない・近似発火" という push 型ルータの本質**：時刻・順序・
  exactly-once・状態・不在検知・生WS を任せると壊れる。
- **SQS の弱点は "visibility=timeout≠fence・dedup=5分窓・状態を持てない"**：業務 exactly-once・fence・
  スケジュール・状態照会・単体 fan-out を任せると壊れる。
- **両者に共通して欠けるのは DB の4性質（exactly-once CAS / 不在検知 poll / 順序 version / 現在状態）**。
  だから **event は doorbell、正しさは DB**（[db-ecs-complete](db-ecs-complete.md)）。

## 保証しない範囲・未検証

- 数値（256KB・14日・5分 dedup・15分 delay・1分粒度・12h visibility）は AWS 公式仕様ベース。
  実発火遅延・重複率・failover 跨ぎの fence 単調性は **LIVE_ENV_REQUIRED（未実測）**。
- 二重実行・失効回収・順序・outbox の**機構**はローカルで実証済（[EXP-62](event-driven-worker.md)/[EXP-2](fencing.md)/
  [EXP-47](event-ordering.md)/[EXP-44](outbox.md)）。AWS を1回通して測るまで「確定」とは書かない（[rds-proxy](rds-proxy.md) と同姿勢）。
