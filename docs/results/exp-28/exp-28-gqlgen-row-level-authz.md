# EXP-28 行レベル認可: この robot を操作してよいか（対象×主体）

| | |
| --- | --- |
| Experiment | EXP-28 / gqlgen-row-level-authz |
| Starting SHA | `b582002fec96` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) grant のある (u-op, r0000) は操作できる。 2) 同じテナント内でも grant の無い robot（u-op, r0001）は拒否。 3) grant を持たない別主体（u-other, r0000）は、他人が操作できる robot でも拒否。 4) 拒否時は命令を書き込まない（行を作らない）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=b582002fec96+dirty |
| Started / Ended | 2026-09-07T07:57:08Z / 2026-09-07T07:57:08Z |

## Results

### grant あり (u-op, r0000) → 操作できる — OK

| 数えたもの | 値 |
| --- | --- |
| ok | 1 |

### grant 無しの robot (u-op, r0001) → 拒否・行を作らない — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| rejected | 1 |
| rows | 0 |

- error: [{"message":"forbidden: この robot を操作する権限がない","path":["sendCommand"],"locations":[{"line":1,"column":35}],"extensions":{"code":"USER_ERROR"}}]

### 別主体 (u-other, r0000) → 他人が操作できる robot でも拒否 — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| rejected | 1 |
| rows | 0 |

- error: [{"message":"forbidden: この robot を操作する権限がない","path":["sendCommand"],"locations":[{"line":1,"column":35}],"extensions":{"code":"USER_ERROR"}}]

## Verdict

行レベル認可は『この主体がこの対象を操作してよいか』を、対象と主体を見てリゾルバで判定する。フィールド単位の @auth では表せない（行に依る）。主体は context から取り、拒否時は書き込まない。

## 適用範囲

- MySQL 8.0 / gqlgen v0.17 / robot_operator (tenant_id, operator, robot_id) が grant
- 主体（operator）は context（X-User→principal）から。引数から取らない
- 行レベルは対象×主体に依るのでリゾルバで判定（@auth ディレクティブでは表せない）

## 保証しない範囲・未検証

- ここは『操作(mutation)』の行レベル認可。読み取り側の行フィルタは別途（一覧に混ぜない）
- grant の管理（誰がいつ付与・失効するか）は本実験外
- ADMIN のバイパス等のポリシーは要件次第。ここでは grant 必須で統一

## 再利用できる成果物

- internal/gql: canOperate（robot_operator による行レベル認可）と sendCommand リゾルバ
- docs/graphql.md: 行レベル認可（対象×主体）

## 次の実験

- なし

