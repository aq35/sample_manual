// Package poollab は EXP-5（接続プールの飽和点）の実験本体。
//
// 知りたいのは「MaxOpenConns をいくつにすべきか」ではなく、
// **どこで頭打ちになり、そのとき何が起きるか**。
// 同時実行数を上げていくと、ある点から先はスループットが伸びず、遅延だけが伸びる。
// その点を測って初めて、上限の決め方に根拠が持てる。
package poollab

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
)

// Mode は負荷の種類。
type Mode string

const (
	// PointRead: 主キー1件の読み取り（もっとも軽い往復）。
	PointRead Mode = "point_read"
	// WriteTx: 1行更新のトランザクション（コミットの fsync を含む）。
	WriteTx Mode = "write_tx"
	// SlowTx: DB 側で待たせるトランザクション（遅い処理が接続を占有する状況）。
	SlowTx Mode = "slow_tx"
)

// Config は1回の測定条件。
type Config struct {
	Mode        Mode
	MaxOpen     int
	MaxIdle     int
	ConnMaxIdle time.Duration
	Concurrency int
	Duration    time.Duration
	SlowFor     time.Duration // SlowTx のときの DB 側の待ち時間
}

func (c *Config) setDefaults() {
	if c.Mode == "" {
		c.Mode = PointRead
	}
	if c.MaxOpen <= 0 {
		c.MaxOpen = 8
	}
	if c.MaxIdle == 0 {
		c.MaxIdle = c.MaxOpen
	}
	if c.ConnMaxIdle <= 0 {
		c.ConnMaxIdle = 30 * time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.Duration <= 0 {
		c.Duration = 1500 * time.Millisecond
	}
	if c.SlowFor <= 0 {
		c.SlowFor = 100 * time.Millisecond
	}
}

// Result は1回の測定結果。
type Result struct {
	Config Config

	Ops       int64
	Errors    int64
	Elapsed   time.Duration
	OpsPerSec float64
	Latency   expkit.LatencyStats

	// database/sql 側から見た数字
	OpenConns         int
	InUse             int
	Idle              int
	WaitCount         int64
	WaitDuration      time.Duration
	MaxIdleClosed     int64
	MaxLifetimeClosed int64
	NewConnections    int64 // サーバ側 Connections カウンタの増分

	// MySQL 側から見た数字
	MaxThreadsConnected int
	MaxThreadsRunning   int
}

// Setup は測定用の表を用意する。
func Setup(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS pool_item (
			id INT NOT NULL, v BIGINT NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	for i := 0; i < 64; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO pool_item (id, v) VALUES (?, 0) ON DUPLICATE KEY UPDATE v = v`, i); err != nil {
			return err
		}
	}
	return nil
}

// Run は1つの条件で負荷をかける。プールはこの関数の中だけで作って捨てる
// （測定ごとに接続の状態を揃えるため）。
func Run(ctx context.Context, dsn string, cfg Config) (Result, error) {
	cfg.setDefaults()
	res := Result{Config: cfg}

	db, err := sql.Open("mysql", normalizeDSN(dsn))
	if err != nil {
		return res, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(cfg.MaxOpen)
	db.SetMaxIdleConns(cfg.MaxIdle)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdle)

	if err := db.PingContext(ctx); err != nil {
		return res, err
	}

	// サーバ側の観測用に、測定対象とは別の接続を1本持つ
	probe, err := sql.Open("mysql", normalizeDSN(dsn))
	if err != nil {
		return res, err
	}
	defer func() { _ = probe.Close() }()
	probe.SetMaxOpenConns(1)

	startConns, _ := serverStatusInt(ctx, probe, "Connections")

	var (
		ops, errs    atomic.Int64
		maxConnected atomic.Int64
		maxRunning   atomic.Int64
		latency      = expkit.NewLatency()
		wg           sync.WaitGroup
	)

	runCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	// サーバ側の同時接続・実行中スレッドを追い続ける
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if v, err := serverStatusInt(ctx, probe, "Threads_connected"); err == nil {
					updateMax(&maxConnected, int64(v))
				}
				if v, err := serverStatusInt(ctx, probe, "Threads_running"); err == nil {
					updateMax(&maxRunning, int64(v))
				}
			}
		}
	}()

	start := time.Now()
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for runCtx.Err() == nil {
				begin := time.Now()
				if err := doOp(runCtx, db, cfg, worker); err != nil {
					if runCtx.Err() != nil {
						return
					}
					errs.Add(1)
					continue
				}
				latency.Record(time.Since(begin))
				ops.Add(1)
			}
		}(i)
	}
	wg.Wait()
	res.Elapsed = time.Since(start)
	<-watchDone

	endConns, _ := serverStatusInt(ctx, probe, "Connections")

	st := db.Stats()
	res.Ops = ops.Load()
	res.Errors = errs.Load()
	res.OpsPerSec = float64(res.Ops) / res.Elapsed.Seconds()
	res.Latency = latency.Stats()
	res.OpenConns = st.OpenConnections
	res.InUse = st.InUse
	res.Idle = st.Idle
	res.WaitCount = st.WaitCount
	res.WaitDuration = st.WaitDuration
	res.MaxIdleClosed = st.MaxIdleClosed
	res.MaxLifetimeClosed = st.MaxLifetimeClosed
	res.NewConnections = int64(endConns - startConns)
	res.MaxThreadsConnected = int(maxConnected.Load())
	res.MaxThreadsRunning = int(maxRunning.Load())
	return res, nil
}

func doOp(ctx context.Context, db *sql.DB, cfg Config, worker int) error {
	id := worker % 64
	switch cfg.Mode {
	case PointRead:
		var v int64
		return db.QueryRowContext(ctx, "SELECT v FROM pool_item WHERE id = ?", id).Scan(&v)

	case WriteTx:
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "UPDATE pool_item SET v = v + 1 WHERE id = ?", id); err != nil {
			return err
		}
		return tx.Commit()

	case SlowTx:
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SELECT SLEEP(?)", cfg.SlowFor.Seconds()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE pool_item SET v = v + 1 WHERE id = ?", id); err != nil {
			return err
		}
		return tx.Commit()
	}
	return fmt.Errorf("知らないモード: %s", cfg.Mode)
}

func serverStatusInt(ctx context.Context, db *sql.DB, name string) (int, error) {
	var k string
	var v int
	err := db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE ?", name).Scan(&k, &v)
	return v, err
}

func updateMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// normalizeDSN は測定に必要な設定を足す。
func normalizeDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	base, query := dsn, ""
	if i := strings.Index(dsn[maxInt(at, 0):], "?"); i >= 0 {
		i += maxInt(at, 0)
		base, query = dsn[:i], dsn[i+1:]
	}
	v, err := url.ParseQuery(query)
	if err != nil {
		return dsn
	}
	v.Set("parseTime", "true")
	if v.Get("loc") == "" {
		v.Set("loc", "UTC")
	}
	return base + "?" + v.Encode()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
