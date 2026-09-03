// Command sizeprobe は「ドライバを1つ入れたときの実行ファイルの大きさ」と
// 「そのビルドが本当に動くか」を測るための最小の main。
//
//	go build -o /tmp/a ./cmd/sizeprobe                            # pure Go（既定）
//	CGO_ENABLED=1 go build -tags sqlite_cgo -o /tmp/b ./cmd/sizeprobe
//	/tmp/a /tmp/probe.db
//
// ★cgo 版は CGO_ENABLED=0 でも**ビルドは通る**。落ちるのは実行時。
// 「クロスコンパイルできるか」を go build の終了コードで判断すると取り違える。
// だからこの main は、開くだけでなく **書いて読み戻す**ところまでやる。
package main

import (
	"database/sql"
	"fmt"
	"os"
)

//smlint:allow nocontext 理由: ドライバ1つぶんの大きさを測るための最小の main。context を足すと測りたいものが変わる
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sizeprobe <db path>")
		os.Exit(2)
	}
	db, err := sql.Open(driverName, "file:"+os.Args[1])
	if err != nil {
		fail("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS probe (k INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		fail("create: %v", err)
	}
	//smlint:allow rowsaffected 理由: 書けたかどうかはエラーで判る。行数に意味は無い
	if _, err := db.Exec("INSERT OR REPLACE INTO probe (k, v) VALUES (1, 'ok')"); err != nil {
		fail("insert: %v", err)
	}
	var v, ver string
	if err := db.QueryRow("SELECT v, sqlite_version() FROM probe WHERE k = 1").Scan(&v, &ver); err != nil {
		fail("select: %v", err)
	}
	fmt.Printf("%s sqlite=%s write-read=%s\n", driverName, ver, v)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
