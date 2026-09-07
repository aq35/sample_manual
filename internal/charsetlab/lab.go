// Package charsetlab は EXP-57（utf8mb4 と index 長・照合）の実験本体。
//
// utf8mb4 は1文字最大4バイト。InnoDB(DYNAMIC) の index キーは最大 3072 バイトなので、
// VARCHAR(768)*4=3072 は索引化できるが VARCHAR(769) は超えて失敗する（1071）。長い列は
// prefix index で回避する。照合(collation)は等価判定・一意制約の意味を変える（_ci は大小/アクセント
// 無視、_bin は厳密一致）。実挙動で示す。
package charsetlab

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
)

// TryFullIndex は VARCHAR(chars) utf8mb4 の列に「全長の」索引を張れるか試す。
// 返り値 ok=false かつ keyTooLong=true なら 1071（キーが長すぎ）。
func TryFullIndex(ctx context.Context, db *sql.DB, chars int) (ok, keyTooLong bool, err error) {
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_idx")
	create := "CREATE TABLE cs_idx (s VARCHAR(" + itoa(chars) + ") NOT NULL) " +
		"CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci ENGINE=InnoDB ROW_FORMAT=DYNAMIC"
	if _, err = db.ExecContext(ctx, create); err != nil {
		return false, false, err
	}
	_, addErr := db.ExecContext(ctx, "ALTER TABLE cs_idx ADD INDEX k_s (s)")
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_idx")
	if addErr == nil {
		return true, false, nil
	}
	var me *mysql.MySQLError
	if errors.As(addErr, &me) && me.Number == 1071 { // Specified key was too long
		return false, true, nil
	}
	return false, false, addErr
}

// TryPrefixIndex は長い utf8mb4 列でも prefix 索引(col(prefix)) なら張れることを確認する。
func TryPrefixIndex(ctx context.Context, db *sql.DB, chars, prefix int) (ok bool, err error) {
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_pfx")
	create := "CREATE TABLE cs_pfx (s VARCHAR(" + itoa(chars) + ") NOT NULL) " +
		"CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci ENGINE=InnoDB ROW_FORMAT=DYNAMIC"
	if _, err = db.ExecContext(ctx, create); err != nil {
		return false, err
	}
	_, addErr := db.ExecContext(ctx, "ALTER TABLE cs_pfx ADD INDEX k_s (s("+itoa(prefix)+"))")
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_pfx")
	return addErr == nil, addErr
}

// Collation は指定 collation の列で「大小無視の一致」と「一意制約の重複拒否」を測る。
//
//	matchCI:     name='ABC' が 'abc' にヒットするか（_ci なら true、_bin なら false）
//	dupRejected: 'abc' の後に 'ABC' を入れると一意制約で弾かれるか（_ci なら true、_bin なら false）
func Collation(ctx context.Context, db *sql.DB, collation string) (matchCI, dupRejected bool, err error) {
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_coll")
	create := "CREATE TABLE cs_coll (name VARCHAR(64) NOT NULL, UNIQUE KEY uq_name (name)) " +
		"CHARACTER SET utf8mb4 COLLATE " + collation + " ENGINE=InnoDB"
	if _, err = db.ExecContext(ctx, create); err != nil {
		return false, false, err
	}
	defer func() { _, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS cs_coll") }()

	//smlint:allow rowsaffected 理由: 実験の INSERT。1行に当たる
	if _, err = db.ExecContext(ctx, "INSERT INTO cs_coll (name) VALUES ('abc')"); err != nil {
		return false, false, err
	}
	var cnt int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cs_coll WHERE name='ABC'").Scan(&cnt); err != nil {
		return false, false, err
	}
	matchCI = cnt > 0

	//smlint:allow rowsaffected 理由: 実験の INSERT。重複拒否されるかを見る
	_, dupErr := db.ExecContext(ctx, "INSERT INTO cs_coll (name) VALUES ('ABC')")
	if dupErr != nil {
		var me *mysql.MySQLError
		if errors.As(dupErr, &me) && me.Number == 1062 {
			dupRejected = true
		} else {
			return matchCI, false, dupErr
		}
	}
	return matchCI, dupRejected, nil
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
