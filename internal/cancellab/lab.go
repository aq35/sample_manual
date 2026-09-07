// Package cancellab は EXP-36（context キャンセルでクエリが本当に止まるか）の実験本体。
//
// クライアントがタイムアウトしても、DB 側のクエリが走り続けて接続を握りっぱなしだと、
// プールが枯れる。問い: context のタイムアウトで (1) 呼び出しは即戻るか、(2) DB 側の
// クエリも止まるか（幽霊スレッドが残らないか）。サーバ側の保険 MAX_EXECUTION_TIME も見る。
package cancellab

import (
	"context"
	"database/sql"
	"time"
)

func Open(dsn string, pool int) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	return db, nil
}

// SleepWithTimeout は SELECT SLEEP(sleepSec) を timeout つき context で投げ、所要とエラーを返す。
func SleepWithTimeout(db *sql.DB, sleepSec float64, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t0 := time.Now()
	var x int
	err := db.QueryRowContext(ctx, "SELECT SLEEP(?)+1", sleepSec).Scan(&x)
	return time.Since(t0), err
}

// SleepNoTimeout は同じクエリを timeout 無しで投げる（対照。フルに待つ）。
func SleepNoTimeout(ctx context.Context, db *sql.DB, sleepSec float64) (time.Duration, error) {
	t0 := time.Now()
	var x int
	err := db.QueryRowContext(ctx, "SELECT SLEEP(?)+1", sleepSec).Scan(&x)
	return time.Since(t0), err
}

// SleepMaxExecTime はサーバ側の MAX_EXECUTION_TIME(ms) ヒントで上限をかける（読み取り専用の保険）。
func SleepMaxExecTime(ctx context.Context, db *sql.DB, sleepSec float64, ms int) (time.Duration, error) {
	t0 := time.Now()
	var x int
	// ヒントは SELECT 直後に置く。SLEEP は SELECT なので効く。
	q := "SELECT /*+ MAX_EXECUTION_TIME(" + itoa(ms) + ") */ SLEEP(?)+1"
	err := db.QueryRowContext(ctx, q, sleepSec).Scan(&x)
	return time.Since(t0), err
}

// LingeringSleeps は worker のスレッドで今なお SLEEP を実行中の本数を返す（幽霊クエリの検査）。
// 自分自身（この COUNT クエリ。文中に SLEEP( を含むので LIKE に自己マッチする）は除く。
func LingeringSleeps(ctx context.Context, admin *sql.DB) (int, error) {
	var n int
	err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.processlist
		  WHERE user='worker' AND command='Query' AND id <> CONNECTION_ID() AND info LIKE '%SLEEP(%'`).
		Scan(&n)
	return n, err
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
