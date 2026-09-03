// コマンド effectlab は EXP-1 の子プロセス。
//
// 親（テスト）がこれを起動し、指定した地点で SIGKILL する。
// 「プロセスが本当に死ぬ」ことが実験の前提なので、goroutine で模擬しない。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aq35/sample_manual/internal/effectlab"
	"github.com/aq35/sample_manual/internal/expkit"
)

func main() {
	var (
		dsn      = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN")
		service  = flag.String("service", "", "外部サービスの URL")
		tenant   = flag.String("tenant", "t-exp1", "テナント")
		strategy = flag.String("strategy", string(effectlab.StrategyNaive), "方式")
		requests = flag.Int("requests", 5, "要求の件数")
		killOn   = flag.Int("kill-on", 0, "何番目の要求で故障注入するか（0 = しない）")
		recover_ = flag.Bool("recover", false, "再起動後の処理として走る")
		timeout  = flag.Duration("http-timeout", 2*time.Second, "外部サービスの応答待ち")
	)
	flag.Parse()

	if *dsn == "" || *service == "" {
		fmt.Fprintln(os.Stderr, "dsn と service は必須")
		os.Exit(2)
	}

	db, err := effectlab.Open(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "DB:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	lab := effectlab.New(effectlab.Config{
		DSN:         *dsn,
		ServiceURL:  *service,
		Tenant:      *tenant,
		Strategy:    effectlab.Strategy(*strategy),
		Requests:    *requests,
		KillOn:      *killOn,
		HTTPTimeout: *timeout,
		Recover:     *recover_,
		KS:          expkit.NewKillSwitch(),
		Out:         os.Stdout,
	}, db)

	if *recover_ {
		err = lab.Recover(ctx)
	} else {
		err = lab.Run(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%sDONE\n", expkit.MarkerPrefix)
}
