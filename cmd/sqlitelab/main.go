// Command sqlitelab は EXP-10 の子プロセス。
//
// 「別プロセスが同じファイルを書いている」状況は、goroutine では作れない。
// 同じプロセス内の *sql.DB は接続を共有しているが、別プロセスは
// **OS のファイルロック越しに**競合する。そこが SQLite の本番構成の要点なので、
// 実験も本物の別プロセスで行う。
//
//	sqlitelab -driver=purego -db=/tmp/x.db -dur=2s -wal -busy=5s
//
// 結果は JSON で標準出力へ1行出す（親が読む）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/sqlitefacts"
)

type result struct {
	Driver   string  `json:"driver"`
	Ops      int64   `json:"ops"`
	Busy     int64   `json:"busy"`
	Errs     int64   `json:"errs"`
	PerSec   float64 `json:"per_sec"`
	P50Micro int64   `json:"p50_us"`
	P99Micro int64   `json:"p99_us"`
	Journal  string  `json:"journal_mode"`
}

func main() {
	var (
		driver = flag.String("driver", "purego", "purego / cgo")
		path   = flag.String("db", "", "SQLite のファイル")
		dur    = flag.Duration("dur", 2*time.Second, "書き込み続ける時間")
		tenant = flag.String("tenant", "t1", "テナント")
		rows   = flag.Int("rows", 200, "対象行数")
		wal    = flag.Bool("wal", true, "WAL を使うか")
		busy   = flag.Duration("busy", 5*time.Second, "busy_timeout")
		setup  = flag.Bool("setup", false, "スキーマを作って行を入れてから始める")
		maxOpn = flag.Int("maxopen", 1, "MaxOpenConns")
		sync   = flag.String("sync", "NORMAL", "synchronous（OFF / NORMAL / FULL）")
		commit = flag.Int("commit-then-die", 0, "この件数を1件ずつコミットしてから、自分を SIGKILL する")
	)
	flag.Parse()

	if *path == "" {
		fail("-db が要る")
	}
	d, err := sqlitefacts.ByName(*driver)
	if err != nil {
		fail("%v", err)
	}
	p := sqlitefacts.Pragmas{BusyTimeout: *busy, ForeignKeys: true, Synchronous: *sync}
	if *wal {
		p.JournalMode = "WAL"
	}
	db, err := d.Open(*path, p)
	if err != nil {
		fail("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(*maxOpn)
	db.SetMaxIdleConns(*maxOpn)

	ctx := context.Background()
	if *setup {
		if err := sqlitefacts.Setup(ctx, db, *tenant); err != nil {
			fail("setup: %v", err)
		}
		if err := sqlitefacts.Seed(ctx, db, *tenant, *rows); err != nil {
			fail("seed: %v", err)
		}
	}

	var journal string
	_ = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal)

	// 「コミットしてから落ちる」実験。
	// ★ここで大事なのは、外から時間を見計らって kill しないこと。
	// コミットが返った**直後**の地点で自分を殺すから、
	// 「返ったのに消えている」かどうかが確実に判定できる（docs/experiments.md）。
	if *commit > 0 {
		ks := expkit.NewKillSwitch()
		if err := sqlitefacts.CommitN(ctx, db, *tenant, *commit); err != nil {
			fail("commit: %v", err)
		}
		ks.Point("after_commit")
		fmt.Printf(`{"committed":%d,"journal_mode":%q}`+"\n", *commit, journal)
		return
	}

	tp := sqlitefacts.WriteLoad(ctx, db, *tenant, *rows, 1, *dur)
	out := result{
		Driver: d.Name, Ops: tp.Ops, Busy: tp.Busy, Errs: tp.Errs,
		PerSec:   tp.PerSec(),
		P50Micro: tp.Latency.P50.Microseconds(),
		P99Micro: tp.Latency.P99.Microseconds(),
		Journal:  journal,
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
