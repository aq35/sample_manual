# EXP-27 sendCommand の冪等性（uq_idem）と入力検証

| | |
| --- | --- |
| Experiment | EXP-27 / gqlgen-mutation-idempotency |
| Starting SHA | `b582002fec96` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 同じ idempotencyKey の再送は、新しい行を作らず既存の命令を返す（DB 上も1行）。 2) 違う idempotencyKey は別の命令になる。 3) 不正入力（未知の type・空の冪等キー・長すぎる payload）は DB に触れる前に拒否し、行は作らない。 4) 検証エラーはクライアント起因として返す（内部 500 にしない）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=b582002fec96+dirty |
| Started / Ended | 2026-09-07T07:57:08Z / 2026-09-07T07:57:08Z |

## Results

### 冪等: 同じ idempotencyKey の再送は同じ命令・DB 1行 — OK

| 数えたもの | 値 |
| --- | --- |
| rows | 1 |
| same_id | 1 |

- 初回 cmd-b1e4fdbc60af1136fff2e431e5403fff / 再送 cmd-b1e4fdbc60af1136fff2e431e5403fff

### 違う idempotencyKey は別命令 — OK

| 数えたもの | 値 |
| --- | --- |
| different_id | 1 |

### 入力検証: 未知の type は拒否・行を作らない — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| rejected | 1 |
| rows | 0 |

- error: [{"message":"invalid input: type=\"explode\" は不正（move/stop/charge/reset のいずれか）","path":["sendCommand"],"locations":[{"line":1,"column":35}],"extensions":{"code":"USER_ERROR"}}]

### 入力検証: 空の idempotencyKey は拒否 — **事故あり**

- error: [{"message":"invalid input: idempotencyKey は必須（二重発行を防ぐため）","path":["sendCommand"],"locations":[{"line":1,"column":35}],"extensions":{"code":"USER_ERROR"}}]

## Verdict

書き込みは冪等キー（uq_idem）で再送を吸収し、同じキーは新しい行を作らず既存を返す。入力は DB に触れる前に検証し、不正はクライアント起因エラーで返して行を作らない。

## 適用範囲

- MySQL 8.0 / gqlgen v0.17 / cmd_command.uq_idem (tenant_id, idem_key)
- 許可 type=move/stop/charge/reset・payload<=255・idempotencyKey 必須
- 冪等は『既存を探して返す／無ければ INSERT、競合時は既存を返す』

## 保証しない範囲・未検証

- 競合時のフォールバック（ErrConflict→再取得）は同時再送の一方のみ検証。厳密な並行試験は別
- 入力検証はサーバ側の最小限。スキーマ制約（enum 化等）でさらに前段に寄せられる

## 再利用できる成果物

- internal/gql: sendCommand（validateSendCommand・冪等発行）
- docs/graphql.md: mutation の冪等性・入力検証

## 次の実験

- EXP-28 行レベル認可

