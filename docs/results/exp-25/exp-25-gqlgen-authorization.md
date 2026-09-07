# EXP-25 @auth ディレクティブでフィールド単位の認可（テナント内ロール）

| | |
| --- | --- |
| Experiment | EXP-25 / gqlgen-authorization |
| Starting SHA | `5ad9d6d4f8a2` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) serial は @auth(requires: ADMIN)。ADMIN は取得でき、VIEWER/OPERATOR は拒否され null＋エラー。 2) ロールも主体（context）から決める。GraphQL 引数に role を置かない（詐称不能）。 3) @auth は解決関数の前に走るので、拒否時は serial 用の DB クエリを発行しない。 4) 拒否は serial だけを null にし、id/name など他フィールドは返る（nullable フィールド）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=5ad9d6d4f8a2+dirty |
| Started / Ended | 2026-09-07T07:44:59Z / 2026-09-07T07:44:59Z |

## Results

### ADMIN: serial が見える — OK

| 数えたもの | 値 |
| --- | --- |
| db_queries | 3 |
| serial_visible | 1 |

- getRobot + name + serial を引く

### VIEWER: serial 拒否（null＋エラー）・id/name は返る・serial の DB は引かない — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| db_queries | 2 |
| name_ok | 1 |
| serial_null | 1 |

- error: [{"message":"internal error","path":["robot","serial"],"locations":[{"line":1,"column":40}]}]

### OPERATOR: serial 拒否（ADMIN 未満） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| serial_null | 1 |

- error: [{"message":"internal error","path":["robot","serial"],"locations":[{"line":1,"column":40}]}]

## Verdict

認可はフィールド単位に @auth(requires:) で宣言し、解決の前にロールを検査する（拒否時は DB に触れない）。ロールもテナントと同様に主体から決め、GraphQL 引数には置かない。nullable にすれば拒否は該当フィールドだけを null にする。

## 適用範囲

- MySQL 8.0 / gqlgen v0.17 / serial=@auth(requires: ADMIN) / ロールは X-Role（実運用は JWT）
- ロール強さ VIEWER<OPERATOR<ADMIN。@auth は不足なら next を呼ばず拒否
- serial は nullable。拒否時は serial だけ null になり他フィールドは返る

## 保証しない範囲・未検証

- ここはフィールド単位のロール認可のみ。行単位（この robot を操作してよいか）は別途
- ロールの出所（JWT 検証・失効）は本実験外。ここは検証済みロールが context にある前提

## 再利用できる成果物

- internal/gql: @auth ディレクティブ（authz.go）とロール context
- docs/graphql.md: 認可（フィールド単位ロール）

## 次の実験

- EXP-26 永続化クエリ allowlist・レート制限

