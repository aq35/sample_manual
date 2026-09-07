// Package resiliencelab は EXP-33（DB フェイルオーバ/再起動への耐性）の実験本体。
//
// DB が再起動・フェイルオーバすると、プールが握っていた接続は切れる。問い:
//   1. アイドルの接続が切られても、次のクエリで database/sql が自動で張り直して成功するか
//      （自前の再接続コードは要らないのか）。
//   2. 実行中のクエリが切られたら、それは自動リトライされない（アプリが冪等な読みだけ retry すべき）。
//
// 接続を「切る」のは、別のワーカー接続から自分のアカウントのスレッドを KILL して模す。
package resiliencelab

import (
	"context"
	"database/sql"
	"time"
)

func Open(dsn string, pool int, lifetime time.Duration) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	db.SetConnMaxLifetime(lifetime)
	return db, nil
}

// Warm は n 本の接続を実際に開く（並行に短い SLEEP を投げて占有する）。
func Warm(ctx context.Context, db *sql.DB, n int) {
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			var x int
			//smlint:allow loopquery 理由: 接続を n 本開けるためのウォームアップ（実験対象）
			_ = db.QueryRowContext(ctx, "SELECT SLEEP(0.2)+1").Scan(&x)
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
}

// KillWorkerConns は admin 接続から、自分以外の worker スレッドを全部 KILL する
// （＝DB 側から接続が切られる状況を模す）。切った本数を返す。
func KillWorkerConns(ctx context.Context, admin *sql.DB) (int, error) {
	rows, err := admin.QueryContext(ctx,
		"SELECT id FROM information_schema.processlist WHERE user='worker' AND id <> CONNECTION_ID()")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	killed := 0
	for _, id := range ids {
		//smlint:allow loopquery 理由: 各スレッドを KILL する（接続断を模す・実験対象）
		//smlint:allow rowsaffected 理由: KILL に影響行数は無い
		if _, err := admin.ExecContext(ctx, "KILL ?", id); err == nil {
			killed++
		}
	}
	return killed, nil
}

// RunN は軽いクエリを n 回投げ、エラー回数を返す（切断後の自動回復を測る）。
func RunN(ctx context.Context, db *sql.DB, n int) int {
	errs := 0
	for i := 0; i < n; i++ {
		var x int
		//smlint:allow loopquery 理由: 回復後の成功率を測る負荷ループ（実験対象）
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&x); err != nil {
			errs++
		}
	}
	return errs
}

// Inflight は長いクエリを1本投げ、その結果（切られたら error）を返す。
func Inflight(ctx context.Context, db *sql.DB) error {
	var x int
	return db.QueryRowContext(ctx, "SELECT SLEEP(2)+1").Scan(&x)
}
