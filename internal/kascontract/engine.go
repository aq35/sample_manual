package kascontract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Engine は KAS が要求する意味を、あるデータベースの上で満たす実装。
//
// ★戻り値はすべて domain の型（LeaseOutcome など）。ドライバの生の値は外に出さない。
// MySQL 実装と SQLite 実装が同じ Engine を満たし、同じ vector に同じ domain 結果を返す。
type Engine interface {
	Name() string
	Setup(ctx context.Context) error
	Reset(ctx context.Context) error

	// Put は「無ければ入れる、あれば更新する」。insert/update の区別は
	// **事前の存在確認**で決める（影響行数に頼らない）。
	Put(ctx context.Context, r Record) (UpsertOutcome, error)
	Get(ctx context.Context, key string) (Record, bool, error)

	// CAS は version による compare-and-swap。SWAPPED/STALE の判定は
	// **読み戻した version** で決める（影響行数に頼らない。同値更新でも壊れない）。
	CAS(ctx context.Context, key string, expectVersion int64, newPayload string) (CASOutcome, error)

	// AcquireLease は fencing 付きのリース取得。RowsAffected ではなく
	// 「今の担当・期限・fence」を読んで domain の結果へ正規化する。
	AcquireLease(ctx context.Context, tenant, owner string, nowMillis, ttlMillis int64) (LeaseOutcome, int64, error)
}

// dialect は engine 間で違う最小限の SQL 片。
type dialect struct {
	name        string
	createState string
	createLease string
	intType     string
}

type sqlEngine struct {
	db *sql.DB
	d  dialect
}

func (e *sqlEngine) Name() string { return e.d.name }

func (e *sqlEngine) Setup(ctx context.Context) error {
	for _, ddl := range []string{e.d.createState, e.d.createLease} {
		if _, err := e.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("%s setup: %w", e.d.name, err)
		}
	}
	return nil
}

func (e *sqlEngine) Reset(ctx context.Context) error {
	for _, t := range []string{"kas_state", "kas_lease"} {
		//smlint:allow rowsaffected 理由: 後片付け。消える行が 0 でも正しい
		//smlint:allow loopquery 理由: 固定の2表を空にするだけ
		if _, err := e.db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	return nil
}

func (e *sqlEngine) Get(ctx context.Context, key string) (Record, bool, error) {
	var r Record
	var note sql.NullString
	err := e.db.QueryRowContext(ctx,
		"SELECT k, version, payload, note, cnt, updated_a FROM kas_state WHERE k = ?", key).
		Scan(&r.Key, &r.Version, &r.Payload, &note, &r.Count, &r.UpdatedA)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if note.Valid {
		r.Note = &note.String
	}
	return r, true, nil
}

func (e *sqlEngine) Put(ctx context.Context, r Record) (UpsertOutcome, error) {
	// ★insert/update の区別は存在確認で決める（影響行数では engine 間で区別できない）。
	prev, existed, err := e.Get(ctx, r.Key)
	if err != nil {
		return "", err
	}
	if !existed {
		if _, err := e.db.ExecContext(ctx,
			"INSERT INTO kas_state (k, version, payload, note, cnt, updated_a) VALUES (?,?,?,?,?,?)",
			r.Key, r.Version, r.Payload, nullString(r.Note), r.Count, r.UpdatedA); err != nil {
			return "", err
		}
		return UpsertInserted, nil
	}
	if prev.Payload == r.Payload && sameNote(prev.Note, r.Note) && prev.Count == r.Count {
		return UpsertUnchanged, nil
	}
	//smlint:allow rowsaffected 理由: 変化の有無は事前の Get で判定済み。ここは確定した更新
	if _, err := e.db.ExecContext(ctx,
		"UPDATE kas_state SET payload = ?, note = ?, cnt = ?, updated_a = ? WHERE k = ?",
		r.Payload, nullString(r.Note), r.Count, r.UpdatedA, r.Key); err != nil {
		return "", err
	}
	return UpsertUpdated, nil
}

func (e *sqlEngine) CAS(ctx context.Context, key string, expect int64, newPayload string) (CASOutcome, error) {
	cur, ok, err := e.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return CASMissing, nil
	}
	if cur.Version != expect {
		return CASStale, nil
	}
	// version 一致。更新して version を1つ進める。
	// ★判定は影響行数ではなく「読み戻した version が expect+1 か」で行う。
	// 同じ payload に更新しても（同値更新でも）version は必ず進むので、engine 差に強い。
	//smlint:allow rowsaffected 理由: 影響行数では同値更新のとき engine 間で 0/1 が割れる。読み戻しで判定する
	if _, err := e.db.ExecContext(ctx,
		"UPDATE kas_state SET version = version + 1, payload = ? WHERE k = ? AND version = ?",
		newPayload, key, expect); err != nil {
		return "", err
	}
	after, _, err := e.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if after.Version == expect+1 {
		return CASSwapped, nil
	}
	return CASStale, nil
}

