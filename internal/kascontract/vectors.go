package kascontract

// Vectors は言語・DB 非依存の契約テストベクタ。
//
// 各ベクタは「操作の並び」と「期待される domain の結果」を持つ。
// **期待値はドライバの戻り値ではなく domain の型**（LeaseOutcome など）。
// MySQL と SQLite の両方に同じベクタを流し、同じ domain 結果になることを確かめる。
//
// これを docs/kas/contract-vectors.json にも書き出す（他言語の実装がそのまま使えるように）。

// Op は1つの操作。
type Op struct {
	Kind    string  `json:"kind"` // put / get / cas / lease / rollback_probe
	Key     string  `json:"key,omitempty"`
	Tenant  string  `json:"tenant,omitempty"`
	Owner   string  `json:"owner,omitempty"`
	Payload string  `json:"payload,omitempty"`
	Version int64   `json:"version,omitempty"` // CAS の期待 version
	Note    *string `json:"note,omitempty"`    // NULL と "" を区別
	Count   int64   `json:"count,omitempty"`
	Updated string  `json:"updated,omitempty"` // 日時の canonical 形式
	NowMs   int64   `json:"now_ms,omitempty"`
	TTLMs   int64   `json:"ttl_ms,omitempty"`

	// 期待される domain 結果（どれか1つ）。
	WantUpsert UpsertOutcome `json:"want_upsert,omitempty"`
	WantCAS    CASOutcome    `json:"want_cas,omitempty"`
	WantLease  LeaseOutcome  `json:"want_lease,omitempty"`
	WantFence  int64         `json:"want_fence,omitempty"`
	WantExists *bool         `json:"want_exists,omitempty"`
	WantRecord *Record       `json:"want_record,omitempty"` // get の期待値（往復の検査）
}

// Vector は名前つきの操作列。
type Vector struct {
	ID  string `json:"id"`
	Ask string `json:"ask"`
	Ops []Op   `json:"ops"`
	// Note は「MySQL と SQLite で生の戻り値は違うが domain 結果は同じ」ことの説明。
	Note string `json:"note,omitempty"`
}

func b(v bool) *bool { return &v }

