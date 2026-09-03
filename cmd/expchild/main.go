// コマンド expchild は、実験基盤（internal/expkit）自身を検査するための子プロセス。
//
// 実験の測定器そのものが壊れていると、その先の実験の数字が全部疑わしくなる。
// これは「測定器を疑う」ためのいちばん小さなプログラム。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

func main() {
	var (
		steps    = flag.Int("steps", 3, "通過する地点の数")
		hold     = flag.Duration("hold", 0, "最後の地点のあと待つ時間")
		graceful = flag.Bool("graceful", false, "SIGTERM を受けたら片付けて終わる")
	)
	flag.Parse()

	ks := expkit.NewKillSwitch()

	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigc {
			fmt.Printf("%sSIGNAL %v\n", expkit.MarkerPrefix, s)
			_ = os.Stdout.Sync()
			if *graceful {
				fmt.Printf("%sCLEANUP\n", expkit.MarkerPrefix)
				_ = os.Stdout.Sync()
				os.Exit(0)
			}
		}
	}()

	for i := 1; i <= *steps; i++ {
		ks.Point(fmt.Sprintf("step%d", i))
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	fmt.Printf("%sDONE\n", expkit.MarkerPrefix)
}
