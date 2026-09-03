// コマンド proxylab は EXP-5 の後半（RDS Proxy / 接続集約）を測るための道具。
//
// ★ここで確かめたい主張は、**本物の RDS Proxy が無いと検証できない**。
// このリポジトリの実行環境には AWS が無いので、
// 「実行したことにする」のではなく、**どこへ向けても走る道具**として置いてある。
//
//	go run ./cmd/proxylab -dsn "user:pass@tcp(<proxy-endpoint>:3306)/db"
//
// 直接 MySQL に向ければ「集約なし」の基準値が取れる。
// RDS Proxy に向ければ、同じ手順で pinning の有無を比較できる。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type scenario struct {
	name string
	desc string
	// run は「クライアント接続1本で行う操作」。接続は固定される（sql.Conn）。
	run func(ctx context.Context, c *sql.Conn) error
	// pinning は RDS Proxy で pinning を起こすと **文書で言われている** 操作か。
	// 実測で確かめるべき対象。
	suspectedPinning bool
}

func main() {
	var (
		dsn     = flag.String("dsn", os.Getenv("MYSQL_DSN"), "接続先（RDS Proxy でも直接でも可）")
		clients = flag.Int("clients", 8, "同時に張るクライアント接続の数")
		hold    = flag.Duration("hold", 2*time.Second, "操作後に接続を保持する時間")
	)
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "dsn は必須")
		os.Exit(2)
	}

	scenarios := []scenario{
		{"plain_select", "ふつうの SELECT", func(ctx context.Context, c *sql.Conn) error {
			var v int
			return c.QueryRowContext(ctx, "SELECT 1").Scan(&v)
		}, false},
		{"transaction", "トランザクションを開いて閉じる", func(ctx context.Context, c *sql.Conn) error {
			tx, err := c.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			var v int
			if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
				_ = tx.Rollback()
				return err
			}
			return tx.Commit()
		}, false},
		{"open_transaction", "トランザクションを開いたまま保持する", func(ctx context.Context, c *sql.Conn) error {
			if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
				return err
			}
			var v int
			return c.QueryRowContext(ctx, "SELECT 1").Scan(&v)
		}, true},
		{"session_variable", "セッション変数を設定する", func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, "SET @proxylab_var = 1")
			return err
		}, true},
		{"temp_table", "一時表を作る", func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, "CREATE TEMPORARY TABLE proxylab_tmp (id INT)")
			return err
		}, true},
		{"user_lock", "GET_LOCK でユーザーロックを取る", func(ctx context.Context, c *sql.Conn) error {
			var got sql.NullInt64
			return c.QueryRowContext(ctx, "SELECT GET_LOCK('proxylab', 0)").Scan(&got)
		}, true},
		{"prepared_statement", "プリペアドステートメントを保持する", func(ctx context.Context, c *sql.Conn) error {
			st, err := c.PrepareContext(ctx, "SELECT ?")
			if err != nil {
				return err
			}
			var v int
			if err := st.QueryRowContext(ctx, 1).Scan(&v); err != nil {
				_ = st.Close()
				return err
			}
			return nil // ★閉じずに保持する
		}, true},
	}

	ctx := context.Background()
	fmt.Printf("接続先: %s\n", maskDSN(*dsn))
	fmt.Printf("クライアント接続 %d 本・操作後 %v 保持\n\n", *clients, *hold)
	fmt.Printf("%-20s %-34s %10s %10s %10s\n", "シナリオ", "操作", "client", "server", "pinning疑い")

	for _, sc := range scenarios {
		clientConns, serverConns, err := measure(ctx, *dsn, *clients, *hold, sc)
		if err != nil {
			fmt.Printf("%-20s %-34s %10s %10s  %v\n", sc.name, sc.desc, "-", "-", err)
			continue
		}
		mark := ""
		if sc.suspectedPinning {
			mark = "文書上あり"
		}
		fmt.Printf("%-20s %-34s %10d %10d %10s\n", sc.name, sc.desc, clientConns, serverConns, mark)
	}

	fmt.Println()
	fmt.Println("読み方:")
	fmt.Println("  client == server なら集約されていない（直接接続、または pinning 中）")
	fmt.Println("  client >  server なら集約が効いている")
	fmt.Println("  RDS Proxy では CloudWatch の DatabaseConnectionsCurrentlySessionPinned も併せて見ること")
	fmt.Println("  （この道具はエンドポイントから見える範囲しか観測できない）")
}

// measure は clients 本の接続で操作を行い、保持中の
// 「クライアント接続数」と「サーバ側で見える接続数」を返す。
func measure(ctx context.Context, dsn string, clients int, hold time.Duration, sc scenario) (int, int, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(clients + 2)
	db.SetMaxIdleConns(clients + 2)

	if err := db.PingContext(ctx); err != nil {
		return 0, 0, err
	}

	conns := make([]*sql.Conn, 0, clients)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < clients; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			return 0, 0, err
		}
		conns = append(conns, c)
		if err := sc.run(ctx, c); err != nil {
			return 0, 0, fmt.Errorf("%s: %w", sc.name, err)
		}
	}

	time.Sleep(hold)

	// サーバ側から見えている接続数（同じユーザのもの）
	var serverConns int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.processlist WHERE user = SUBSTRING_INDEX(CURRENT_USER(), '@', 1)`).
		Scan(&serverConns)
	if err != nil {
		return len(conns), 0, nil // 権限が無い環境では取れないことがある
	}
	return len(conns), serverConns, nil
}

func maskDSN(dsn string) string {
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == ':' {
			for j := i + 1; j < len(dsn); j++ {
				if dsn[j] == '@' {
					return dsn[:i+1] + "***" + dsn[j:]
				}
			}
		}
	}
	return dsn
}
