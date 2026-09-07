// Package procseplab は EXP-29（Web と Worker を分けるべきか）の実験本体。
//
// 問い: Web（外から来る・バースト・重め）と Worker（定期ポーリング・軽い・遅延に敏感）が
// 同じ接続プールを共有すると、Web のバーストで Worker が接続を待たされて dispatch 遅延が
// 跳ねるのか。プールを分ける（役割ごとに取り分を持つ）と守れるのか。
//
// ここは repo 層でなく「接続プールの振る舞い」そのものを見るので、生の *sql.DB を直接使う。
package procseplab

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

// Open は同じ DSN から、指定の MaxOpenConns で新しいプールを開く。
func Open(dsn string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	return db, nil
}

// WebBurst は web を模す: goroutines 本で、window の間ずっと重い（SLEEP）クエリを回して
// プールの接続を占有し続ける。返す stop を呼ぶと止まる。
func WebBurst(ctx context.Context, db *sql.DB, goroutines int, sleep time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	secs := sleep.Seconds()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				// SLEEP はサーバ側で接続を保持する＝プールの1本を占有する。
				var x int
				//smlint:allow loopquery 理由: web の負荷を模す占有ループ（実験対象）
				_ = db.QueryRowContext(ctx, "SELECT SLEEP(?)+1", secs).Scan(&x)
			}
		}()
	}
	return func() { cancel(); wg.Wait() }
}

// WorkerLatency は worker を模す: 軽い問い合わせを iters 回、直列に投げてレイテンシを測る。
// 共有プールなら web に接続を取られて acquire 待ちが乗る。専用プールなら乗らない。
func WorkerLatency(ctx context.Context, db *sql.DB, iters int) expkit.LatencyStats {
	lat := expkit.NewLatency()
	for i := 0; i < iters; i++ {
		t0 := time.Now()
		var x int
		//smlint:allow loopquery 理由: worker のポーリングを模す測定ループ（実験対象）
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&x); err != nil {
			return expkit.LatencyStats{}
		}
		lat.Record(time.Since(t0))
		time.Sleep(2 * time.Millisecond) // ポーリングの間隔を模す（連続占有にしない）
	}
	return lat.Stats()
}
