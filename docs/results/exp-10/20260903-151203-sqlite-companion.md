# EXP-10 MySQL で確かめた結論のうち、SQLite へ持ち込めるものと持ち込めないものを切り分ける

| | |
| --- | --- |
| Experiment | EXP-10 / sqlite-companion |
| Starting SHA | `83598d6b4d1e` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) pure Go 版と cgo 版のドライバは、同じ SQL・同じ PRAGMA に対して同じ観測値を返す （違いは速度と配布のしやすさだけ）。 2) 書き込みはデータベース全体で1つずつしか進まない。MaxOpenConns を増やしても 書き込みスループットは上がらず、SQLITE_BUSY が増えるだけ。 3) 既定の journal_mode=delete では読み手と書き手が互いを止める。WAL にすると止めなくなる。 4) MySQL で確かめた結論のうち、影響行数・UPSERT の戻り値・型の厳格さ・ DDL のトランザクション性・外部キーの既定は、そのままでは持ち込めない。 5) GET_LOCK 相当は無い。マイグレーションの排他は BEGIN IMMEDIATE で作れて、 しかも DDL をロールバックできるぶん EXP-6 より単純になる。 6) WAL のまま .db だけをコピーしたバックアップは、コミット済みのデータを失う。 VACUUM INTO なら失わない。 7) PRAGMA を db.Exec で入れると、プールの中の一部の接続にしか効かない。 8) 別プロセス3本が同じファイルを書くと、busy_timeout=0 では SQLITE_BUSY が大量に出る。 WAL と busy_timeout を両方入れると 0 になる。 9) synchronous を下げると書き込みは速くなるが、失うのは電源断への耐性であって プロセス死への耐性ではない。synchronous=OFF でも、コミットが返った直後に SIGKILL された 程度ではコミット済みのデータは消えない。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=83598d6b4d1e+dirty |
| Started / Ended | 2026-09-03T15:12:03Z / 2026-09-03T15:12:35Z |

## Workload

- `probes` = 12
- `rows` = 500
- `write_load` = 1行更新のトランザクションを1秒

## Failure injection

- `backup` = WAL に載ったまま .db だけをコピーする
- `concurrency` = 同一プロセス4本 / 別プロセス3本

## Results

### 同じ問いを両エンジンへ投げた結果 — **事故あり**

分類は観測値の一致・不一致で機械的に決めている（知識で決めていない）

| 数えたもの | 値 |
| --- | --- |
| DIFFERENT_MECHANISM | 4 |
| DIFFERENT_RESULT | 6 |
| SAME_SEMANTICS | 2 |

