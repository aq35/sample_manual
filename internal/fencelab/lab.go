// Package fencelab は EXP-2（lease / fencing / clock skew）の実験本体。
//
// 守りたい不変条件は4つ。
//
//  1. lease を失った worker は、それ以降 commit できない
//  2. 新しい担当の fence より古い write は拒否される
//  3. プロセスの自己申告時刻だけで lease を奪えない
//  4. 同時に claim したとき、勝者は1つ
//
// 「たいてい大丈夫」では意味がない。壊す条件（時計のずれ・停止・遅延・kill）を
// 明示的に作って、そのうえで上の4つが保たれることを確かめる。
package fencelab

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// Mode は比較する方式。
type Mode string

const (
	// ModeNoLease: 担当を決めない。全員が書く（事故の再現）。
	ModeNoLease Mode = "no_lease"
	// ModeLocalClock: 期限の判定を **自分の時計** で行う lease。
	// 時計が進んだプロセスは、生きている lease を「切れている」と見なして奪える。
	ModeLocalClock Mode = "lease_local_clock"
	// ModeDBClock: 期限の判定を **DB の時計** で行う lease（時計を1つに集約する）。
	ModeDBClock Mode = "lease_db_clock"
	// ModeFencing: DB 時計の lease ＋ 書き込みに fence を添えて古い書き込みを拒否する（推奨）。
	ModeFencing Mode = "lease_db_clock_fencing"
	// ModeGetLock: 接続占有型のロックで担当を表す（比較用）。
	ModeGetLock Mode = "get_lock"
)

// Modes は実験で回す順。
var Modes = []Mode{ModeNoLease, ModeLocalClock, ModeDBClock, ModeFencing}

var ErrNotOwner = errors.New("担当ではない")

// Worker は1プロセスぶんの担当者。
type Worker struct {
	db     *sql.DB
	tenant string
	owner  string
	mode   Mode
	ttl    time.Duration

	// skew はこのプロセスの時計のずれ（自己申告時刻に足される）。
	skew time.Duration

	fence    uint64
	lockConn *sql.Conn // ModeGetLock のときだけ使う（接続を占有する）
}

func NewWorker(db *sql.DB, tenant, owner string, mode Mode, ttl, skew time.Duration) *Worker {
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	return &Worker{db: db, tenant: tenant, owner: owner, mode: mode, ttl: ttl, skew: skew}
}

// Now はこのプロセスが「今」だと思っている時刻（ずれ込み）。
func (w *Worker) Now() time.Time { return time.Now().Add(w.skew) }

// Fence は現在持っている fence 番号。
func (w *Worker) Fence() uint64 { return w.fence }
func (w *Worker) Owner() string { return w.owner }

