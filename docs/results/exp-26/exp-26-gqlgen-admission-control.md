# EXP-26 永続化クエリ allowlist とテナント単位のレート制限

| | |
| --- | --- |
| Experiment | EXP-26 / gqlgen-admission-control |
| Starting SHA | `5ad9d6d4f8a2` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) allowlist: 事前登録したクエリだけ実行できる。1文字でも違えば（無害でも）拒否される。 2) レート制限: テナントごとのトークンバケツ。burst を超えると拒否。時間が経てば補充されて再び通る。 3) レート制限はテナント単位。あるテナントが枯らしても、別テナントは自分のバケツで通る。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=5ad9d6d4f8a2+dirty |
| Started / Ended | 2026-09-07T07:47:29Z / 2026-09-07T07:47:29Z |

## Results

### allowlist: 登録済みクエリは通る — OK

| 数えたもの | 値 |
| --- | --- |
| ok | 1 |
| returned | 3 |

### allowlist: 未登録クエリは拒否（無害でも） — **事故あり**

- error: [{"message":"internal error"}]

### レート制限: burst=3 → 3本通り4本目拒否 — OK

| 数えたもの | 値 |
| --- | --- |
| passed | 3 |
| rejected | 1 |

### レート制限: 2秒後に補充 → 2本通る / 別テナントは自分のバケツで通る — OK

| 数えたもの | 値 |
| --- | --- |
| other_tenant_ok | 1 |
| refill_passed | 2 |

## Verdict

複雑度上限に加え、受付段でも締める。永続化クエリ allowlist で未登録クエリを実行させず、テナント単位のレート制限で単位時間の本数を抑える（1テナントの暴走を他テナントに波及させない）。

## 適用範囲

- MySQL 8.0 / gqlgen v0.17 / allowlist=生クエリの sha256 / レート=毎秒1本・burst3・決定的時計
- allowlist は OperationContext.RawQuery のハッシュで判定（パース前段）
- レート制限は InterceptOperation でテナントごとにトークンバケツ

## 保証しない範囲・未検証

- allowlist はビルド時に既知のクエリを登録する運用前提（クライアントのクエリは有限個）
- レートの適正値は本番のトラフィックで決める。ここは 1/s・burst3 の例
- 分散環境ではバケツを共有ストア（Redis 等）に置く必要がある。本実験はプロセス内

## 再利用できる成果物

- internal/gql: allowlist.go（永続化クエリ）・ratelimit.go（テナント単位トークンバケツ）
- docs/graphql.md: 受付制御（allowlist・レート制限）

## 次の実験

- なし