- [DIFFERENT_RESULT] affected-rows-noop | MySQL: 0 行 | SQLite: 1 行
-     → repo.Expect（EXP-2 の fencing、楽観ロック）は影響行数で持ち主を判定している。MySQL は既定で『実際に変わった行数』を返すので 0 になり、『自分が持ち主でない』と誤判定する（EXP-2 で踏んだ罠。clientFoundRows=true で回避した）。
- [DIFFERENT_RESULT] upsert-affected-rows | MySQL: 挿入=1 更新=2 変化なし=0 | SQLite: 挿入=1 更新=1 変化なし=1
-     → MySQL の ON DUPLICATE KEY UPDATE は 挿入=1 / 更新=2 / 変化なし=0 を返す。『2 なら更新だった』という判定を書いていると、SQLite ではすべて 1 で常に『挿入』に見える。
- [DIFFERENT_RESULT] type-affinity | MySQL: 拒否される: Error 1366 (HY000): Incorrect integer value: 'abc' for column 'v' at row 1 | SQLite: 入る（読み戻すと "abc"）
-     → SQLite は型宣言を**助言**として扱う（型親和性）。文字列がそのまま入る。MySQL は厳格モードで拒否する。『列の型で守られている』という前提が SQLite では成り立たない。
- [DIFFERENT_MECHANISM] ddl-rollback | MySQL: ROLLBACK しても残る（DDL は暗黙にコミットされる） | SQLite: ROLLBACK で消えた（DDL はトランザクションに入る）
-     → MySQL の DDL は暗黙にコミットされる（EXP-6 のマイグレーション事故の原因）。SQLite の DDL はトランザクションに入るので、『マイグレーション全体を1つのトランザクション』にできる。つまり EXP-6 で必要だった『段階の記録』は、SQLite では別の形になる。
- [DIFFERENT_MECHANISM] foreign-keys-default | MySQL: 検査される（親の無い行は拒否） | SQLite: 検査されない（親の無い行が入る）
-     → SQLite の外部キー検査は既定で OFF、しかも**接続ごとの設定**。DSN ではなく db.Exec("PRAGMA foreign_keys=ON") で入れると、プールの中の1本にしか効かない（Accident_接続ごとのPRAGMA で再現した）。
- [DIFFERENT_MECHANISM] empty-where-update | MySQL: 止まる: Error 1175 (HY000): You are using safe update mode and you tried to update a table without… | SQLite: 止まらない（全件更新が通る）
-     → MySQL には sql_safe_updates があるが、SQLite には無い。『間違えて全件更新』を止める仕組みが1つ減るので、repo の文字列検査（EXP-8）と Expect による影響行数の確認の比重が上がる。
- [SAME_SEMANTICS] append-only-trigger | MySQL: 守れる（更新が拒否される） | SQLite: 守れる（更新が拒否される）
- [DIFFERENT_MECHANISM] advisory-lock | MySQL: あり（GET_LOCK=1） | SQLite: なし（GET_LOCK という関数が無い）
-     → EXP-6 のマイグレーションは GET_LOCK で『同時に1つだけ』を作っている（docs/locking.md）。SQLite には相当物が無い。代わりに**書き込みロックがデータベース全体で1つ**なので、BEGIN IMMEDIATE を取った側だけが進む、という別の作り方になる。
- [DIFFERENT_RESULT] returning | MySQL: 使えない: Error 1064 (42000): You have an error in your SQL syntax; check the manual that correspond… | SQLite: 使える（更新後の値 2 が1往復で取れる）
-     → 『更新して、更新後の行を1往復で取る』が SQLite では書ける。MySQL 8.0 では書けない。SQLite 前提で書くと MySQL へ戻せなくなる（片道の移植になる）。
- [SAME_SEMANTICS] datetime-type | MySQL: time.Time "2026-01-02 03:04:05.678 +0000 UTC" | SQLite: time.Time "2026-01-02 03:04:05.678 +0000 UTC"
- [DIFFERENT_RESULT] datetime-mixed-format | MySQL: あと=1件（正=1） 同時刻=2件（正=2） 格納形 "2026-01-02 03:04:05 +0000 UTC" / "2026-01-02 04:04:05 +0000 UTC" | SQLite: あと=1件（正=1） 同時刻=1件（正=2） 格納形 "2026-01-02 03:04:05 +0000 UTC" / "2026-01-02 04:04:05 +0000 UTC"
-     → lease の期限判定（EXP-2）は `WHERE expires_at < ?` の形。SQLite に日時型は無く、比較は**入っている文字列の辞書順**になる。入れ方が2通り混ざると、`2026-01-02 03:04:05.678` と `2026-01-02T03:04:05.678Z` が同じ時刻として比較されない。
- [DIFFERENT_RESULT] datetime-text-column | MySQL: あと=1件（正=1） 同時刻=2件（正=2） 格納形 "2026-01-02 03:04:05" / "2026-01-02 04:04:05" | SQLite: あと=1件（正=1） 同時刻=1件（正=2） 格納形 "2026-01-02 03:04:05 +0000 UTC" / "2026-01-02 04:04:05"
-     → SQLite の列の型は助言でしかないが、**ドライバはその宣言を見て time.Time へ変換している**。つまり『時刻として扱われるか』は列の宣言しだいで変わる。MySQL では VARCHAR に入れても入れた文字列がそのまま返るだけで、変換は起きない。

### ドライバを入れ替えて同じ問いを投げた（仮説1の検証） — **事故あり**

modernc(pure Go) と mattn(cgo) に、同じ問いを同じ順で投げて突き合わせた

| 数えたもの | 値 |
| --- | --- |
| 問いの総数 | 12 |
| 食い違った問い | 1 |

- datetime-text-column: pure Go="あと=1件（正=1） 同時刻=1件（正=2） 格納形 \"2026-01-02 03:04:05 +0000 UTC\" / \"2026-01-02 04:04:05\"" / cgo="あと=1件（正=1） 同時刻=1件（正=2） 格納形 \"2026-01-02 03:04:05+00:00\" / \"2026-01-02 04:04:05\""

### ドライバ: modernc(pure Go) — OK

MaxOpenConns=1 / WAL / synchronous=NORMAL / 500 行