func (e *sqlEngine) AcquireLease(ctx context.Context, tenant, owner string, now, ttl int64) (LeaseOutcome, int64, error) {
	// 現在の担当を読む。無ければ新規取得。
	var (
		curOwner string
		expires  int64
		fence    int64
	)
	err := e.db.QueryRowContext(ctx,
		"SELECT owner, expires_at, fence FROM kas_lease WHERE tenant = ?", tenant).
		Scan(&curOwner, &expires, &fence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 新規。fence=1 から。
		//smlint:allow rowsaffected 理由: 新規取得。存在しないことは上の SELECT で確認済み
		if _, err := e.db.ExecContext(ctx,
			"INSERT INTO kas_lease (tenant, owner, expires_at, fence) VALUES (?,?,?,1)",
			tenant, owner, now+ttl); err != nil {
			return "", 0, err
		}
		return LeaseAcquired, 1, nil
	case err != nil:
		return "", 0, err
	}
	// 既存の担当が居る。
	switch {
	case curOwner == owner && expires > now:
		// 自分が既に持っている → 期限を伸ばす（fence は据え置き）
		//smlint:allow rowsaffected 理由: 自分の担当の更新。存在は確認済み
		if _, err := e.db.ExecContext(ctx,
			"UPDATE kas_lease SET expires_at = ? WHERE tenant = ? AND owner = ?",
			now+ttl, tenant, owner); err != nil {
			return "", 0, err
		}
		return LeaseAcquired, fence, nil
	case expires > now:
		// 有効な別の担当が居る → 取れない
		return LeaseHeldByOther, fence, nil
	default:
		// 期限切れ → 奪取。fence を +1 して「担当が変わった」ことを示す。
		newFence := fence + 1
		//smlint:allow rowsaffected 理由: 期限切れの奪取。version(fence) を進めることが目的
		if _, err := e.db.ExecContext(ctx,
			"UPDATE kas_lease SET owner = ?, expires_at = ?, fence = ? WHERE tenant = ? AND fence = ?",
			owner, now+ttl, newFence, tenant, fence); err != nil {
			return "", 0, err
		}
		return LeaseAcquired, newFence, nil
	}
}

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func sameNote(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}

// ---- engine の作り方 ----

// MySQLEngine は *sql.DB（MySQL）を Engine にする。
func MySQLEngine(db *sql.DB) Engine {
	return &sqlEngine{db: db, d: dialect{
		name: "mysql",
		createState: `CREATE TABLE IF NOT EXISTS kas_state (
			k VARCHAR(64) NOT NULL PRIMARY KEY,
			version BIGINT NOT NULL DEFAULT 0,
			payload VARCHAR(255) NOT NULL,
			note VARCHAR(255) NULL,
			cnt BIGINT NOT NULL DEFAULT 0,
			updated_a VARCHAR(32) NOT NULL DEFAULT ''
		) ENGINE=InnoDB`,
		createLease: `CREATE TABLE IF NOT EXISTS kas_lease (
			tenant VARCHAR(64) NOT NULL PRIMARY KEY,
			owner VARCHAR(64) NOT NULL,
			expires_at BIGINT NOT NULL,
			fence BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB`,
	}}
}

// SQLiteEngine は *sql.DB（SQLite）を Engine にする。
func SQLiteEngine(db *sql.DB) Engine {
	return &sqlEngine{db: db, d: dialect{
		name: "sqlite",
		createState: `CREATE TABLE IF NOT EXISTS kas_state (
			k TEXT NOT NULL PRIMARY KEY,
			version INTEGER NOT NULL DEFAULT 0,
			payload TEXT NOT NULL,
			note TEXT,
			cnt INTEGER NOT NULL DEFAULT 0,
			updated_a TEXT NOT NULL DEFAULT ''
		)`,
		createLease: `CREATE TABLE IF NOT EXISTS kas_lease (
			tenant TEXT NOT NULL PRIMARY KEY,
			owner TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			fence INTEGER NOT NULL DEFAULT 0
		)`,
	}}
}

func ptr(s string) *string { return &s }

// canonicalError は engine 間で文言の違うエラーを domain の分類へ。
func classifyBusy(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "lock wait timeout") ||
		strings.Contains(s, "deadlock")
}

var _ = ptr
var _ = classifyBusy
