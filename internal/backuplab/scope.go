package backuplab

// EvidenceScope は「このバックアップ検証が何を実証したか」の水準。
//
// ★別 schema・同一 MySQL server への復元は「別環境相当」であって、
// 別 host・別 version の実証ではない。混同しないよう、水準を型で区別する。
type EvidenceScope string

const (
	// SameServerDifferentSchema: 同じ mysqld の別スキーマへ復元した（EXP-11 が実証したのはここ）。
	SameServerDifferentSchema EvidenceScope = "SAME_SERVER_DIFFERENT_SCHEMA"
	// SameVersionDifferentServer: 同じ MySQL version の別プロセス／別ホストへ復元した。
	SameVersionDifferentServer EvidenceScope = "SAME_VERSION_DIFFERENT_SERVER"
	// CrossVersion: 別の MySQL version へ復元した（8.0 → 8.4 など）。
	CrossVersion EvidenceScope = "CROSS_VERSION"
	// CrossRegion: 別リージョンへ転送して復元した。
	CrossRegion EvidenceScope = "CROSS_REGION"
	// PITR: フルバックアップ + binlog 適用による point-in-time recovery。
	PITR EvidenceScope = "PITR"
	// PhysicalBackup: 物理バックアップ（XtraBackup・スナップショット）。
	PhysicalBackup EvidenceScope = "PHYSICAL_BACKUP"
	// LiveEnvVerified: 本番相当の環境で実証済み。
	LiveEnvVerified EvidenceScope = "LIVE_ENV_VERIFIED"
)

// ScopeStatus は各水準の実証状態。
type ScopeStatus struct {
	Scope    EvidenceScope
	Verified bool
	Note     string
}

// EvidenceMatrix は EXP-11 が実証した範囲と、していない範囲を明示する。
//
// **実証していないものを「検証済み」と書かない。**
func EvidenceMatrix() []ScopeStatus {
	return []ScopeStatus{
		{SameServerDifferentSchema, true,
			"EXP-11 が実証。mysqldump --single-transaction → workerdb から workerdb2 へ復元 → " +
				"指紋一致 → アプリ起動して lease/state/migration を読み戻せた"},
		{SameVersionDifferentServer, false,
			"未検証。別プロセス／別ホストの mysqld を用意して復元する必要がある（LIVE_ENV_REQUIRED）"},
		{CrossVersion, false,
			"未検証。8.0 で取ったダンプを 8.4 等へ復元する。予約語・デフォルト・照合順序の差が出うる（LIVE）"},
		{CrossRegion, false,
			"未検証。転送中の破損・遅延は Truncate/Corrupt で模擬したが、実リージョン間転送は別（LIVE）"},
		{PITR, false,
			"未検証。フルバックアップ + binlog 適用。binlog を持っていないこの構成では測れない（LIVE）"},
		{PhysicalBackup, false,
			"未検証。XtraBackup・EBS スナップショット。crash recovery が走ることの確認が要る（LIVE）"},
		{LiveEnvVerified, false,
			"未検証。上記のいずれかを本番相当で通して初めて到達する"},
	}
}

// HashRules は content hash を作るときに固定する規則。
//
// ★これを固定しないと、同じデータでもプロセスや version で hash が変わり、
// 「行数一致・hash 不一致」の検出そのものが信用できなくなる。
func HashRules() []string {
	return []string{
		"table順: 突き合わせる表は呼び出し側が明示した順（appTables）。表ごとに独立に畳む",
		"row順: 全列を ORDER BY に並べて決定化する（物理順・挿入順に依存しない）",
		"column順: information_schema.columns の column_name 昇順に固定（宣言順・追加順に依存しない）",
		"NULL表現: NULL は \\x00NULL、空文字は \\x00 + 空 で区別する",
		"文字コード: information_schema から character_set_name を schema hash に含める",
		"collation: collation_name を schema hash に含める（照合順序が違えば別スキーマ扱い）",
		"timezone: 値は文字列として読む（ドライバの time.Time 変換を通さない）。保存側で UTC 固定",
		"decimal: 文字列として読む（float 経由の丸めを避ける）",
		"binary: バイト列はそのまま hash に流す（文字コード変換しない）",
		"JSON: 未対応（このアプリに JSON 列は無い）。使うなら canonical 化してから hash する（キー順・空白除去）",
		"generated column: generation_expression を schema hash に含める。値は通常列と同じく読む",
	}
}
