# 用語集（この repo の言葉を噛み砕く）

各 md に出てくる用語を、**日本語（英語）— 一言で。たとえ／どこで使うか**の形でまとめた。
「これ何？」となったらここに戻る。

---

## 速さ・容量の言葉

- **律速（りっそく）（rate-limiting step / bottleneck）** — **一番遅い所が全体の速さを決める**こと。
  たとえ: 3車線が1車線に減る所で渋滞する、その1車線が「律速」。Worker は DB 往復が律速、Web は CPU が律速。
- **スループット（throughput）** — **単位時間に捌ける量**（件/秒・rps）。「1秒で何件こなせるか」。
- **レイテンシ（latency）** — **1件にかかる時間**（応答時間）。スループットが「量」なら、こちらは「速さ」。
- **p50 / p95 / p99（percentile）** — 遅い順に並べたとき **95% がこの時間内に収まる**、の意味（p95）。
  平均でなく p95 を見るのは「たまに遅い」を捉えるため。
- **rps（requests per second）** — 1秒あたりのリクエスト数。Web の処理量の単位。
- **往復 / RTT（round-trip time）** — アプリ ⇄ DB の**1回のやり取りにかかる時間**。近い DB で ~0.1ms、本番だと ~0.5ms 等。
- **vCPU** — 仮想 CPU **1個ぶん**の処理能力。「1 vCPU = 1秒に1,000ミリ秒ぶんの計算枠」。
- **OOM（out of memory）** — メモリを使い切って**プロセスが強制終了**されること。

## DB・クエリの言葉

- **コネクションプール（connection pool）** — DB 接続を**使い回す束**。毎回つなぐと重いので数本〜数十本を共有する。
- **プール枯渇 / 膝（saturation / knee）** — 同時クエリがプール本数を超え、**待たされ始める**点。グラフが急に折れ曲がる（膝）。
- **N+1** — 一覧 1 回＋各行に 1 回ずつ問い合わせて、**往復が爆発する**アンチパターン。まとめて引けば直る。
- **covering index（カバリングインデックス）** — **索引だけで答えが出て**、表本体を読まずに済む索引。速い。
- **keyset ページング（keyset pagination）** — 「前回の最後の id の続きから」読むページ送り。**OFFSET より速い**。
- **OFFSET** — 「先頭から N 件飛ばす」指定。**後ろのページほど遅い**（飛ばす分も数える）。
- **走査（scan）行数** — クエリが**実際になめた行数**。クエリの重さは総行数でなく走査行数で決まる。
- **inline / off-page** — 太い列が**行の中に納まる(inline)** か、**別ページに追い出される(off-page)** かの格納方式。舐めの重さが変わる。
- **clustered index（クラスタ索引）** — InnoDB では**表本体そのものが主キー順に並んだ木**。主キー設計が挿入速度を決める理由。
- **B-tree / ページ分割（page split）** — 索引の木構造と、挿入で**ページが割れて断片化**すること。ランダムな主キーで起きやすい。
- **ANALYZE / 統計** — オプティマイザが実行計画を選ぶための**行数などの見積り情報**。古いと計画がずれる。

## トランザクション・ロックの言葉

- **トランザクション（transaction / tx）** — **まとめて成功か失敗か**にする一連の操作。途中で落ちても中途半端に残らない。
- **コミット / ロールバック（commit / rollback）** — 確定させる / 取り消す。コミットは**ディスク書き込み(fsync)**を伴い重い。
- **分離レベル RR / RC** — **REPEATABLE READ**（MySQL 既定・tx 内で見え方が固定）と **READ COMMITTED**（毎回最新を見る）。→ [isolation.md](isolation.md)
- **スナップショット（snapshot）** — ある瞬間の**一貫した見え方**。RR は tx 開始時のスナップショットを読み続ける。
- **MVCC（multi-version concurrency control）** — 読みと書きがぶつからないよう**複数版を保持**する仕組み。RR の一貫読みの正体。
- **non-repeatable read（非再現読み）** — 同じ行を2回読んで**値が変わる**こと（RC で起きる）。
- **phantom（ファントム／幻の行）** — 範囲を読んでいる間に**その範囲へ行が挿入される**こと。
- **gap ロック（gap lock／隙間ロック）** — 行と行の**隙間をロック**して phantom を防ぐ（RR で範囲をロック読みしたとき）。
- **デッドロック（deadlock）** — 2つの tx が**互いのロックを待って進めない**状態。片方が犠牲になりやり直す（1213）。
- **lease / fence（リース／フェンス）** — 「今の担当は自分」という**期限つき借用権**と、古い担当の書き込みを弾く**番号（フェンストークン）**。→ [fencing.md](fencing.md)

## メモリ・Go の言葉

- **goroutine** — Go の**軽量スレッド**。1本の初期スタックは ~2KB（[EXP-50](reference-numbers.md)）。接続1本＝goroutine 1本になりがち。
- **GC（garbage collection）** — 使わなくなったメモリの**自動回収**。回収が多いと CPU を食う（＝GC 圧）。
- **allocation / alloc（アロケーション）** — **メモリ確保**。ループ内で確保を繰り返すと GC 圧が上がる。`allocs/op` はその回数。
- **boxing（ボクシング）** — `int` などの値を `any`/interface に入れるとき起きる**確保**。ホットパスでは避ける。→ [reference-numbers.md](reference-numbers.md)
- **ヒープ / スタック（heap / stack）** — 長生きする確保はヒープ、関数内で完結すればスタック（速い）。
- **スナップショット（メモリ側）** — hub がテナントの状態を**メモリに持つ写し**。破棄粒度が問題になる。→ [subscription-design.md](subscription-design.md)

