# EXP-44 DB 更新と外部呼び出しをまたぐ crash で二重/未送信を出さない（outbox＋冪等）

| | |
| --- | --- |
| Experiment | EXP-44 / transactional-outbox |
| Starting SHA | `bec717ea663f` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 素朴（外部送信→done、非冪等）: 送信直後・done 前に落ちると再送で二重効果（effects > 命令数）。 2) outbox（意図を原子的に記録→relay が at-least-once 送信）＋冪等受け側: 再送があっても効果は命令数ぴったり（exactly-once）。未送信も残らない（pending=0）。 3) 同じ crash 注入（3件で送信後 mark 前に落ちる）で比較する。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=bec717ea663f+dirty |
| Started / Ended | 2026-09-07T11:53:58Z / 2026-09-07T11:53:58Z |

## Results

### 素朴: 外部送信→done・非冪等（crash 再送で二重効果） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| attempts | 27 |
| commands | 20 |
| effects | 27 |

- 効果 27 > 命令 20（7件が二重に効いた）

### outbox＋冪等: at-least-once 送信でも効果は命令数ぴったり — OK

| 数えたもの | 値 |
| --- | --- |
| attempts | 27 |
| commands | 20 |
| effects | 20 |
| pending_after1 | 7 |
| pending_after2 | 0 |

- 送信 27 回（再送含む）だが効果 20 = 命令 20。未送信 0

## Verdict

DB 更新と外部呼び出しをまたぐ処理は、業務変更と『送信意図(outbox)』を1トランザクションで原子的に書き、別 relay が at-least-once で送る。外部を冪等(idem_key)にすれば、crash による再送があっても効果は命令数ぴったり（exactly-once）で、未送信も残らない。素朴な『送信→done・非冪等』はcrash 再送で二重効果になる。

## 適用範囲

- MySQL 8.0 / outbox 20件・3の倍数(7件)で送信後 sent 前に crash / 外部はメモリの擬似ロボット
- 効果=distinct な適用（冪等受け側は idem_key で重複無視）。送信=呼び出し回数（再送含む）
- 業務変更と outbox 行は同一トランザクションで書く（ここでは outbox 投入で代表）

## 保証しない範囲・未検証

- 『原子的に業務変更＋outbox』の tx は SeedOutbox で代表。実装では業務行の INSERT/UPDATE と同 tx
- relay の並行実行は claim（原子的 UPDATE・EXP-30）で1件1レプリカに。ここは単一 relay
- 外部の冪等性は受け側の責務（idem_key）。受け側が冪等でないなら二重効果は防げない
- outbox のパージ（sent の掃除）は保持期間で（EXP-15 の DROP PARTITION 等）

## 再利用できる成果物

- internal/outboxlab: outbox テーブル・relay・冪等/非冪等の外部擬似
- docs/outbox.md: トランザクショナル outbox と exactly-once 外部作用

## 次の実験

- EXP-45 poison / dead-letter

