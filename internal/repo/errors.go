// Package repo は Go から MySQL を扱うリポジトリ層。
//
// 目的は3つ。
//
//  1. **事故を防ぐ** — テナント指定忘れ、WHERE の書き間違い、更新の消失を
//     「気をつける」ではなく仕組みで止める
//  2. **性能を崩しにくくする** — 深い OFFSET、N+1、全件取得を書きにくくし、
//     実行計画をテストで固定できるようにする
//  3. **運用保守しやすくする** — 問い合わせ回数・遅いクエリ・長いトランザクションが
//     数字で見え、エラーが型で分かる
//
// 設計判断の根拠（実験結果）は docs/repository-layer.md にある。
package repo

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

var (
	// ErrNotFound は対象が無い（sql.ErrNoRows を包む）。
	ErrNotFound = errors.New("見つからない")
	// ErrConflict は一意制約の違反（MySQL 1062）。
	ErrConflict = errors.New("すでに存在する")
	// ErrOptimisticLock は version 不一致。読み直してやり直す。
	ErrOptimisticLock = errors.New("他の処理が先に更新した")
	// ErrUnexpectedRowCount は影響行数が宣言と違う（＝ WHERE の書き間違いの疑い）。
	ErrUnexpectedRowCount = errors.New("影響行数が想定と違う")
	// ErrMissingTenant は SQL に :tenant が無い（＝テナント指定忘れ）。
	ErrMissingTenant = errors.New("SQL に :tenant が無い")
	// ErrUnsafeStatement は危険な文（WHERE 無しの UPDATE/DELETE など）。
	ErrUnsafeStatement = errors.New("危険な SQL")
	// ErrTooManyRows は上限を超える取得要求。
	ErrTooManyRows = errors.New("一度に取りすぎ")
)

// Error は、どの操作で何が起きたかを保つ。ログにそのまま出せる形にしておく。
type Error struct {
	Op     string // 例: "profile.Rename"
	Tenant string
	Err    error
	SQL    string
}

func (e *Error) Error() string {
	if e.SQL != "" {
		return fmt.Sprintf("%s(tenant=%s): %v\nSQL: %s", e.Op, e.Tenant, e.Err, e.SQL)
	}
	return fmt.Sprintf("%s(tenant=%s): %v", e.Op, e.Tenant, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func wrap(op, tenant, query string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Tenant: tenant, Err: classify(err), SQL: query}
}

// classify は driver のエラーを、扱いやすい型に寄せる。
// 呼び出し側が errors.Is(err, repo.ErrConflict) で分岐できるようにするため。
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1062: // Duplicate entry
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	return err
}

// IsDeadlock はデッドロック（1213）。リトライしてよい。
func IsDeadlock(err error) bool { return mysqlErrNo(err) == 1213 }

// IsLockWaitTimeout はロック待ちタイムアウト（1205）。
// リトライしてよいが、繰り返すなら設計を見直す合図。
func IsLockWaitTimeout(err error) bool { return mysqlErrNo(err) == 1205 }

// IsSafeUpdateBlocked は sql_safe_updates による拒否（1175）。
// 「キー列を使わない UPDATE/DELETE」を MySQL 側が止めた状態。
func IsSafeUpdateBlocked(err error) bool { return mysqlErrNo(err) == 1175 }

func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// Retryable は、そのまま同じ処理をやり直してよいエラーか。
func Retryable(err error) bool {
	return IsDeadlock(err) || IsLockWaitTimeout(err) || errors.Is(err, ErrOptimisticLock)
}
