# EXP-32 ローリングデプロイ中の無停止スキーマ変更（status→state 改名）

| | |
| --- | --- |
| Experiment | EXP-32 / expand-contract |
| Starting SHA | `4bb77e776e25` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 列を一気に張り替える（CHANGE status state）と、その瞬間から旧アプリ v1 の SELECT status が壊れる。 2) expand（state を足して backfill）すれば、v1 は status のまま無傷、v2 は state を使える。 3) 移行期は両方に書く（dual-write）ので、v1 も v2 も読める。 4) contract（status を落とす）は v1 が全て退役した後にだけ。先にやると v1 が壊れる。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4bb77e776e25+dirty |
| Started / Ended | 2026-09-07T09:36:29Z / 2026-09-07T09:36:29Z |

## Results

### 危険: 一気に CHANGE status→state（デプロイ中に v1 が壊れる） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| v1_ok_after | 0 |
| v1_ok_before | 1 |

- 改名後の v1 error: Error 1054 (42S22): Unknown column 'status' in 'field list'

### 安全①expand: state を足す → v1 は無傷（status のまま読み書きできる） — OK

| 数えたもの | 値 |
| --- | --- |
| v1_read_ok | 1 |
| v1_write_ok | 1 |

### 安全②移行期: v2 は dual-write＋state 読み / v1 も status 読み → 両方 OK — OK

| 数えたもの | 値 |
| --- | --- |
| v1_read_ok | 1 |
| v2_read_ok | 1 |
| v2_write_ok | 1 |

### 安全③contract: status を落とす → v2 は無傷。v1 はもう壊れる（退役後だから可） — OK

| 数えたもの | 値 |
| --- | --- |
| v1_ok | 0 |
| v2_read_ok | 1 |
| v2_write_ok | 1 |

- contract 後の v1 error: Error 1054 (42S22): Unknown column 'status' in 'field list'（v1 は退役済みなので問題ない）

## Verdict

スキーマ変更は『足す→両対応→切替→消す』（expand/contract）で無停止にする。列の一気張り替え・先に消す、は旧アプリを即壊す。追加は nullable で、移行期は dual-write、消すのは旧バージョンが全て退役した後にだけ。

## 適用範囲

- MySQL 8.0 / 列 status→state の改名を例に / v1=status を読み書き, v2=state を使う
- デプロイ中は v1 と v2 が同じ DB を同時に触る前提
- MySQL 8.0 の ADD/DROP COLUMN は多くが online DDL（別途 EXP でオンライン挙動は測る）

## 保証しない範囲・未検証

- 大きな表での ALTER の所要・ロックは表サイズと版で変わる（online DDL / gh-ost は別）
- dual-write の一貫性は、書き込み経路が1つ（このアプリ）である前提。複数書き手なら別途
- backfill は小表で一括。1回で終わらない規模ではチャンク分割・進捗管理が要る

## 再利用できる成果物

- internal/deploylab: v1/v2 の読み書きと expand/contract の DDL 手順
- docs/zero-downtime-migration.md: 無停止スキーマ変更の手順

## 次の実験

- EXP-33 DB フェイルオーバ/再起動への耐性