// Migrate は実験用の表を作る。
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		var lines []string
		for _, ln := range strings.Split(stmt, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		s := strings.TrimSpace(strings.Join(lines, "\n"))
		if s == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// Reset はテナントの行を消し、書き込み対象の行を1つ用意する。
func Reset(ctx context.Context, db *sql.DB, tenant string) error {
	for _, t := range []string{"fence_lease", "fence_state", "fence_audit"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO fence_state (tenant_id, k, v, fence, writer) VALUES (?, 'counter', 0, 0, 'init')`, tenant)
	return err
}

// Acquire は担当を取りに行く。取れたら fence 番号を返す。
func (w *Worker) Acquire(ctx context.Context) (bool, error) {
	switch w.mode {
	case ModeNoLease:
		w.fence = 0
		return true, nil // 誰でも書ける

	case ModeLocalClock:
		return w.acquireLocalClock(ctx)

	case ModeGetLock:
		return w.acquireGetLock(ctx)

	default: // ModeDBClock / ModeFencing
		return w.acquireDBClock(ctx)
	}
}

// acquireDBClock は期限の判定を DB の時計（NOW(3)）で行う。
//
// ★ここが要点。「切れているかどうか」を判断する時計を1つに決める。
// 各プロセスの時計を使うと、ずれたプロセスが生きている lease を奪える。
func (w *Worker) acquireDBClock(ctx context.Context) (bool, error) {
	const q = `
INSERT INTO fence_lease (tenant_id, owner, expires_at, fence)
VALUES (?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), 1) AS new
ON DUPLICATE KEY UPDATE
  fence      = fence_lease.fence + IF(fence_lease.owner <> new.owner AND fence_lease.expires_at <= NOW(3), 1, 0),
  owner      = IF(fence_lease.owner = new.owner OR fence_lease.expires_at <= NOW(3), new.owner, fence_lease.owner),
  expires_at = IF(fence_lease.owner = new.owner, new.expires_at, fence_lease.expires_at)`
	if _, err := w.db.ExecContext(ctx, q, w.tenant, w.owner, w.ttl.Microseconds()); err != nil {
		return false, err
	}
	owner, fence, _, err := w.readLease(ctx)
	if err != nil {
		return false, err
	}
	if owner != w.owner {
		return false, nil
	}
	w.fence = fence
	return true, nil
}

// acquireLocalClock は **自分の時計** で期限を判定する（壊れる方）。
func (w *Worker) acquireLocalClock(ctx context.Context) (bool, error) {
	owner, fence, expires, err := w.readLease(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := w.db.ExecContext(ctx,
			`INSERT INTO fence_lease (tenant_id, owner, expires_at, fence) VALUES (?,?,?,1)`,
			w.tenant, w.owner, w.Now().UTC().Add(w.ttl))
		if err != nil {
			return false, err
		}
		w.fence = 1
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// ★自己申告の時刻で「切れている」と判断する
	if owner != w.owner && expires.After(w.Now().UTC()) {
		return false, nil
	}
	newFence := fence
	if owner != w.owner {
		newFence++
	}
	//smlint:allow rowsaffected 理由: EXP-2 の実験対象。担当の書き込みを1件ずつ測っている
	if _, err := w.db.ExecContext(ctx,
		`UPDATE fence_lease SET owner = ?, expires_at = ?, fence = ? WHERE tenant_id = ?`,
		w.owner, w.Now().UTC().Add(w.ttl), newFence, w.tenant); err != nil {
		return false, err
	}
	w.fence = newFence
	return true, nil
}

// acquireGetLock は接続占有型のロックで担当を表す（比較用）。
func (w *Worker) acquireGetLock(ctx context.Context) (bool, error) {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	// ★この接続を持ち続けている間だけ担当。閉じると解放される。
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", "fencelab."+w.tenant).Scan(&got); err != nil {
		_ = conn.Close()
		return false, err
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return false, nil
	}
	w.lockConn = conn
	w.fence = 0 // 接続占有型のロックには fence が無い（世代が表せない）
	return true, nil
}

// Renew は担当を延長する。担当を失っていれば false。
func (w *Worker) Renew(ctx context.Context) (bool, error) {
	switch w.mode {
	case ModeNoLease:
		return true, nil
	case ModeGetLock:
		return w.lockConn != nil, nil
	case ModeLocalClock:
		res, err := w.db.ExecContext(ctx,
			`UPDATE fence_lease SET expires_at = ? WHERE tenant_id = ? AND owner = ?`,
			w.Now().UTC().Add(w.ttl), w.tenant, w.owner)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n == 1, nil
	default:
		res, err := w.db.ExecContext(ctx,
			`UPDATE fence_lease SET expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND)
			  WHERE tenant_id = ? AND owner = ? AND expires_at > NOW(3)`,
			w.ttl.Microseconds(), w.tenant, w.owner)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n == 1, nil
	}
}

// Write は担当としての書き込み。accepted=false なら拒否された。
func (w *Worker) Write(ctx context.Context, value int64) (bool, error) {
	var (
		accepted bool
		note     string
	)
	switch w.mode {
	case ModeFencing:
		// ★fence を添えて書く。古い担当の書き込みはここで落ちる。
		// 「lease を持っているつもり」かどうかに関係なく、DB が番号で判断する。
		res, err := w.db.ExecContext(ctx,
			`UPDATE fence_state SET v = ?, fence = ?, writer = ?
			  WHERE tenant_id = ? AND k = 'counter' AND fence <= ?`,
			value, w.fence, w.owner, w.tenant, w.fence)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		accepted = n == 1
		note = "fence"
	default:
		// fence が無い方式は、担当を失っていても書けてしまう
		res, err := w.db.ExecContext(ctx,
			`UPDATE fence_state SET v = ?, fence = ?, writer = ?
			  WHERE tenant_id = ? AND k = 'counter'`,
			value, w.fence, w.owner, w.tenant)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		accepted = n >= 0 // 実行できた＝書けた（値が同じで 0 行のこともある）
		note = string(w.mode)
	}

	if _, err := w.db.ExecContext(ctx,
		`INSERT INTO fence_audit (tenant_id, writer, fence, accepted, note) VALUES (?,?,?,?,?)`,
		w.tenant, w.owner, w.fence, accepted, note); err != nil {
		return accepted, err
	}
	return accepted, nil
}

// Release は担当を明け渡す。
func (w *Worker) Release(ctx context.Context) error {
	if w.mode == ModeGetLock {
		if w.lockConn != nil {
			_, _ = w.lockConn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", "fencelab."+w.tenant)
			err := w.lockConn.Close()
			w.lockConn = nil
			return err
		}
		return nil
	}
	if w.mode == ModeNoLease {
		return nil
	}
	//smlint:allow rowsaffected 理由: EXP-2 の実験対象。担当の書き込みを1件ずつ測っている
	_, err := w.db.ExecContext(ctx,
		`UPDATE fence_lease SET expires_at = NOW(3) WHERE tenant_id = ? AND owner = ?`, w.tenant, w.owner)
	return err
}

func (w *Worker) readLease(ctx context.Context) (owner string, fence uint64, expires time.Time, err error) {
	err = w.db.QueryRowContext(ctx,
		`SELECT owner, fence, expires_at FROM fence_lease WHERE tenant_id = ?`, w.tenant).
		Scan(&owner, &fence, &expires)
	return
}

// CurrentOwner は現在の担当（実験の検証用）。
func CurrentOwner(ctx context.Context, db *sql.DB, tenant string) (string, uint64, error) {
	var owner string
	var fence uint64
	err := db.QueryRowContext(ctx,
		`SELECT owner, fence FROM fence_lease WHERE tenant_id = ?`, tenant).Scan(&owner, &fence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return owner, fence, err
}

// StateWriter は counter を最後に書いた者と、その fence。
func StateWriter(ctx context.Context, db *sql.DB, tenant string) (string, uint64, int64, error) {
	var writer string
	var fence uint64
	var v int64
	err := db.QueryRowContext(ctx,
		`SELECT writer, fence, v FROM fence_state WHERE tenant_id = ? AND k = 'counter'`, tenant).
		Scan(&writer, &fence, &v)
	return writer, fence, v, err
}

// AcceptedWritesBy は、その書き手の受理された書き込み回数。
func AcceptedWritesBy(ctx context.Context, db *sql.DB, tenant, writer string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fence_audit WHERE tenant_id = ? AND writer = ? AND accepted = 1`,
		tenant, writer).Scan(&n)
	return n, err
}

