# subscription/hub の設計（キャッシュ破棄・失権・状態×タスク）

hub でリアルタイムに状態を配るときの3つの設計判断を、実測つきでまとめる。
容量の前提は [docs/capacity.md](capacity.md)（1タスク SSE 1万〜1.5万本）、
数値の早見表は [docs/reference-numbers.md](reference-numbers.md)。

---

## 1. キャッシュ破棄の粒度（[EXP-52](../internal/hubcachelab) / [receipt](results/exp-52/exp-52-hub-cache-granularity.md)）

1 hub（1テナント）に複数ロボットがいる。1台の状態が変わったとき、hub の snapshot を
どう破棄・再取得するか。

| 方式 | 1回の poll で読む行 | N=2000・50poll・5台/poll の総読み |
| --- | --- | --- |
| テナント丸ごと引き直し（粗い） | 毎回 N 行 | **100,000 行（≒19.5MB）** |
| 版(ver)で差分だけ（細かい） | 変わった台数だけ | **250 行（≒49KB）・400x 少ない** |

- **破棄の合図は粗くてよい。再取得を細かくする**。書き込みで `ver` を単調に上げ（[EXP-47](event-ordering.md)）、
  poll は `WHERE tenant=? AND ver>:last ORDER BY ver`（索引レンジ）で**差分だけ**引く。
- 「テナント丸ごと再読」は変更が5台でも毎回 N 行を舐める＝台数に比例した無駄（メモリ・IO の実弾）。
- **購読者が何人いても poll は hub で共有**（1本）。hub が無いと購読者 S 人ぶんに膨らむ（[EXP-38](sse-fan-in.md)）。
- **まばらさが効く**：全台が毎 poll 変わるワークロードでは差分＝丸ごとになる。状態がまばらに変わる
  ほど差分が効く（＝IoT/ロボットの状態配信は典型的にまばら）。
- stale 対策は **イベント失効＋短め TTL**、期限切れ一斉読みは **singleflight**（[EXP-46](cache.md)）。
  跨プロセスなら無効化を **pub/sub** で配る（[EXP-43](sse-fan-in.md)）。

## 2. 長寿命接続の途中失権（[EXP-53](../internal/revauthlab) / [receipt](results/exp-53/exp-53-mid-stream-revocation.md)）

SSE/subscription は何時間も生きる。**接続時に一度だけ認可すると、途中で権限が剥奪されても
配信が続く**（情報漏洩）。

| 方式 | 剥奪後に配信した件数（漏洩） | 停止までの遅れ |
| --- | --- | --- |
| 接続時だけ認可（re-auth なし） | **595 件（残り全部）** | 止まらない |
| 定期 re-auth（50 event ごと） | **45 件（< 50）** | 45 |
| 定期 re-auth（10 event ごと） | **5 件（< 10）** | 5 |

- **配信ループの中で定期 re-auth する**（grant 表の再読・トークン期限チェック。[EXP-13](credential-rotation.md)/[EXP-28](security-layers.md)）。
  剥奪から **高々「re-auth 粒度」ぶん**で止まる。細かくするほど速いが認可コストが増える（trade-off）。
- 剥奪前は両者とも正常配信＝**可用性は壊さない**。
- **即時切断が要るなら push 型**：失権イベントを pub/sub で配って即座に切る（[EXP-43](sse-fan-in.md)）。
- **最大接続寿命（強制再接続）を併用**すると、re-auth が漏れても上限で必ず切れる（保険）。

## 3. ロボットの状態とタスクの状態、両方を1つの subscription で返すか

**あり。ただし条件つき**（設計判断。裏づけは [EXP-41](graphql.md)/[EXP-47](event-ordering.md)/[EXP-14](fanout.md)/[EXP-40](capacity.md)/[EXP-25](graphql.md)）。

- **1接続・型つき封筒（discriminated union）で両方**を返すのが基本。接続は増やさないほど容量が
  効く（SSE はメモリ・fd 律速・[EXP-40](capacity.md)）。GraphQL なら union 型 subscription が素直（[EXP-41](graphql.md)）。

  ```graphql
  type RobotState { robotId: ID!  online: Boolean!  battery: Int!  ver: Int! }
  type TaskState  { taskId: ID!  robotId: ID!  status: TaskStatus!  ver: Int! }
  union StateEvent = RobotState | TaskState
  type Subscription { states(tenant: ID!): StateEvent! }   # 1本で両方・client は型で絞る
  ```

- **2つは性質が違う**：ロボット状態＝高頻度テレメトリ（電池・位置）、タスク状態＝低頻度の離散遷移
  （queued→running→done）。混ぜるなら：
  - **種類ごとに別の `ver`** を持つ（1つのカウンタを共有しない）。順序・重複耐性は種類単位で（[EXP-47](event-ordering.md)）。
  - **高頻度側は coalesce**（最新スナップショットを配る・毎ティック配らない・[EXP-14](fanout.md)/[EXP-46](cache.md)）。
    さもないとタスクイベントがテレメトリに埋もれる。
  - **認可はフィールド/種類単位**（見せてよい列だけ push・[EXP-25](graphql.md)）。
- **やってはいけない**：2種類を1つの `ver` に混ぜる／テレメトリを間引かず全ティック push／認可を分けない。

> まとめ: 「1本の subscription・型つき封筒・種類ごとに ver・高頻度は coalesce・認可はフィールド単位」
> なら両方返して良い。分けたくなるのは、購読者の関心がはっきり二分される（タスク管理画面は状態を
> 要らない）ときや、テレメトリが極端に高頻度でチャンネルを分離したいとき。

## 保証しない範囲・未検証

- EXP-52 の版は単調増加前提（採番は書き込み側）。全台が毎回変わるなら差分の利点は消える。
- EXP-53 は event 数を粒度にしたモデル。実運用は時間 tick か版境界で持つ（認可コストは接続数×頻度）。
- 状態×タスクの購読は設計方針（本ページ）で、追加の実測は必須にしていない（[EXP-41/47/14/40/25] が裏づけ）。