| 数えたもの | 値 |
| --- | --- |
| read_busy | 0 |
| read_errs | 0 |
| read_ops | 80873 |
| write_busy | 0 |
| write_errs | 0 |
| write_ops | 36176 |

| 測ったもの | 値 |
| --- | --- |
| read_p50_us | 10.000 |
| read_p99_us | 33.000 |
| read_per_sec | 80870.991 |
| write_p50_us | 17.000 |
| write_p99_us | 52.000 |
| write_per_sec | 36174.896 |

遅延: n=36176 p50=17µs p95=30µs p99=53µs max=9.299ms

- 同じホスト・同じ条件で測ったドライバ間の比較。**MySQL との比較には使わない**

### ドライバ: mattn(cgo) — OK

MaxOpenConns=1 / WAL / synchronous=NORMAL / 500 行

| 数えたもの | 値 |
| --- | --- |
| read_busy | 0 |
| read_errs | 0 |
| read_ops | 103406 |
| write_busy | 0 |
| write_errs | 0 |
| write_ops | 50650 |

| 測ったもの | 値 |
| --- | --- |
| read_p50_us | 8.000 |
| read_p99_us | 29.000 |
| read_per_sec | 103403.685 |
| write_p50_us | 11.000 |
| write_p99_us | 38.000 |
| write_per_sec | 50648.602 |

遅延: n=50650 p50=11µs p95=18µs p99=39µs max=9.021ms

- 同じホスト・同じ条件で測ったドライバ間の比較。**MySQL との比較には使わない**

### 配布のしやすさ（実行ファイルの大きさとクロスコンパイル） — OK

cmd/sizeprobe（開いて sqlite_version() を1回聞くだけの main）を各条件でビルドした

| 数えたもの | 値 |
| --- | --- |
| cgo / linux-amd64 (CGO_ENABLED=0) bytes | 3082782 |
| cgo / linux-amd64 (CGO_ENABLED=1) bytes | 6785560 |
| pure Go / darwin-arm64 bytes | 8986338 |
| pure Go / linux-amd64 bytes | 9245401 |
| pure Go / linux-arm64 bytes | 8911591 |

- pure Go / linux-amd64: ビルドできる / 8.8 MiB / 実行できる: sqlite sqlite=3.46.0 write-read=ok
- pure Go / linux-arm64: ビルドできる / 8.5 MiB
- pure Go / darwin-arm64: ビルドできる / 8.6 MiB
- cgo / linux-amd64 (CGO_ENABLED=1): ビルドできる / 6.5 MiB / 実行できる: sqlite3 sqlite=3.46.1 write-read=ok
- cgo / linux-amd64 (CGO_ENABLED=0): ビルドできる / 2.9 MiB / **実行すると失敗する**: create: Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a stub
- cgo / linux-arm64 (CGO_ENABLED=1, クロス): **ビルドできない** — # runtime/cgo

### 書き込み 1 並行 / MaxOpenConns=1 — OK

| 数えたもの | 値 |
| --- | --- |
| busy | 0 |
| errs | 0 |
| ops | 37119 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 17.000 |
| p99_us | 50.000 |
| per_sec | 37117.609 |

### 書き込み 4 並行 / MaxOpenConns=1 — OK

| 数えたもの | 値 |
| --- | --- |
| busy | 0 |
| errs | 0 |
| ops | 20968 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 113.000 |
| p99_us | 527.000 |
| per_sec | 15756.464 |

### 書き込み 4 並行 / MaxOpenConns=4 — OK

| 数えたもの | 値 |
| --- | --- |
| busy | 0 |
| errs | 0 |
| ops | 38637 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 18.000 |
| p99_us | 512.000 |
| per_sec | 38134.137 |

### 書き込み 8 並行 / MaxOpenConns=8 — OK

| 数えたもの | 値 |
| --- | --- |
| busy | 0 |
| errs | 0 |
| ops | 38567 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 18.000 |
| p99_us | 1164.000 |
| per_sec | 38006.077 |

### journal_mode=delete で読みながら書く — **事故あり**

書き手 1 本・読み手 3 本を同時に1秒

| 数えたもの | 値 |
| --- | --- |
| read_busy | 3 |
| read_errs | 0 |
| read_ops | 36 |
| write_busy | 0 |
| write_errs | 0 |
| write_ops | 1505 |

| 測ったもの | 値 |
| --- | --- |
| read_p99_us | 501711.000 |
| read_per_sec | 33.273 |
| write_per_sec | 1504.588 |