// ContractVectors は最低限の契約（レビュー指示 #5 の項目）。
func ContractVectors() []Vector {
	i64max := int64(9223372036854775807)
	i64min := int64(-9223372036854775808)
	return []Vector{
		{
			ID:   "put-insert-then-update-then-unchanged",
			Ask:  "PUT の insert / update / unchanged を区別できるか",
			Note: "MySQL の影響行数は 1/2/0、SQLite は 1/1/1。存在確認で正規化するので domain 結果は一致する。",
			Ops: []Op{
				{Kind: "put", Key: "a", Payload: "v1", WantUpsert: UpsertInserted},
				{Kind: "put", Key: "a", Payload: "v2", WantUpsert: UpsertUpdated},
				{Kind: "put", Key: "a", Payload: "v2", WantUpsert: UpsertUnchanged},
			},
		},
		{
			ID:   "cas-success-and-conflict",
			Ask:  "CAS が成功・競合を正しく返すか",
			Note: "同値更新でも version は進む。影響行数（MySQL=0/SQLite=1）ではなく読み戻した version で判定。",
			Ops: []Op{
				{Kind: "put", Key: "c", Payload: "p0", WantUpsert: UpsertInserted},
				{Kind: "cas", Key: "c", Version: 0, Payload: "p1", WantCAS: CASSwapped},
				{Kind: "cas", Key: "c", Version: 0, Payload: "p2", WantCAS: CASStale},   // 既に version=1
				{Kind: "cas", Key: "c", Version: 1, Payload: "p1", WantCAS: CASSwapped}, // 同値でも SWAPPED
			},
		},
		{
			ID:  "cas-missing-row",
			Ask: "存在しない行への CAS は NOT_FOUND か",
			Ops: []Op{
				{Kind: "cas", Key: "nope", Version: 0, Payload: "x", WantCAS: CASMissing},
			},
		},
		{
			ID:  "get-missing-row",
			Ask: "存在しない行の GET は「無い」か",
			Ops: []Op{
				{Kind: "get", Key: "ghost", WantExists: b(false)},
			},
		},
		{
			ID:   "null-vs-empty-string",
			Ask:  "NULL と空文字を区別して往復できるか",
			Note: "SQLite は型が緩いが、note を *string 相当で扱えば NULL と \"\" は別物として往復する。",
			Ops: []Op{
				{Kind: "put", Key: "n1", Payload: "x", Note: nil, WantUpsert: UpsertInserted},
				{Kind: "put", Key: "n2", Payload: "x", Note: ptr(""), WantUpsert: UpsertInserted},
				{Kind: "get", Key: "n1", WantExists: b(true), WantRecord: &Record{Key: "n1", Payload: "x", Note: nil}},
				{Kind: "get", Key: "n2", WantExists: b(true), WantRecord: &Record{Key: "n2", Payload: "x", Note: ptr("")}},
			},
		},
		{
			ID:  "integer-boundary",
			Ask: "int64 の上限・下限を往復できるか",
			Ops: []Op{
				{Kind: "put", Key: "imax", Payload: "x", Count: i64max, WantUpsert: UpsertInserted},
				{Kind: "put", Key: "imin", Payload: "x", Count: i64min, WantUpsert: UpsertInserted},
				{Kind: "get", Key: "imax", WantExists: b(true), WantRecord: &Record{Key: "imax", Payload: "x", Count: i64max}},
				{Kind: "get", Key: "imin", WantExists: b(true), WantRecord: &Record{Key: "imin", Payload: "x", Count: i64min}},
			},
		},
		{
			ID:   "datetime-canonical",
			Ask:  "日時を canonical 形式で往復できるか",
			Note: "SQLite に日時型は無いので、必ず RFC3339 UTC millis の文字列で入れる（EXP-10 の教訓）。",
			Ops: []Op{
				{Kind: "put", Key: "t1", Payload: "x", Updated: "2026-01-02T03:04:05.678Z", WantUpsert: UpsertInserted},
				{Kind: "get", Key: "t1", WantExists: b(true), WantRecord: &Record{Key: "t1", Payload: "x", UpdatedA: "2026-01-02T03:04:05.678Z"}},
			},
		},
		{
			ID:   "lease-lifecycle",
			Ask:  "リースの ACQUIRED / HELD_BY_OTHER / STALE_FENCE / NOT_FOUND",
			Note: "RowsAffected ではなく担当・期限・fence を読んで正規化する。",
			Ops: []Op{
				{Kind: "lease", Tenant: "t", Owner: "A", NowMs: 1000, TTLMs: 100, WantLease: LeaseAcquired, WantFence: 1},
				{Kind: "lease", Tenant: "t", Owner: "B", NowMs: 1050, TTLMs: 100, WantLease: LeaseHeldByOther, WantFence: 1},
				{Kind: "lease", Tenant: "t", Owner: "B", NowMs: 2000, TTLMs: 100, WantLease: LeaseAcquired, WantFence: 2}, // A の期限切れ後に奪取 → fence+1
			},
		},
	}
}

// LiveOnlyItems は「最低限」のうち、in-process のベクタでは実証しきれず
// 別の live 実験に委ねる項目（実行したふりをしない）。
func LiveOnlyItems() map[string]string {
	return map[string]string{
		"busy/lock":            "EXP-10（別プロセス競合・DEFERRED 昇格）で実測。SQLITE_BUSY と MySQL の lock wait は文言が違うため classifyBusy で正規化する",
		"transaction rollback": "EXP-10（ddl-rollback）と各実験の tx テストで実証済み。DDL のトランザクション性は engine 差あり（DIFFERENT_MECHANISM）",
		"WAL checkpoint":       "SQLite 固有。EXP-10 のバックアップ（.db だけコピーで WAL 上のコミットを失う）で実測。MySQL には概念が無い",
		"crash/restart":        "EXP-1（外部 effect 途中の SIGKILL）・EXP-6（マイグレーション crash）・EXP-11（バックアップ復元）で実測。fence の revision は本ベクタの lease-lifecycle で担保",
		"append-only trigger":  "EXP-10（append-only-trigger）で実測。MySQL は SIGNAL、SQLite は RAISE(ABORT)。DIFFERENT_MECHANISM",
	}
}
