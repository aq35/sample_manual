// Package capacitylab は EXP-31（1タスクでどれくらい捌けるか）の実験本体。
//
// 「1 vCPU / 2GB の ECS タスクで、SSE は何本・Worker はどれくらい捌けるか」を、
// 実測できる2つの単位コストから見積もる:
//   1. 1接続あたりのメモリ（goroutine スタック＋バッファ）→ メモリ律速の SSE 本数。
//   2. 単一プロセスの DB 往復スループット（並行度別）→ DB 律速の Worker 処理量。
//
// CPU 律速の rps（JSON 整形など計算主体）はホスト依存なので、ここでは測らずモデルで扱う
// （このサンドボックスの CPU は本番 1 vCPU と違うため、測っても誤解を生む）。
package capacitylab

import (
	"context"
	"database/sql"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ConnMem は「n 本の接続」を模して、1本あたりのメモリ（バイト）を実測する。
// 各接続 = 1 goroutine（park）＋ bufBytes のバッファ（TLS/HTTP の読み書きバッファ相当）。
// heap と goroutine スタックの増分を n で割って返す。
func ConnMem(n, bufBytes int) float64 {
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	bufs := make([][]byte, n)
	for i := 0; i < n; i++ {
		b := make([]byte, bufBytes)
		for j := 0; j < bufBytes; j += 4096 { // ページを実際に触って常駐させる
			b[j] = byte(j)
		}
		bufs[i] = b
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			<-stop
			runtime.KeepAlive(b)
		}(b)
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	used := (int64(m1.HeapAlloc) - int64(m0.HeapAlloc)) + (int64(m1.StackInuse) - int64(m0.StackInuse))

	close(stop)
	wg.Wait()
	runtime.KeepAlive(bufs)
	if n == 0 {
		return 0
	}
	return float64(used) / float64(n)
}

// OpThroughput は単一プロセスが並行度 concurrency で回したときの DB 往復数/秒を実測する。
// Worker の「claim → dispatch」は短い DB 往復の繰り返しなので、これが処理量の目安になる。
// query は軽い1往復（例: SELECT 1）。IO 待ちが主なので concurrency を上げると伸び、DB で頭打ちになる。
func OpThroughput(ctx context.Context, db *sql.DB, concurrency int, dur time.Duration, query string) float64 {
	var ops int64
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				var x int
				//smlint:allow loopquery 理由: DB 往復スループットを測る負荷ループ（実験対象）
				if err := db.QueryRowContext(ctx, query).Scan(&x); err != nil {
					return
				}
				atomic.AddInt64(&ops, 1)
			}
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}
