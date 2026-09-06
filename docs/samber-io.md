# samber/lo・samber/mo（IO）の使いどころと、層の線引き

結論から:

- **`samber/lo`（map/slice ユーティリティ・Ternary）** → **app / usecase / handler 層で使ってよい。**
- **`samber/mo` の `IO` / `Result` / `Option`** → **DB 境界（repo/domain/store/model）に置かない。**
- 便利ユーティリティの置き場は **`internal/appx`**（app 層専用）。lo に無い小物だけ貯める。
- この線引きは **`internal/lint` の `layerimport` 検査**で機械的に守る（repo/domain が
  lo・mo・appx を import したら指摘）。

## なぜ mo.IO を repository 層に入れないか（実証: `internal/moio`）

`mo.IO[R]` は「副作用を遅延した値」。`Run() R` を呼ぶと実行される。
DB 境界に置くと、この repo が EXP-1/EXP-2 で守っている性質を覆い隠す。
`internal/moio/moio_test.go` が**動くコードで**示している:

| 検査 | 何が起きるか |
| --- | --- |
| `Run はerrorを返さない` | `mo.IO[R].Run()` の戻りは `R` だけ。失敗がゼロ値に潰れ、「エラー」と「空の結果」が区別できない |
| `IOEitherでも素のGoから離れる` | `IOEither.Run()` は `Either[error,R]`。error は運べるが `(R, error)`+`database/sql` の規約から外れ、毎回変換が要る／`errors.Is` に一手間 |
| `contextが遅延に埋もれる` | 「組み立て」と「Run」がずれる設計なので、その間の ctx キャンセル/期限の扱いが遅延の内側に隠れる |
| `OUTCOME_UNKNOWNを潰す` | timeout（EXP-1 の「effect は起きたが結果不明」）を、`R` のゼロ値＝「空の結果」に潰す。EXP-1 の禁止事項に抵触 |
| `app層の純粋な組み立てなら可` | 副作用の無い値の合成なら、潰す相手（error/ctx）が居ないので問題ない |

要するに **DB の戻り値の「意味」（error・ctx・fencing）を汎用型で覆わない。**
EXP-10 の `kascontract` でやったのと同じ思想: ドライバの戻り値ではなく**ドメインの結果型**で意味を持つ。
`mo.Result[int]` を lease の結果にすると `LeaseOutcome`（ACQUIRED/HELD_BY_OTHER/STALE_FENCE/NOT_FOUND）が消える。

## lo はなぜ app 層なら良いか

`lo.Map` / `lo.Filter` / `lo.Uniq` / `lo.GroupBy` / `lo.Ternary` は**純粋な変換**。
DB の意味を覆わない。app 層で「取ってきた行を整形する」繋ぎコードの手数を確実に減らす。

Go に三項演算子が無いぶんは `lo.Ternary` / `appx.If` で埋める（`appx.IfF` は遅延評価）。

## `internal/appx`（ユーティリティ置き場）

lo をそのまま使ってよいので、appx には**lo に無い/何度も書く小物**だけ貯める:

| 関数 | 用途 |
| --- | --- |
| `If` / `IfF` | 三項演算子の代わり（IfF は選ばれた側だけ評価） |
| `Coalesce` | 最初の非ゼロ値（環境変数のフォールバック） |
| `KeyByID` | GetMany の後、ID で引き当てる map 化 |
| `Partition` | 述語で2分割 |
| `ChunkIDs` | IN 句の上限で分割（size<=0 を安全側に倒す） |

## 検査で守る

`layerimport` 検査（`internal/lint`）が、repo/domain/store/model 層での
`samber/lo`・`samber/mo`・`internal/appx` の import を指摘する。
逃げ道は `//smlint:allow layerimport 理由: ...`。
「規約」を口で言うのではなく、EXP-9 と同じく機械で守る。
