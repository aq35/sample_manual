// コマンド shutdownlab は EXP-3 の子プロセス。
//
// 受信（pull）→ 復号 → 有界キュー → バッチ → トランザクション → ack という
// ふつうの worker の形を持ち、指定の地点で正確に信号を受けられるようにしてある。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/shutdownlab"
)

type worker struct {
	db      *sql.DB
	http    *http.Client
	queue   string
	tenant  string
	name    string
	run     int
	batchN  int
	pauseAt string

	graceful        bool
	ackBeforeCommit bool
	drainDeadline   time.Duration

	ch chan int // 有界キュー

	mu        sync.Mutex
	pulled    []int // 取り出したが、まだ commit していないもの
	committed int
	acked     int
	nacked    int

	shutdown  chan struct{}
	closeOnce sync.Once
	paused    bool
}

func main() {
	var (
		dsn      = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN")
		queueURL = flag.String("queue", "", "キューの URL")
		tenant   = flag.String("tenant", "t-exp3", "テナント")
		name     = flag.String("name", "w1", "worker 名")
		run      = flag.Int("run", 1, "何回目の起動か")
		items    = flag.Int("items", 40, "処理する上限")
		batchN   = flag.Int("batch", 5, "1トランザクションにまとめる件数")
		capacity = flag.Int("capacity", 16, "有界キューの大きさ")
		graceful = flag.Bool("graceful", true, "片付けてから終わる")
		ackFirst = flag.Bool("ack-before-commit", false, "commit の前に ack する（事故の再現）")
		drain    = flag.Duration("drain-deadline", 3*time.Second, "片付けの期限")
		slow     = flag.Duration("work-delay", 5*time.Millisecond, "1件あたりの処理時間")
	)
	flag.Parse()

	if *dsn == "" || *queueURL == "" {
		fmt.Fprintln(os.Stderr, "dsn と queue は必須")
		os.Exit(2)
	}
	db, err := shutdownlab.Open(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "DB:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	w := &worker{
		db: db, http: &http.Client{Timeout: 2 * time.Second},
		queue: *queueURL, tenant: *tenant, name: *name, run: *run,
		batchN: *batchN, pauseAt: os.Getenv(expkit.PausePointEnv),
		graceful: *graceful, ackBeforeCommit: *ackFirst, drainDeadline: *drain,
		ch:       make(chan int, *capacity),
		shutdown: make(chan struct{}),
	}

	baseGoroutines := runtime.NumGoroutine()

	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		s := <-sigc
		fmt.Printf("%ssignal %v\n", expkit.MarkerPrefix, s)
		_ = os.Stdout.Sync()
		if !w.graceful {
			// ★片付けずに落ちる。取り出したものは宙に浮く
			fmt.Printf("%sexit_without_cleanup pulled=%d\n", expkit.MarkerPrefix, len(w.pulled))
			_ = os.Stdout.Sync()
			os.Exit(1)
		}
		w.beginShutdown()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// lease の更新（止め忘れがちな ticker の代表）
	leaseStop := make(chan struct{})
	var leaseWG sync.WaitGroup
	leaseWG.Add(1)
	go func() {
		defer leaseWG.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.point("lease_renew")
				//smlint:allow loopquery 理由: EXP-3 の worker 本体。1件ずつ処理する形を測っている
				_, _ = db.ExecContext(ctx,
					`INSERT INTO shutdown_lease (tenant_id, owner, renewals) VALUES (?,?,1)
					 ON DUPLICATE KEY UPDATE owner = VALUES(owner), renewals = renewals + 1,
					   renewed_at = CURRENT_TIMESTAMP(3)`, w.tenant, w.name)
			case <-leaseStop:
				return
			}
		}
	}()

	// 受信（受付）
	var pullWG sync.WaitGroup
	pullWG.Add(1)
	go func() {
		defer pullWG.Done()
		defer close(w.ch) // ★受付を閉じるのは受信側の責任
		remaining := *items
		for remaining > 0 {
			select {
			case <-w.shutdown:
				fmt.Printf("%sstopped_accepting remaining=%d\n", expkit.MarkerPrefix, remaining)
				_ = os.Stdout.Sync()
				return
			default:
			}
			w.point("idle")
			w.point("fetching")
			batch, err := w.pull(ctx, 4)
			if err != nil {
				w.point("retry_sleep")
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if len(batch) == 0 {
				return
			}
			w.point("decoding")
			for _, id := range batch {
				w.point("enqueue")
				select {
				case w.ch <- id:
					remaining--
				case <-w.shutdown:
					// 受付停止後に手元に残ったものは、キューへ戻す
					w.nack(ctx, []int{id})
					return
				}
			}
		}
	}()

	// 処理（バッチ → トランザクション → ack）
	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		var batch []int
		flush := func() {
			if len(batch) == 0 {
				return
			}
			w.point("batching")
			if err := w.commit(ctx, batch); err != nil {
				fmt.Fprintln(os.Stderr, "commit:", err)
				w.nack(ctx, batch)
			}
			batch = nil
		}
		for id := range w.ch {
			time.Sleep(*slow)
			batch = append(batch, id)
			if len(batch) >= w.batchN {
				flush()
			}
		}
		flush() // 受付が閉じたら、残りを出し切る
	}()

	// 片付けの期限つき待ち
	select {
	case <-processDone:
	case <-time.After(*drain + 30*time.Second):
	}

	// 期限内に終わらなければ、何が残っているかを記録して落ちる
	leftover := w.remaining()
	if len(leftover) > 0 {
		fmt.Printf("%sshutdown_deadline_exceeded pending=%d\n", expkit.MarkerPrefix, len(leftover))
		w.nack(ctx, leftover)
	}

	close(leaseStop)
	leaseWG.Wait()
	pullWG.Wait()
	cancel()

	// ★後始末: 接続を明示的に閉じる。
	// HTTP クライアントは keep-alive の接続ごとに読み書きの goroutine を持つので、
	// 閉じないと「終わったはずなのに goroutine が残る」状態になる
	// （最初の測定では、これが原因で全実行が起動時+2 のまま終わっていた）。
	w.http.CloseIdleConnections()
	_ = db.Close()

	// goroutine が残っていないか（自分で数えて出す）
	time.Sleep(50 * time.Millisecond)
	fmt.Printf("%ssummary committed=%d acked=%d nacked=%d goroutines=%d base=%d\n",
		expkit.MarkerPrefix, w.committed, w.acked, w.nacked, runtime.NumGoroutine(), baseGoroutines)
	fmt.Printf("%sDONE\n", expkit.MarkerPrefix)
}

