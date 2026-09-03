# MySQL 停止の原因調査と suite の完了条件（最終健全性レビュー #1・#2）

テスト後に MySQL が止まっていた件。**再起動で閉じず、原因を先に確定した。**
結論を先に書く: **原因はコンテナのアイドル再取得**であり、どの実験でもない。

## 収集した証拠

| 項目 | 観測 | 意味 |
| --- | --- | --- |
| system uptime | 調査時 **8 分**（`/proc/uptime` 526s） | コンテナ全体が数分前に再起動 = 再取得された |
| mysqld プロセス | 復帰時に**存在しない** | init/supervisor 管理下になく、手動起動なので一緒に消えた |
| error log の最後 | pid 1600 の最終行は 15:11 の redo 警告。**`Received SHUTDOWN` も `Shutdown complete` も無い** | 正常終了命令を受けていない |
| 各再起動の冒頭 | 03:14 / 11:38 / 15:00 のいずれも **`Starting XA crash recovery`** | 直前インスタンスは正常終了していない（＝突然消えた） |
| crash stacktrace | error log に `mysqld got signal` 等の**痕跡なし** | mysqld 内部クラッシュではない |
| メモリ | free 15 GB / swap 0 / **OOM 痕跡なし**（dmesg 空） | OOM ではない |
| disk / inode | 33% / 2% | 枯渇していない |
| kernel OOM log | なし | OOM Killer は動いていない |
| aborted connections | 通常範囲 | 接続の異常大量切断はなし |
| datadir | workerdb / workerdb2 / worker ユーザーは**再起動後も残っている** | データは永続ディスク。消えるのはプロセスだけ |

証拠ファイル: `docs/results/`（各 receipt に実行 SHA・環境・測定器 version）と、
この調査時のスナップショットは本文の表に固定してある。

## 各実験ごとの生存確認（二分探索）

EXP-1〜11 を**1つずつ**実行し、各実験後に MySQL の生存 + read/write probe +
接続数が baseline へ戻るかを確認した（`scripts/postflight.sh`）。

| 実験 | exit | 実験後の MySQL | probe | 接続 |
| --- | --- | --- | --- | --- |
| EXP-1 | 0 | alive（uptime 233） | ok | baseline |
| EXP-2 | 0 | alive（245） | ok | baseline |
| EXP-3 | 0 | alive（248） | ok | baseline |
| EXP-4 | 0 | alive（276） | ok | baseline |
| EXP-5 | 0 | alive（302） | ok | baseline（※後述） |
| EXP-6 | 0 | alive（306） | ok | baseline |
| EXP-7 | 0 | alive（313） | ok | baseline |
| EXP-8 | 0 | alive（313） | ok | baseline |
| EXP-9 | 0 | alive（323） | ok | baseline |
| EXP-10 | 0 | alive（359） | ok | baseline |
| EXP-11 | 0 | alive（361） | ok | baseline |

**uptime は 219→361 と単調増加**。sweep 中に MySQL は一度も再起動していない。
**どの実験も MySQL を止めなかった。**

### 測定器を疑った点（EXP-5 の誤検出）

最初の sweep で EXP-5 直後だけ「接続 5 > baseline 1」で postflight が fail した。
processlist を見ると、そこには `event_scheduler` と自分の probe しか居なかった。
原因は**クライアント（go test）が exit した直後で、MySQL がまだ TCP を回収していなかった**こと。
数秒で 1 に戻った。実装（poollab のリーク）ではなく測定器（即時判定）の問題だった。

対策: postflight の接続チェックを、回収を待つよう最大 10 秒ポーリングしてから判定するようにした。
再 sweep では EXP-5 も baseline へ戻った。

## 原因（確定）

**コンテナのアイドル再取得。** この remote 実行環境は、ターン間のアイドルで
コンテナを再取得（再起動）する（環境ドキュメントに明記）。mysqld は harness の
ライフサイクル外で手動起動した daemon なので、コンテナと一緒に落ち、復帰時に
自動起動されない。XA crash recovery が毎回走るのはこのため。

**否定できたもの**: 実験による停止 / OOM / disk 枯渇 / mysqld 内部クラッシュ /
停止命令の受信。いずれも証拠と一致しない。

この停止は「アイドルのたびに起きる」再現性がある（本セッションで 3 回以上観測）。
**原因は特定できたが、これは環境要因であって、suite が壊したのではない。**

## suite の完了条件を変えた（#2）

`go test ./...` の exit 0 **だけ**を成功条件にしない。`scripts/run-all.sh` の
末尾に postflight を組み込んだ。成功条件:

```
tests exit 0
AND MySQL alive
AND read/write probe succeeds
AND connection count returns to baseline
AND no leaked process
AND no leaked goroutine（各実験の sampler が閾値で検査。EXP-3 で goroutine リークを実際に検出済み）
AND no unexpected schema/database
AND worktree clean（EXP_RECORD 実行を除く）
AND result manifest valid（各 latest.json に meter_version）
```

### 外すと停止を見逃す counter-proof

`scripts/postflight-counterproof.sh`。死んだエンドポイントに対して:

- `go test`（DB パッケージ）は **exit 0**。DB に繋がらないと fail ではなく **skip** するから
- 同じ相手への probe は **down を検出**

つまり postflight を外すと、MySQL 停止が「成功」に埋もれる。実行例:

```
go test exit=0（skip されて 0 に見える: yes）
probe 結果: MySQL は down
COUNTER-PROOF OK: postflight を外すと、MySQL 停止が『成功』に埋もれる。
```

## 評価水準

- 個別の EXP-1〜11: `SELF_TESTED`（各 receipt に実行 SHA・環境・測定器 version）
- suite 全体: 原因が確定し、postflight で停止を検出できるようにしたため、
  **停止＝環境要因と判定できる状態**になった。ただし停止そのものは環境の仕様なので、
  「MySQL が落ちない」ことは保証しない（アイドルで必ず落ちる）。
  したがって「MySQL が常時生存する」意味での `VERIFIED_CLEAN` は主張しない。
  主張するのは「各実験は MySQL を壊さない」「停止は postflight で検出される」まで。

## 再発時の運用

コンテナ復帰後は mysqld を起動し直す（`scripts/mysql-up.sh`）。
CI/本番では mysqld を supervisor（systemd / Docker healthcheck + restart policy）
の管理下に置き、手動 daemon にしない。Docker なら healthcheck で
`mysqladmin ping` を叩き、落ちたら restart させる。
