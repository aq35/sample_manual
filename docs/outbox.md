# トランザクショナル outbox と exactly-once の外部作用（EXP-44）

ワーカーは「DB 更新」と「外部呼び出し（ロボット指示）」をまたぐ。両者を別々にやると、間で
落ちたときに **二重指示**（送った後 done にする前に落ちて再送）や **未送信**（done にした後
落ちて未送）になる。outbox でこれを断つ。EXP-1（crash と OUTCOME_UNKNOWN）の完成形。

実装は [internal/outboxlab](../internal/outboxlab)、receipt は
[docs/results/exp-44](results/exp-44/exp-44-transactional-outbox.md)。

## パターン

1. **書く（原子的）**: 業務変更（例: `cmd_command` を dispatched に）と、**送信意図＝outbox 行**を
   **同一トランザクション**で書く。これで「業務変更したのに送信意図が無い／その逆」が起きない。
2. **送る（relay）**: 別の relay が未送信(outbox pending)を読み、外部へ送り、sent に落とす。
   送った直後・sent 前に落ちても、次周で**再送**される（**at-least-once**）。
3. **冪等な受け側**: 外部は `idem_key` で重複を無視する。→ 再送があっても**効果は1回**＝
   実質 **exactly-once**。

## 結果（20命令・7件で「送信後 sent 前」に crash 注入）

| 方式 | 送信回数(attempts) | 効果(effects) | 未送信 |
| --- | --- | --- | --- |
| 素朴（送信→done・非冪等） | 27 | **27**（7件が二重に効いた） | — |
| outbox＋冪等 | 27（再送含む） | **20**（命令数ぴったり） | 7 → **0** |

- 素朴は crash 再送で **二重効果**（effects 27 > 命令 20）。
- outbox は送信こそ再送で 27 回だが、冪等受け側が重複を無視するので **効果は 20＝exactly-once**。
  未送信も残らない（pending 0）。

## 要点

- **業務変更と outbox 行は同一トランザクション**（別々にしない）。ここが原子性の肝。
- **relay は at-least-once**（必ず届くが重複しうる）。だから**受け側の冪等性（idem_key）が必須**。
  受け側が冪等でないなら、この二重効果は防げない。
- relay の並行実行は**原子的 claim**（条件つき UPDATE・[EXP-30](redundancy.md)）で1件1レプリカに。
- 送信済み outbox の掃除は保持期間で（[EXP-15](table-split.md) の `DROP PARTITION` 等）。
- 「送ったが結果不明(OUTCOME_UNKNOWN)」は失敗と決めつけない（[EXP-1](crash-effects.md)）。再送は
  冪等だから安全、結果は独立に観測して記録する。

## 保証しない範囲・未検証

- 業務変更＋outbox の原子 tx は本実験では outbox 投入で代表（実装では業務行の INSERT/UPDATE と同 tx）。
- 外部の冪等性は受け側の責務。受け側 API に冪等キーが無い場合は別の仕組み（dedup ストア）が要る。
- 2フェーズコミットは使わない（DB と外部をまたぐ分散 tx は避け、outbox＋冪等で代替する方針）。
