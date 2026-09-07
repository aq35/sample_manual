// Package isolationlab は EXP-55（トランザクション分離レベル RR vs RC）の実験本体。
//
// MySQL/InnoDB の既定は REPEATABLE READ(RR)。RR はトランザクション開始時点のスナップショットを
// 読み続ける（再読で同じ値＝non-repeatable read が起きない）。さらに範囲のロック読みで gap ロックを
// 取り、範囲内への INSERT（phantom）を防ぐ。READ COMMITTED(RC) は文ごとに最新の確定値を読み
// （再読で変わりうる）、gap ロックを基本取らないのでロック競合は減るが phantom は起きうる。
package isolationlab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// Setup は iso_row(k PK, v) を作り、10/20/30 の3行を入れる（15 は「隙間」）。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS iso_row",
		"CREATE TABLE iso_row (k INT PRIMARY KEY, v INT NOT NULL) ENGINE=InnoDB",
		"INSERT INTO iso_row (k, v) VALUES (10,1),(20,1),(30,1)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("iso setup: %w", err)
		}
	}
	return nil
}

// NonRepeatableRead は level のトランザクションで k=10 を2回読む。間に別接続が v を +100 して commit する。
// RR なら first==second（スナップショット）、RC なら second==first+100（最新を読む）。
func NonRepeatableRead(ctx context.Context, db *sql.DB, level string) (first, second int, err error) {
	//smlint:allow rowsaffected 理由: 実験の前提リセット。1行に当たる（PK 指定）
	if _, err = db.ExecContext(ctx, "UPDATE iso_row SET v=1 WHERE k=10"); err != nil {
		return 0, 0, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL "+level); err != nil {
		return 0, 0, err
	}
	if _, err = conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return 0, 0, err
	}
	if err = conn.QueryRowContext(ctx, "SELECT v FROM iso_row WHERE k=10").Scan(&first); err != nil {
		return 0, 0, err
	}
	// 別接続が更新して commit（この tx の外）
	//smlint:allow rowsaffected 理由: 実験。1行に当たる
	if _, err = db.ExecContext(ctx, "UPDATE iso_row SET v=v+100 WHERE k=10"); err != nil {
		return 0, 0, err
	}
	if err = conn.QueryRowContext(ctx, "SELECT v FROM iso_row WHERE k=10").Scan(&second); err != nil {
		return 0, 0, err
	}
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	return first, second, nil
}

// GapInsertBlocked は level のトランザクションが範囲 (10,30) をロック読みしている間に、
// 別接続が隙間 k=15 を INSERT できるかを見る。RR は gap ロックでブロック（1205）、RC は通る。
// 返り値 blocked=true なら INSERT がロック待ちタイムアウトで弾かれた（gap ロックあり）。
func GapInsertBlocked(ctx context.Context, db *sql.DB, level string) (blocked bool, err error) {
	//smlint:allow rowsaffected 理由: 実験の後片付け。隙間行が無ければ 0 でも正しい
	if _, err = db.ExecContext(ctx, "DELETE FROM iso_row WHERE k=15"); err != nil {
		return false, err
	}

	locker, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = locker.Close() }()
	if _, err = locker.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL "+level); err != nil {
		return false, err
	}
	if _, err = locker.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return false, err
	}
	// 範囲のロック読み（RR はここで gap/next-key ロックを取る）
	//smlint:allow rowsaffected 理由: ロックを取るためのロック読み。行数は使わない
	if _, err = locker.ExecContext(ctx, "SELECT k FROM iso_row WHERE k>10 AND k<30 FOR UPDATE"); err != nil {
		_, _ = locker.ExecContext(ctx, "ROLLBACK")
		return false, err
	}

	// victim: 短い lock_wait_timeout で隙間 15 を INSERT
	victim, err := db.Conn(ctx)
	if err != nil {
		_, _ = locker.ExecContext(ctx, "ROLLBACK")
		return false, err
	}
	defer func() { _ = victim.Close() }()
	if _, err = victim.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout=1"); err != nil {
		_, _ = locker.ExecContext(ctx, "ROLLBACK")
		return false, err
	}
	//smlint:allow rowsaffected 理由: 実験の INSERT。成否（ブロックされるか）が見たいもの
	_, insErr := victim.ExecContext(ctx, "INSERT INTO iso_row (k, v) VALUES (15, 1)")

	_, _ = locker.ExecContext(ctx, "ROLLBACK") // gap ロック解放
	//smlint:allow rowsaffected 理由: 後片付け
	_, _ = db.ExecContext(ctx, "DELETE FROM iso_row WHERE k=15")

	if insErr == nil {
		return false, nil // 通った＝gap ロック無し（RC）
	}
	var me *mysql.MySQLError
	if errors.As(insErr, &me) && me.Number == 1205 {
		return true, nil // ロック待ちタイムアウト＝gap ロックでブロック（RR）
	}
	return false, insErr // 想定外エラー
}