### journal_mode=wal で読みながら書く — OK

書き手 1 本・読み手 3 本を同時に1秒

| 数えたもの | 値 |
| --- | --- |
| read_busy | 0 |
| read_errs | 0 |
| read_ops | 67015 |
| write_busy | 0 |
| write_errs | 0 |
| write_ops | 12442 |

| 測ったもの | 値 |
| --- | --- |
| read_p99_us | 282.000 |
| read_per_sec | 67012.110 |
| write_per_sec | 12441.911 |

### 別プロセス3本が同じファイルを書く / WAL 無し・待たない（busy_timeout=0） — **事故あり**

goroutine ではなく本物の別プロセス（OS のファイルロック越しに競合する）

| 数えたもの | 値 |
| --- | --- |
| busy | 152851 |
| errs | 0 |
| ops | 1137 |

- プロセス1: {"driver":"modernc(pure Go)","ops":380,"busy":51549,"errs":0,"per_sec":379.989692399604,"p50_us":690,"p99_us":4060,"journal_mode":"delete"}
- プロセス2: {"driver":"modernc(pure Go)","ops":353,"busy":50587,"errs":0,"per_sec":352.9899609655102,"p50_us":664,"p99_us":2616,"journal_mode":"delete"}
- プロセス3: {"driver":"modernc(pure Go)","ops":404,"busy":50715,"errs":0,"per_sec":403.8451298350047,"p50_us":683,"p99_us":3431,"journal_mode":"delete"}

### 別プロセス3本が同じファイルを書く / WAL 有り・待たない（busy_timeout=0） — **事故あり**

goroutine ではなく本物の別プロセス（OS のファイルロック越しに競合する）

| 数えたもの | 値 |
| --- | --- |
| busy | 46701 |
| errs | 0 |
| ops | 31324 |

- プロセス1: {"driver":"modernc(pure Go)","ops":10300,"busy":14785,"errs":0,"per_sec":10299.556871865145,"p50_us":34,"p99_us":867,"journal_mode":"wal"}
- プロセス2: {"driver":"modernc(pure Go)","ops":10605,"busy":16278,"errs":0,"per_sec":10604.364300173298,"p50_us":33,"p99_us":837,"journal_mode":"wal"}
- プロセス3: {"driver":"modernc(pure Go)","ops":10419,"busy":15638,"errs":0,"per_sec":10414.751260564248,"p50_us":33,"p99_us":818,"journal_mode":"wal"}

### 別プロセス3本が同じファイルを書く / WAL 有り・待つ（busy_timeout=5s） — OK

goroutine ではなく本物の別プロセス（OS のファイルロック越しに競合する）

| 数えたもの | 値 |
| --- | --- |
| busy | 0 |
| errs | 0 |
| ops | 36860 |

- プロセス1: {"driver":"modernc(pure Go)","ops":4285,"busy":0,"errs":0,"per_sec":4000.260161610697,"p50_us":18,"p99_us":66,"journal_mode":"wal"}
- プロセス2: {"driver":"modernc(pure Go)","ops":23108,"busy":0,"errs":0,"per_sec":23107.40308956339,"p50_us":17,"p99_us":72,"journal_mode":"wal"}
- プロセス3: {"driver":"modernc(pure Go)","ops":9467,"busy":0,"errs":0,"per_sec":9388.296222870356,"p50_us":19,"p99_us":370,"journal_mode":"wal"}

### PRAGMA を db.Exec で1回だけ入れた（事故） — **事故あり**

起動時に db.Exec("PRAGMA foreign_keys = ON") を呼ぶ、よくある書き方

| 数えたもの | 値 |
| --- | --- |
| 設定が効いていた接続 | 1 |
| 調べた接続 | 4 |

- PRAGMA は接続ごとの設定。*sql.DB はプールなので、そのとき使われた1本にしか効かない
- 外部キー検査が効いている接続と効いていない接続が混ざる。**どちらに当たるかは実行のたびに変わる**ので、テストでは通って本番で壊れる

### PRAGMA を DSN に書いた（対策） — OK

file:...?_pragma=foreign_keys(1) — 新しい接続すべてに当たる

| 数えたもの | 値 |
| --- | --- |
| 設定が効いていた接続 | 4 |
| 調べた接続 | 4 |

### 読んでから書くトランザクションを2本同時 / BEGIN（既定の DEFERRED） — **事故あり**

