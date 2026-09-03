// Package kascontract は EXP-10 の実験結果を、言語・DB 非依存の「契約」に変える。
//
// ★このパッケージの主張:
// **ドライバの戻り値を domain semantics にしない。**
//
// EXP-10 で分かったとおり、同じ SQL でも MySQL と SQLite は違う値を返す
// （同値 UPDATE の影響行数が 0 対 1、UPSERT の 1/2/0 対 1/1/1 など）。
// これをそのまま `if affected == 1` のように使うと、engine を替えた瞬間に壊れる。
//
// だから repository の内側で、ドライバの戻り値を**domain の結果型**へ正規化する。
// 上位（KAS のユースケース）は domain の結果型だけを見る。
// 例: RowsAffected 0/1 を直接使わず、LeaseOutcome へ正規化する。
package kascontract

// LeaseOutcome は「担当を取りに行った」結果。
//
// ★これが契約の核。RowsAffected では表せない意味を、型で表す。
type LeaseOutcome string

const (
	LeaseAcquired    LeaseOutcome = "ACQUIRED"      // 自分が担当になった（新規 or 期限切れを奪取）
	LeaseHeldByOther LeaseOutcome = "HELD_BY_OTHER" // 有効な別の担当が居る
	LeaseStaleFence  LeaseOutcome = "STALE_FENCE"   // 自分の fence が古い（追い越された）
	LeaseNotFound    LeaseOutcome = "NOT_FOUND"     // 対象テナントが無い
)

// CASOutcome は楽観ロック（version による compare-and-swap）の結果。
//
// ★同値更新のとき、MySQL の影響行数は 0、SQLite は 1。
// どちらも「version が一致し、書き込みは確定した」なら SWAPPED にする。
// 影響行数ではなく、**version が進んだかどうか**で判定する。
type CASOutcome string

const (
	CASSwapped CASOutcome = "SWAPPED"   // 期待 version と一致し、更新が確定した
	CASStale   CASOutcome = "STALE"     // 期待 version と違う（誰かが先に更新した）
	CASMissing CASOutcome = "NOT_FOUND" // 行が無い
)

// UpsertOutcome は UPSERT の結果。
//
// ★MySQL は挿入=1/更新=2/変化なし=0 を返すが、SQLite は全部 1。
// 影響行数では insert/update を区別できないので、**事前の存在確認**で正規化する。
// 「区別する必要が無いなら区別しない」ことも契約の一部（PUT の冪等性）。
type UpsertOutcome string

const (
	UpsertInserted  UpsertOutcome = "INSERTED"
	UpsertUpdated   UpsertOutcome = "UPDATED"
	UpsertUnchanged UpsertOutcome = "UNCHANGED"
)

// TxOutcome はトランザクションの結末。
type TxOutcome string

const (
	TxCommitted  TxOutcome = "COMMITTED"
	TxRolledBack TxOutcome = "ROLLED_BACK"
)

// Record は1つの状態行（KAS が保存する最小単位）。
// 値の往復（NULL・整数境界・日時）を engine 間で揃えられるかを見るために使う。
type Record struct {
	Key      string
	Version  int64
	Payload  string
	Note     *string // NULL と空文字を区別するため *string
	Count    int64   // 整数境界の往復用
	UpdatedA string  // 日時の canonical 形式（RFC3339 UTC millis）
}
