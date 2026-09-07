// Package tzlab は EXP-35（タイムゾーン/DST のスケジューリング）の実験本体。
//
// always-on スケジューラの定番バグ:
//   1. 「毎日 02:30（現地）」のような現地時刻の予定は、夏時間の切替で消える時刻/二重の時刻に当たる。
//   2. MySQL の DATETIME（tz 無し・そのまま保存）と TIMESTAMP（UTC 保存・セッション tz で変換）は
//      挙動が違う。セッション tz が変わると TIMESTAMP は値がずれ、DATETIME はずれない。
package tzlab

import (
	"context"
	"database/sql"
)

func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS tztab",
		`CREATE TABLE tztab (id INT PRIMARY KEY, dt DATETIME NOT NULL, ts TIMESTAMP NOT NULL) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// StoreAt は指定オフセット（例 "+00:00"）のセッションで、同じ文字列時刻を dt と ts に入れる。
func StoreAt(ctx context.Context, db *sql.DB, tz string, id int, literal string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET time_zone = ?", tz); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO tztab (id, dt, ts) VALUES (?, ?, ?)", id, literal, literal)
	return err
}

// ReadAt は指定オフセットのセッションで dt と ts を文字列で読む（表示上の値を見る）。
func ReadAt(ctx context.Context, db *sql.DB, tz string, id int) (dt, ts string, err error) {
	conn, cerr := db.Conn(ctx)
	if cerr != nil {
		return "", "", cerr
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET time_zone = ?", tz); err != nil {
		return "", "", err
	}
	err = conn.QueryRowContext(ctx,
		`SELECT DATE_FORMAT(dt,'%Y-%m-%d %H:%i:%s'), DATE_FORMAT(ts,'%Y-%m-%d %H:%i:%s') FROM tztab WHERE id=?`, id).
		Scan(&dt, &ts)
	return dt, ts, err
}