20 回 × 2 本 = 40 トランザクション。busy_timeout=2s

| 数えたもの | 値 |
| --- | --- |
| 成功 | 20 |
| 進めなかった | 20 |

- DEFERRED は読み取りロックから始まり、最初の書き込みで昇格しようとする。両方が読んでから書くと、片方は昇格できない
- ★このとき busy_timeout は助けにならない。相手も同じ場所で待っているので、待っても順番が来ない
- IMMEDIATE は最初から書き込みロックを取るので、待てば必ず順番が来る

### 読んでから書くトランザクションを2本同時 / BEGIN IMMEDIATE — OK

20 回 × 2 本 = 40 トランザクション。busy_timeout=2s

| 数えたもの | 値 |
| --- | --- |
| 成功 | 40 |
| 進めなかった | 0 |

- DEFERRED は読み取りロックから始まり、最初の書き込みで昇格しようとする。両方が読んでから書くと、片方は昇格できない
- ★このとき busy_timeout は助けにならない。相手も同じ場所で待っているので、待っても順番が来ない
- IMMEDIATE は最初から書き込みロックを取るので、待てば必ず順番が来る

### マイグレーション4本を1つのトランザクションで流し、3本目で失敗させた — OK

MySQL では DDL が暗黙にコミットされるので、この形は作れない（EXP-6）

| 数えたもの | 値 |
| --- | --- |
| 残った表 | 0 |

- 残った表: []
- 0 なら、失敗したマイグレーションは**跡形もなく戻る**。EXP-6 で必要だった『どこまで進んだかの記録』は、SQLite では要らなくなる
- ただし『複数のマイグレーションをまとめて1トランザクション』にすると、その間じゅう書き込みロックを握る。起動時に他のプロセスが書けなくなる点は MySQL と同じ問題

### journal_mode=delete / synchronous=FULL — OK

1行更新のトランザクションを1本で1秒（MaxOpenConns=1）

| 数えたもの | 値 |
| --- | --- |
| ops | 1337 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 702.000 |
| p99_us | 1421.000 |
| per_sec | 1336.725 |

- この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**

### journal_mode=delete / synchronous=OFF — OK

1行更新のトランザクションを1本で1秒（MaxOpenConns=1）

| 数えたもの | 値 |
| --- | --- |
| ops | 18498 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 47.000 |
| p99_us | 117.000 |
| per_sec | 18497.456 |

- この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**

### journal_mode=wal / synchronous=FULL — OK

1行更新のトランザクションを1本で1秒（MaxOpenConns=1）

| 数えたもの | 値 |
| --- | --- |
| ops | 6415 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 137.000 |
| p99_us | 365.000 |
| per_sec | 6414.072 |

- この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**

### journal_mode=wal / synchronous=NORMAL — OK

1行更新のトランザクションを1本で1秒（MaxOpenConns=1）

| 数えたもの | 値 |
| --- | --- |
| ops | 34965 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 17.000 |
| p99_us | 49.000 |
| per_sec | 34906.004 |

- この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**

### journal_mode=wal / synchronous=OFF — OK

1行更新のトランザクションを1本で1秒（MaxOpenConns=1）

| 数えたもの | 値 |
| --- | --- |
| ops | 50566 |

| 測ったもの | 値 |
| --- | --- |
| p50_us | 17.000 |
| p99_us | 47.000 |
| per_sec | 50564.937 |

- この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**

### synchronous を下げると何を失うか（測っていないことの明示） — OK

- ここで測ったのは**速度だけ**。synchronous=OFF/NORMAL が何を失うかは、このホストでは測れない（電源断・カーネルパニックが要る）
- 次のサブテスト（コミット後のプロセス死）で測れるのは『プロセスが落ちても残るか』まで。**プロセス死と電源断は別の事故**で、synchronous が効くのは後者
- 電源断の実験は LIVE_ENV_REQUIRED（手順は docs/sqlite.md に置いた）

### 200 件コミットした直後に SIGKILL / journal=WAL synchronous=FULL — OK

外から時間を見計らって kill せず、コミットが返った地点で自分を殺す

| 数えたもの | 値 |
| --- | --- |
| コミットした件数 | 200 |
| 落ちたあとに残っていた件数 | 200 |

- SIGKILL された: true / 子プロセスの出力: EXPPOINT after_commit
- ★これで測れるのは**プロセスの死**まで。電源断は測れていない

### 200 件コミットした直後に SIGKILL / journal=WAL synchronous=NORMAL — OK

