# EXP-24 gqlgen のテナント分離・複雑度 DoS・内観・エラー秘匿

| | |
| --- | --- |
| Experiment | EXP-24 / gqlgen-security |
| Starting SHA | `068530920c27` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) テナントは主体から決める（context）。スキーマに tenant 引数が無く、詐称できない。 2) 他テナントだけの id を問い合わせても、:tenant で弾かれ null（存在も漏らさない）。 3) 複雑度上限を超える深い/広いクエリは、DB に触れる前に弾かれる（クエリ数 0）。 4) 本番は内観オフ（__schema は拒否）。内部エラーは一般化して client に漏らさない。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=068530920c27+dirty |
| Started / Ended | 2026-09-07T05:22:23Z / 2026-09-07T05:22:23Z |

## Results

### テナント分離: A から B だけの id を引く → null — OK

| 数えたもの | 値 |
| --- | --- |
| leaked | 0 |
| own_visible | 1 |

- 他テナントの id は :tenant で弾かれ、存在も漏れない

### 認証なし → unauthenticated（テナント未設定では何も返さない） — OK

- error: [{"message":"unauthenticated","path":["robots"],"locations":[{"line":1,"column":3}]}]

### 複雑度 DoS: 上限超クエリは実行前に拒否（DB クエリ 0） — OK

| 数えたもの | 値 |
| --- | --- |
| db_queries | 0 |

- error: [{"message":"internal error","extensions":{"code":"COMPLEXITY_LIMIT_EXCEEDED"}}] / DB クエリ=0

### 内観: 本番オフは拒否 / 開発オンは通る — OK

- オフ: [{"message":"internal error","path":["__schema"],"locations":[{"line":1,"column":3}]}] / オン: queryType=Query

### エラー秘匿: 内部エラー文を client に出さない — OK

| 数えたもの | 値 |
| --- | --- |
| masked | 1 |

- client には unauthenticated だけ。内部の理由文は出さない

## Verdict

テナントは主体から決め（引数から取らない）、全 DB アクセスをテナント束縛 Scope に通す。深い/広いクエリは複雑度上限で実行前に弾き、本番は内観オフ・内部エラーは秘匿する。

## 適用範囲

- MySQL 8.0 / gqlgen v0.17 / 複雑度上限=200 / 内観=本番オフ
- テナントは X-Tenant（実運用は JWT 等）→ context。スキーマに tenant 引数は無い
- 複雑度は commands/robots に first 比例で計上（server.go）

## 保証しない範囲・未検証

- 複雑度の適正値は本番のクエリ形状で決める。ここは 200 の例
- 認可（このテナント内で誰が何を見られるか）は本実験外。ここはテナント境界のみ
- 永続化クエリ（allowlist）でさらに攻撃面を狭められる（本実験外）

## 再利用できる成果物

- internal/gql: テナント束縛 Scope・複雑度上限・内観トグル・エラー秘匿
- docs/graphql.md: gqlgen ベストプラクティス（セキュリティ）

## 次の実験

- なし

