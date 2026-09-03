// コマンド fencelab は EXP-2 の子プロセス。
//
// 「担当（lease）を持っているつもりの worker が、実は失っていた」状況を
// 実際のプロセスで作るために、外から止められる（SIGSTOP 相当の）待ち地点を持つ。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/fencelab"
)

func main() {
	var (
		dsn       = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN")
		tenant    = flag.String("tenant", "t-exp2", "テナント")
		owner     = flag.String("owner", "w1", "この worker の名前")
		mode      = flag.String("mode", string(fencelab.ModeFencing), "方式")
		ttl       = flag.Duration("ttl", 2*time.Second, "lease の有効期間")
		skew      = flag.Duration("skew", 0, "このプロセスの時計のずれ")
		writes    = flag.Int("writes", 3, "書き込み回数")
		interval  = flag.Duration("interval", 100*time.Millisecond, "書き込み間隔")
		acquireIn = flag.Duration("acquire-timeout", 5*time.Second, "担当を取れるまで待つ時間")
		checkRen  = flag.Bool("check-renew", true, "書く前に lease の延長を確認する")
		stopOnLos = flag.Bool("stop-on-lost", true, "担当を失ったら書くのをやめる")
		pauseOn   = flag.Int("pause-before-write", 0, "この回の書き込み直前で止まり、SIGUSR1 を待つ")
		startVal  = flag.Int64("start-value", 1, "書き込む値の起点")
		hold      = flag.Duration("hold", 0, "書き終えたあと担当を持ったまま待つ時間")
		release   = flag.Bool("release", true, "終了時に担当を明け渡す")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "dsn は必須")
		os.Exit(2)
	}
	db, err := fencelab.Open(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "DB:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// SIGUSR1 で「止まっている地点」から再開する。
	// これで GC の長い停止や、OS に止められていた時間を、正確な位置で再現できる。
	resume := make(chan os.Signal, 1)
	signal.Notify(resume, syscall.SIGUSR1)

	ctx := context.Background()
	ks := expkit.NewKillSwitch()
	w := fencelab.NewWorker(db, *tenant, *owner, fencelab.Mode(*mode), *ttl, *skew)

	deadline := time.Now().Add(*acquireIn)
	acquired := false
	for time.Now().Before(deadline) {
		ok, err := w.Acquire(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "acquire:", err)
			os.Exit(1)
		}
		if ok {
			acquired = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !acquired {
		fmt.Printf("%sacquire_failed\n", expkit.MarkerPrefix)
		os.Exit(4)
	}
	fmt.Printf("%sacquired fence=%d\n", expkit.MarkerPrefix, w.Fence())
	_ = os.Stdout.Sync()

	for i := 1; i <= *writes; i++ {
		if *checkRen {
			ok, err := w.Renew(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "renew:", err)
				os.Exit(1)
			}
			if !ok {
				fmt.Printf("%slost_lease at write=%d\n", expkit.MarkerPrefix, i)
				_ = os.Stdout.Sync()
				if *stopOnLos {
					os.Exit(3)
				}
			}
		}
		ks.Point("before_write")

		if *pauseOn == i {
			// ★ここで止まる。外から見ると「生きているのに何もしていない」状態。
			// この間に lease は期限切れになり、別の worker が担当を取る。
			fmt.Printf("%spaused_before_write write=%d fence=%d\n", expkit.MarkerPrefix, i, w.Fence())
			_ = os.Stdout.Sync()
			<-resume
			fmt.Printf("%sresumed write=%d\n", expkit.MarkerPrefix, i)
			_ = os.Stdout.Sync()
		}

		accepted, err := w.Write(ctx, *startVal+int64(i))
		if err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Printf("%swrote seq=%d accepted=%v fence=%d\n", expkit.MarkerPrefix, i, accepted, w.Fence())
		_ = os.Stdout.Sync()
		time.Sleep(*interval)
	}

	if *hold > 0 {
		// 担当を持ったまま待つ（同時 claim の勝者を「同じ瞬間」で数えるため）
		fmt.Printf("%sholding\n", expkit.MarkerPrefix)
		_ = os.Stdout.Sync()
		deadline := time.Now().Add(*hold)
		for time.Now().Before(deadline) {
			if _, err := w.Renew(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "renew:", err)
			}
			time.Sleep(*ttl / 3)
		}
	}
	if *release {
		if err := w.Release(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "release:", err)
		}
	}
	fmt.Printf("%sDONE\n", expkit.MarkerPrefix)
}