## 分散・信頼性の言葉

- **冪等（べきとう）（idempotent）** — **何回実行しても結果が同じ**こと。再送・二重実行に強い。冪等キーで二度目を弾く。
- **at-least-once / exactly-once** — 「最低1回は届く（重複あり）」/「ちょうど1回」。at-least-once × 冪等消費 = 実質 exactly-once。
- **outbox（アウトボックス）** — 外部への**送信予定を、本体と同じ DB トランザクションに書いておく箱**。送信漏れ／二重送信を防ぐ。→ [outbox.md](outbox.md)
- **dead-letter（デッドレター）** — 何度やっても失敗する命令（poison）の**隔離先**。無限リトライでキューを詰まらせない。→ [dead-letter.md](dead-letter.md)
- **fail-fast** — ダメなものは**すぐ諦める**（恒久エラーはリトライしない）。→ [retry.md](retry.md)
- **backpressure（バックプレッシャー）** — 過負荷のとき**受付を絞って**全体が倒れるのを防ぐ。→ [backpressure.md](backpressure.md)
- **graceful shutdown** — 停止時に**途中の仕事を殺さず**、受付を止めてから終わらせる。→ [shutdown.md](shutdown.md)
- **failover（フェイルオーバー）** — DB が倒れたとき**別のノードへ切り替わる**こと。接続は切れるので張り直す。→ [db-resilience.md](db-resilience.md)
- **ノイジーネイバー（noisy neighbor）** — 1テナントの暴走が**同居する他テナントを巻き込む**問題。公平性で隔離。→ [tenant-fairness.md](tenant-fairness.md)
- **バックオフ（backoff）** — 再試行の間隔を**だんだん伸ばす**（指数＋ジッタ）。一斉再試行の雪崩を防ぐ。→ [adaptive-backoff.md](adaptive-backoff.md)

## リアルタイム配信の言葉

- **SSE（Server-Sent Events）** — サーバーからブラウザへの**一方向のライブ配信**。接続は張りっぱなし。
- **subscription（サブスクリプション）** — GraphQL で**変化を購読して受け取る**仕組み（実体は SSE/WebSocket）。
- **hub（ハブ）** — テナントごとに **1つの poller が DB を見て、多数の購読者へ配る**共有の仕組み。DB 集中を防ぐ。→ [sse-fan-in.md](sse-fan-in.md)
- **fan-out / fan-in** — 1つの変更を**多数へ配る**のが fan-out、多数の要求が**1箇所（DB）に集中**するのが fan-in。
- **coalesce（コアレス／畳む）** — バラバラの要求や更新を**まとめて1回にする**。fan-in 対策の基本。→ [fanout.md](fanout.md)
- **pub/sub（publish/subscribe）** — **発行と購読**でメッセージを配る仕組み。プロセスを跨いで無効化や更新を配れる。→ [sse-fan-in.md](sse-fan-in.md)
- **stampede（スタンピード／thundering herd）** — キャッシュ期限切れ直後などに**一斉にアクセスが殺到**すること。
- **singleflight** — 同じ問い合わせが同時に来たら**1回にまとめて**、結果を全員で共有する仕組み。stampede 対策。→ [cache.md](cache.md)
- **TTL（time to live）** — キャッシュの**有効期限**。過ぎたら引き直す。
- **re-auth（re-authorization／再認可）** — 長時間の接続で**権限を定期的に確認し直す**。失権後も配信し続けるのを防ぐ。→ [subscription-design.md](subscription-design.md)

## スキーマ・文字の言葉

- **主キー（primary key / PK）** — 行を一意に決める列。InnoDB では**表の並び順そのもの**。連番が有利な理由。→ [primary-key.md](primary-key.md)
- **二次索引（secondary index）** — 主キー以外の検索用索引。**中に主キーを内包する**ので、太い主キーは全索引を膨らませる。
- **UUID（v4 / v7）** — ランダムな一意 ID（v4）と、**時刻順**にした一意 ID（v7 相当）。v4 は主キーだと断片化する。
- **multi-row INSERT** — 複数行を**1文でまとめて入れる**。単発ループより桁で速い。→ [bulk-insert.md](bulk-insert.md)
- **prepared statement（プリペアドステートメント）** — **パース済みの SQL** を使い回す。パースを1回に。
- **charset / collation（文字集合／照合順序）** — 使える文字の種類 / **比較・並び替えのルール**（大小無視 `_ci` か厳密 `_bin` か）。→ [charset.md](charset.md)
- **utf8mb4** — 絵文字まで含む**完全な UTF-8（1文字最大4バイト）**。索引の長さ上限に効く。
- **prefix index（プレフィックス索引）** — 長い列の**先頭 N 文字だけ**に張る索引。索引長の上限回避。
- **パーティション（partition）** — 表を**日付などで区切った区画**。古い区画を丸ごと DROP して保持期間を適用できる。→ [retention.md](retention.md)

---

> 用語がまだ分かりにくければ、その語を使っている doc（右のリンク）に具体例と実測がある。
> 全体像は [design-principles.md](design-principles.md)、数字は [reference-numbers.md](reference-numbers.md)、
> 具体サンプルは [worked-examples.md](worked-examples.md)。
