// Package retrylab は EXP-49（一時 vs 恒久エラーの分類とリトライ）の実験本体。
//
// 何でもリトライすると、恒久エラー（制約違反・構文）を無駄に何度も叩き、一時エラー（デッドロック・
// ロック待ちタイムアウト・接続断）だけを retry すべき。分類器で「retry してよいか」を判定し、
// 恒久は即失敗（fail-fast）、一時だけバックオフして再試行する。
package retrylab

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Retryable は「このエラーは retry してよいか」。一時エラー（1213 デッドロック / 1205 ロック待ち
// タイムアウト / 接続断）だけ true。恒久エラー（1062 重複キー・構文など）は false。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1213, 1205: // Deadlock / Lock wait timeout
			return true
		default:
			return false // 1062(重複) など DB エラーは恒久＝retry しない
		}
	}
	if errors.Is(err, driver.ErrBadConn) { // 接続断は張り直して retry（EXP-33）
		return true
	}
	return false
}

// Do は fn を最大 maxAttempts 回、Retryable な間だけバックオフして再試行する。
// 返り値は実際の試行回数と最後のエラー。恒久エラーは1回で返る（fail-fast）。
func Do(maxAttempts int, backoff time.Duration, fn func() error) (attempts int, err error) {
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		err = fn()
		if err == nil {
			return attempts, nil
		}
		if !Retryable(err) {
			return attempts, err // 恒久 → 即失敗
		}
		if attempts < maxAttempts {
			time.Sleep(backoff)
		}
	}
	return attempts - 1, err
}

// Setup は実験用の1行を用意する。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS retry_row",
		"CREATE TABLE retry_row (id BIGINT PRIMARY KEY, v INT NOT NULL) ENGINE=InnoDB",
		"INSERT INTO retry_row (id, v) VALUES (1, 0)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// InsertDup は id=1 を再 INSERT して重複キー(1062)を起こす（恒久エラー）。
func InsertDup(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "INSERT INTO retry_row (id, v) VALUES (1, 0)")
	return err
}

// HoldLock は row(1) に X ロックを掛けたトランザクションを開き、release を返す
// （呼ぶとコミットしてロックを解放）。
func HoldLock(ctx context.Context, db *sql.DB) (release func(), err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	//smlint:allow rowsaffected 理由: ロックを掛けるためだけの UPDATE。影響行数は使わない（1行に当たる）
	if _, err := tx.ExecContext(ctx, "UPDATE retry_row SET v = v + 1 WHERE id = 1"); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return func() { _ = tx.Commit() }, nil
}

// LockVictimConn は innodb_lock_wait_timeout を短くした専用接続を返す（ロック待ちを 1205 で起こす用）。
func LockVictimConn(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