外から時間を見計らって kill せず、コミットが返った地点で自分を殺す

| 数えたもの | 値 |
| --- | --- |
| コミットした件数 | 200 |
| 落ちたあとに残っていた件数 | 200 |

- SIGKILL された: true / 子プロセスの出力: EXPPOINT after_commit
- ★これで測れるのは**プロセスの死**まで。電源断は測れていない

### 200 件コミットした直後に SIGKILL / journal=WAL synchronous=OFF — OK

外から時間を見計らって kill せず、コミットが返った地点で自分を殺す

| 数えたもの | 値 |
| --- | --- |
| コミットした件数 | 200 |
| 落ちたあとに残っていた件数 | 200 |

- SIGKILL された: true / 子プロセスの出力: EXPPOINT after_commit
- ★これで測れるのは**プロセスの死**まで。電源断は測れていない

### バックアップ: .db だけをコピー — **事故あり**

書いた直後（チェックポイント前）にバックアップして、別の場所で開き直した

| 数えたもの | 値 |
| --- | --- |
| バックアップの大きさ | 4096 |
| 元の行数 | 300 |
| 復元後の行数 | 0 |

- integrity_check = "ok"
- 中身のハッシュ: 元 a2f40c01c4bcea67 / 復元後 
- ★『バックアップコマンドが成功した』も『integrity_check が ok』も、中身が同じであることを意味しない

### バックアップ: .db + -wal + -shm をコピー — OK

書いた直後（チェックポイント前）にバックアップして、別の場所で開き直した

| 数えたもの | 値 |
| --- | --- |
| バックアップの大きさ | 119296 |
| 元の行数 | 300 |
| 復元後の行数 | 300 |

- integrity_check = "ok"
- 中身のハッシュ: 元 a2f40c01c4bcea67 / 復元後 a2f40c01c4bcea67
- ★『バックアップコマンドが成功した』も『integrity_check が ok』も、中身が同じであることを意味しない

### バックアップ: VACUUM INTO — OK

書いた直後（チェックポイント前）にバックアップして、別の場所で開き直した

| 数えたもの | 値 |
| --- | --- |
| バックアップの大きさ | 53248 |
| 元の行数 | 300 |
| 復元後の行数 | 300 |

- integrity_check = "ok"
- 中身のハッシュ: 元 a2f40c01c4bcea67 / 復元後 a2f40c01c4bcea67
- ★『バックアップコマンドが成功した』も『integrity_check が ok』も、中身が同じであることを意味しない

## Verdict

SQLite は「MySQL の小さい版」ではない。影響行数・UPSERT の戻り値・型の厳格さ・外部キーの既定・DDL のトランザクション性が違うため、repo 層の Expect（影響行数で持ち主を判定する仕組み）は書き換えないと持ち込めない。一方でマイグレーションは SQLite のほうが単純になる（DDL がロールバックできる）。詳細は docs/sqlite.md。

## 適用範囲

- SQLite modernc(pure Go)=3.46.0 mattn(cgo)=3.46.1 / modernc.org/sqlite v1.34.5 / github.com/mattn/go-sqlite3 v1.14.24
- 1台のホストのローカルファイル。ネットワークファイルシステム（NFS・EFS）は未検証
- MySQL 側の観測値は、この実行と同じ時刻に同じ問いを投げて取ったもの（過去の結果を流用していない）

## 保証しない範囲・未検証

- **MySQL の結論を SQLite の結論として流用していない。** 逆も同じ
- ネットワーク越しのファイル（NFS/EFS/SMB）でのロックは未検証。SQLite 本家が推奨していない構成
- 複数ホストからの同時書き込みは、この実験の対象外（SQLite では成り立たない前提）
- レプリケーション（litestream・LiteFS 等）は未検証
- 暗号化・全文検索など拡張が要る機能は、pure Go 版と cgo 版で差が出る可能性がある（未測定）
- 絶対値（ops/sec）はこのホストのものであって、engine 間の速度比較には使えない

## 再利用できる成果物

- internal/sqlitefacts: 同じ問いを両エンジンへ投げて分類する Probe（他プロジェクトの移植判断に流用できる）
- cmd/sqlitelab: 別プロセスから同じファイルを書く子プロセス
- cmd/sizeprobe: ドライバ1つぶんの実行ファイルの大きさとクロスコンパイルの可否を測る最小の main

## 次の実験

- EXP-11 backup / restore / corruption