// Open は DB を開く。
//
// ★clientFoundRows=true を必ず付ける。
//
// 実験中に踏んだ罠: lease の延長は
//
//	UPDATE fence_lease SET expires_at = ? WHERE tenant_id = ? AND owner = ?
//
// の影響行数で「まだ自分が担当か」を判定していた。ところが MySQL は
// **値が変わらない UPDATE を 0 行と報告する**（docs/measurements.md の §9 参照）。
// 取得と延長が同じミリ秒に起きると expires_at が同じ値になり、
// 担当を持っているのに「担当を失った」と誤判定してプロセスが止まった。
//
// clientFoundRows=true にすると、影響行数が「変更された行」ではなく
// 「条件に一致した行」になるので、所有権の判定に使える。
func Open(dsn string) (*sql.DB, error) {
	full, err := withClientFoundRows(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", full)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	return db, nil
}

func withClientFoundRows(dsn string) (string, error) {
	at := strings.LastIndex(dsn, "@")
	base, query := dsn, ""
	if i := strings.Index(dsn[max(at, 0):], "?"); i >= 0 {
		i += max(at, 0)
		base, query = dsn[:i], dsn[i+1:]
	}
	v, err := url.ParseQuery(query)
	if err != nil {
		return "", err
	}
	v.Set("clientFoundRows", "true")
	v.Set("parseTime", "true")
	if v.Get("loc") == "" {
		v.Set("loc", "UTC")
	}
	return base + "?" + v.Encode(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
