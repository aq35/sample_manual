# EXP-1 外部 effect の途中で SIGKILL されたときの二重送信と『結果不明』の扱い

| | |
| --- | --- |
| Experiment | EXP-1 / external-effect-crash |
| Starting SHA | `83598d6b4d1e` (作業ツリーに未コミットの変更あり) |
| Hypothesis (frozen before result) | 1) naive（呼んでから記録する・冪等キー無し）は、effect 成立後の SIGKILL で二重 effect を出す。 2) 冪等キーを付け、送る前に意図を記録すれば、二重 effect は 0 になる。 3) ただし 2 は相手が冪等キーを守ることに依存する。守らない相手では二重に戻る。 4) 応答が得られなかった要求は、送り直す前に相手へ問い合わせれば、    『二重送信』も『不明のまま放置』も避けられる。 |
| Environment | go1.24.7 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=83598d6b4d1e+dirty |
| Started / Ended | 2026-09-03T15:09:37Z / 2026-09-03T15:09:51Z |

## Workload

- `kill_on_request` = 3
- `requests_per_run` = 5
- `strategies` = [naive_retry idempotency_key transactional_outbox outbox_then_observe]

## Failure injection

- `kill_points` = [before_request_saved after_dispatch_reserved before_service_receives after_effect_before_response after_response_before_receipt after_receipt_before_state after_state_before_reply]
- `method` = 子プロセスが指定地点で自分に SIGKILL（effect 成立後の地点だけはサービス側 hook が子を kill）

## Results

### naive_retry — **事故あり**

外部を呼んでから記録する。冪等キー無し。再起動後は記録が無いものを最初から処理する

| 数えたもの | 値 |
| --- | --- |
| completed | 35 |
| duplicate_effects | 5 |
| false_completed | 0 |
| kill_points_exercised | 7 |
| orphan_effects | 0 |
| outcome_unknown_left | 0 |
| re_dispatched | 0 |
| resolved_by_observation | 0 |

遅延: n=7 p50=15.318ms p95=64.495ms p99=64.495ms max=64.495ms

- after_effect_before_response で事故: 二重 3 / 記録なし effect 0 / 実体の無い完了 0
- after_response_before_receipt で事故: 二重 1 / 記録なし effect 0 / 実体の無い完了 0
- after_receipt_before_state で事故: 二重 1 / 記録なし effect 0 / 実体の無い完了 0

### idempotency_key — OK

送る前に意図を記録し、要求ごとに固定した冪等キーを付けて送る

| 数えたもの | 値 |
| --- | --- |
| completed | 35 |
| duplicate_effects | 0 |
| false_completed | 0 |
| kill_points_exercised | 7 |
| orphan_effects | 0 |
| outcome_unknown_left | 0 |
| re_dispatched | 0 |
| resolved_by_observation | 0 |

遅延: n=7 p50=17.872ms p95=24.314ms p99=24.314ms max=24.314ms

- 全 7 地点で、二重 effect も記録漏れも実体の無い完了も 0

### transactional_outbox — OK

業務状態と『送るつもり』を同じトランザクションで確定してから送る

| 数えたもの | 値 |
| --- | --- |
| completed | 35 |
| duplicate_effects | 0 |
| false_completed | 0 |
| kill_points_exercised | 7 |
| orphan_effects | 0 |
| outcome_unknown_left | 0 |
| re_dispatched | 0 |
| resolved_by_observation | 0 |

遅延: n=7 p50=24.658ms p95=47.673ms p99=47.673ms max=47.673ms

- 全 7 地点で、二重 effect も記録漏れも実体の無い完了も 0

### outbox_then_observe — OK

outbox に加え、結果不明のものは **送り直す前に相手へ問い合わせる**

| 数えたもの | 値 |
| --- | --- |
| completed | 35 |
| duplicate_effects | 0 |
| false_completed | 0 |
| kill_points_exercised | 7 |
| orphan_effects | 0 |
| outcome_unknown_left | 0 |
| re_dispatched | 0 |
| resolved_by_observation | 5 |

遅延: n=7 p50=25.253ms p95=32.676ms p99=32.676ms max=32.676ms

- 全 7 地点で、二重 effect も記録漏れも実体の無い完了も 0

### idempotency_key / 相手がキーを守らない場合 — **事故あり**

同じ方式のまま、外部サービスの冪等キー対応だけを外した反証

| 数えたもの | 値 |
| --- | --- |
| duplicate_effects | 3 |
| effects_total | 8 |

- 冪等キーは **こちら側の防御ではない**。相手が重複を弾いて初めて効く
- 相手の実装が不明なら、送り直す前に問い合わせる（observe）ほうが確実

### outbox_then_observe / 相手がキーを守らない場合 — OK

同じ条件（相手は冪等キーを見ない）で、送り直す前に問い合わせる方式

| 数えたもの | 値 |
| --- | --- |
| duplicate_effects | 0 |
| effects_total | 5 |
| re_dispatched | 0 |
| resolved_by_observation | 3 |

- 冪等キーが効かない相手でも、二重 effect は発生しなかった
- 『結果が分からないものは、送り直す前に相手に聞く』が、相手の実装に依存しない唯一の防御

## Verdict

『呼んでから記録する』方式は、effect 成立後に落ちると記録が残らず、再試行で二重 effect になる。送る前に意図を記録し、要求ごとに固定した冪等キーを付ければ二重 effect は 0 になるが、それは相手がキーを守る場合に限る（反証で二重に戻ることを確認）。応答が得られなかったものを『送り直す前に問い合わせる』方式は、相手の冪等性に依存せず、二重送信も『不明のまま放置』も避けられた。

## 適用範囲

- MySQL 8.0 / 同一ホスト / 外部サービスは同一マシンの HTTP サーバ
- 1プロセス・逐次処理。並行実行時の競合は EXP-2 で扱う
- effect の記録は外部サービス側のファイルに fsync して保存（プロセス kill では消えない）

## 保証しない範囲・未検証

- ネットワーク分断（応答が返らないまま接続だけ生きる）は未再現
- 外部サービス側が effect 記録の前に落ちる場合は未検証（相手の耐久性は仮定）
- 観測 API が古い値を返す（レプリカ遅延）場合の扱いは未検証

## 再利用できる成果物

- internal/effectsvc: 外部サービスの代役（effect をファイルに永続化・冪等キー対応を切替可能）
- internal/effectlab: 4方式の実装と OUTCOME_UNKNOWN を含む状態機械
- cmd/effectlab + expkit: 指定地点で SIGKILL して再起動する実験の型

## 次の実験

- EXP-2 lease/fencing: 複数プロセスが同じ対象を処理する場合の重複

