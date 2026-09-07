# gqlgen で GraphQL を書くときのベストプラクティス

GraphQL は「1エンドポイントに、クライアントが好きな形・深さのクエリを投げる」。
だから REST 以上に、**パフォーマンス（N+1・過大なクエリ）** と
**セキュリティ（テナント越え・DoS・情報漏れ）** をサーバ側で締める必要がある。

実装は [internal/gql](../internal/gql)（gqlgen サーバ一式）。実証は
[EXP-23（パフォーマンス）](results/exp-23/exp-23-gqlgen-performance.md)と
[EXP-24（セキュリティ）](results/exp-24/exp-24-gqlgen-security.md)。

## パフォーマンス

### 1. N+1 は DataLoader で畳む（EXP-23）

一覧の各要素で子フィールド（`Robot.commands`）を解決すると、DataLoader が無ければ
1 台ごとに DB を引く（N+1）。DataLoader を入れると `robot_id` をまとめた 1 クエリに畳む。

| 方式 | DB クエリ | レイテンシ |
| --- | --- | --- |
| ローダ無し（N+1） | **51**（robots 1 + 50 台ぶん） | 24ms |
| DataLoader | **2**（robots 1 + commands 1） | 7.5ms |

- 子フィールドは**必ず**ローダ越しに引く（`internal/gql/loader.go`、`dataloadgen`）。
- ローダは**リクエストごと**に作る。使い回すと、別リクエスト・別テナントの結果が混ざる
  （後述）。

### 2. 要求された列だけ引く（射影・EXP-19/21）

`name` は別表 `robot_profile`。フィールドリゾルバにしておけば、クライアントが `name` を
要求しない限り解決関数は呼ばれず、profile を引かない。

| クエリ | DB クエリ |
| --- | --- |
| `robots{ id }`（name 無し） | 1 |
| `robots{ id name }`（name 有り） | 51（1 + 50 の name） |

- `SELECT *` に相当することをしない。要求スカラーだけ投影する（[列の重さ](column-projection.md)）。
- name も件数が多ければ別ローダで畳む（本実験では素朴解決）。

### 3. 一覧は必ず上限つき keyset（EXP-7）

`robots(first: Int!)` は **first を必須**にし、サーバ側で `MaxPageSize`（既定 100）に
頭打ちする。`first: 100000` と要求されても 100 で返す。ページングは OFFSET でなく
keyset（`robot_id > after`）。無制限一覧を作らせない。

### 4. 実行前にコストを見る

深いクエリは複雑度で弾く（次節）。個別の重い検索は
[クエリコストゲート](query-cost-gate.md)（`GuardedQuery`）と併用できる。

## セキュリティ

### 5. テナントは主体から決める。引数から取らない（EXP-24）

- スキーマに `tenant` 引数を**置かない**。テナントは認証（JWT/セッション）から取り、
  検証済みの値を context に載せる（`WithTenant`）。リゾルバはそこからしか読まない。
- 全 DB アクセスを**テナント束縛 Scope**（`repo.Scope`・`:tenant` 束縛）に通す。
  `tenant_id` を書き忘れた SQL は構造的に書けない。
- 実測: 他テナントだけに在る id を問い合わせても `null`（存在も漏らさない）。

```
robot(id: "他テナントの id")  →  null   （:tenant で弾かれる）
テナント未設定（未認証）        →  "unauthenticated"（黙って全件を返さない）
```

- **DataLoader をテナントで閉じる**: ローダはリクエストのテナント Scope で作るので、
  キーが `robot_id` だけでもバッチは必ずそのテナント内に閉じる。ローダをリクエスト跨ぎ
  で共有したり、テナント非依存にキャッシュすると他テナントの結果が混ざる。

### 6. 複雑度上限で DoS を防ぐ（EXP-24）

`commands(first:100)` を 100 台ぶん…のような深い/広いクエリは、複雑度上限
（`FixedComplexityLimit`）で**実行前に**弾く。フィールドの複雑度は取得件数 `first` に
比例させる（`server.go` の `Complexity`）。