// point は名前つき地点。EXP_PAUSE_AT と一致したら、信号が来るまで止まる。
// 「その地点で信号を受けた」状況を正確に作るため。
func (w *worker) point(name string) {
	fmt.Printf("%s%s\n", expkit.MarkerPrefix, name)
	_ = os.Stdout.Sync()
	if w.pauseAt == "" || w.pauseAt != name || w.paused {
		return
	}
	w.paused = true
	fmt.Printf("%sPAUSED at %s\n", expkit.MarkerPrefix, name)
	_ = os.Stdout.Sync()
	<-w.shutdown // 信号が来たら進む
}

func (w *worker) beginShutdown() {
	w.closeOnce.Do(func() {
		fmt.Printf("%sshutdown_begin\n", expkit.MarkerPrefix)
		_ = os.Stdout.Sync()
		close(w.shutdown)
	})
}

func (w *worker) pull(ctx context.Context, n int) ([]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/pull?n=%d", w.queue, n), nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Items []struct {
			ID int `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(out.Items))
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	w.mu.Lock()
	w.pulled = append(w.pulled, ids...)
	w.mu.Unlock()
	return ids, nil
}

// commit はバッチを1トランザクションで書き、そのあと ack する。
func (w *worker) commit(ctx context.Context, ids []int) error {
	if w.ackBeforeCommit {
		// ★事故の再現: commit の前に ack する。ここで落ちると **消失** する
		w.ack(ctx, ids)
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	w.point("in_tx")
	for _, id := range ids {
		//smlint:allow loopquery 理由: EXP-3 の worker 本体。1件ずつ処理する形を測っている
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO shutdown_item (tenant_id, item_id, worker, run) VALUES (?,?,?,?)`,
			w.tenant, id, w.name, w.run); err != nil {
			return err
		}
	}
	w.point("before_commit")
	if err := tx.Commit(); err != nil {
		return err
	}
	w.point("after_commit")

	w.mu.Lock()
	w.committed += len(ids)
	w.pulled = removeAll(w.pulled, ids)
	w.mu.Unlock()

	if !w.ackBeforeCommit {
		w.ack(ctx, ids)
	}
	return nil
}

func (w *worker) ack(ctx context.Context, ids []int) {
	if len(ids) == 0 {
		return
	}
	w.post(ctx, "/ack", ids)
	w.mu.Lock()
	w.acked += len(ids)
	w.mu.Unlock()
}

func (w *worker) nack(ctx context.Context, ids []int) {
	if len(ids) == 0 {
		return
	}
	w.post(ctx, "/nack", ids)
	w.mu.Lock()
	w.nacked += len(ids)
	w.pulled = removeAll(w.pulled, ids)
	w.mu.Unlock()
	fmt.Printf("%snacked n=%d\n", expkit.MarkerPrefix, len(ids))
	_ = os.Stdout.Sync()
}

func (w *worker) post(ctx context.Context, path string, ids []int) {
	body, _ := json.Marshal(map[string]any{"ids": ids})
	req, err := http.NewRequest(http.MethodPost, w.queue+path, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (w *worker) remaining() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int, len(w.pulled))
	copy(out, w.pulled)
	return out
}

func removeAll(src, remove []int) []int {
	drop := map[int]struct{}{}
	for _, id := range remove {
		drop[id] = struct{}{}
	}
	out := src[:0]
	for _, id := range src {
		if _, ok := drop[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}
