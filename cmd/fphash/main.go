// Command fphash は、あるスキーマの指紋（content hash）を1行の JSON で出す。
//
// backuplab.Take と**同じ規則**で hash を作る。別プロセスから叩いて、
// 同じデータに同じ hash が出ること（＝hash 規則がプロセスに依存しないこと）を確かめるのに使う。
//
//	fphash -dsn '...' -tables robot_state,worker_lease
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/backuplab"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN")
	tables := flag.String("tables", "", "カンマ区切りの表名")
	flag.Parse()
	if *dsn == "" || *tables == "" {
		fmt.Fprintln(os.Stderr, "usage: fphash -dsn ... -tables a,b,c")
		os.Exit(2)
	}
	t, err := backuplab.Parse(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := openDB(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()
	_ = t
	fp, err := backuplab.Take(context.Background(), db, strings.Split(*tables, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, _ := json.Marshal(fp)
	fmt.Println(string(b))
}