- 実測: 上限超クエリは **DB に 1 度も触れずに** 拒否（DB クエリ = 0）。

### 7. 認可はフィールド単位に `@auth` で（EXP-25）

テナント分離（§5）が「どのテナントのデータか」なら、認可は「そのテナント内で、この主体は
何を見てよいか」。ロールもテナントと同じく**主体から決める**（引数に置かない）。

スキーマにディレクティブを宣言し、機微なフィールドに付ける:

```graphql
directive @auth(requires: Role!) on FIELD_DEFINITION
type Robot {
  serial: String @auth(requires: ADMIN)   # ADMIN だけ。権限が無ければ null＋エラー
}
```

`@auth` は**解決関数の前に**走る（`internal/gql/authz.go`）。実測:

| ロール | serial | その robot の DB クエリ |
| --- | --- | --- |
| ADMIN | 見える | 3（robot + name + serial） |
| VIEWER / OPERATOR | null＋エラー | **2**（serial は引かない） |

- 拒否時は serial 用の DB を**引かない**（ディレクティブが `next` を呼ばない）。
- `serial` を nullable にすると、拒否は**そのフィールドだけ**を null にし、id/name は返る。
- ここはフィールド単位のロール認可。行単位（この robot を操作してよいか）は別途。

### 8. 受付制御: 永続化クエリ allowlist とレート制限（EXP-26）

複雑度上限（§6）が「1クエリの重さ」を抑えるのに対し、受付段でも締める。

- **永続化クエリ allowlist**（`internal/gql/allowlist.go`）: 事前登録したクエリ以外は
  実行しない。APQ が任意クエリをキャッシュするだけなのと違い、allowlist は**未登録を拒否**
  する（1文字違う無害なクエリも通さない）。本番クライアントのクエリは有限個なので、その
  ハッシュを登録しておく。攻撃者が自由な深い/広いクエリを投げる余地を消す。
- **レート制限**（`internal/gql/ratelimit.go`）: テナントごとのトークンバケツ。実測で
  burst=3 → 3 本通り 4 本目拒否、時間が経てば補充されて再び通る。**テナント単位**なので、
  1 テナントの暴走が別テナントを巻き込まない。分散環境ではバケツを共有ストア（Redis 等）へ。

### 9. 本番は内観オフ・エラーは秘匿

- `Introspection` は本番で切る（攻撃者にスキーマを教えない）。開発ではオン。
- `SetErrorPresenter` で内部エラー（SQL・スタック）を client に出さない。想定内のもの
  （未認証・過大リクエスト）だけ短いメッセージに正規化し、詳細はサーバログへ。

## まとめ（チェックリスト）

- [ ] 子フィールドは DataLoader 越し（リクエストごと・テナント束縛）
- [ ] フィールドリゾルバで射影（要求列だけ引く・`SELECT *` しない）
- [ ] 一覧は `first` 必須・`MaxPageSize` で頭打ち・keyset
- [ ] テナントは context から（引数に置かない）・全 DB は `repo.Scope` 経由
- [ ] 認可は `@auth(requires:)` でフィールド単位（ロールも主体から・解決前に検査）
- [ ] 複雑度上限（first 比例）で深い/広いクエリを実行前に拒否
- [ ] 永続化クエリ allowlist で未登録クエリを実行させない
- [ ] テナント単位のレート制限（1 テナントの暴走を波及させない）
- [ ] 本番は内観オフ・エラーは秘匿・パース済みクエリはキャッシュ

## 再生成

スキーマ（`internal/gql/schema.graphqls`）を変えたら:

```
go run github.com/99designs/gqlgen generate
```

## 適用範囲・保証しない範囲

- gqlgen v0.17 / MySQL 8.0。複雑度の適正値は本番のクエリ形状で決める（本実験は 200 の例）。
- 認可（同一テナント内で誰が何を見られるか）は本実験外。ここはテナント境界のみ。
- 永続化クエリ（allowlist）・レート制限でさらに攻撃面を狭められる（本実験外）。
- gqlgen 追加で本リポジトリの Go 下限が 1.25 に上がった（`golang.org/x/tools` の要求）。
