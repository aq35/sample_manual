// Package mysqltest は、実際の MySQL に対してテストを走らせるための補助。
//
// 環境変数 MYSQL_DSN が無ければテストは skip される（CI で MySQL が無くても壊れない）。
//
//	export MYSQL_DSN='worker:workerpw@tcp(127.0.0.1:3306)/workerdb?parseTime=true&loc=UTC&multiStatements=false'
package mysqltest

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/store"
)

// DSN は MYSQL_DSN を返す。未設定ならテストを skip する。
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN が未設定のため skip（scripts/mysql-up.sh を参照）")
	}
	return dsn
}

// Store は移行済みの Store を返す。
func Store(t testing.TB, cfg store.PoolConfig) *store.Store {
	t.Helper()
	s, err := store.Open(DSN(t), cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.DB().PingContext(ctx); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// Raw は素の *sql.DB を返す（設定の実験用）。
func Raw(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", DSN(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	return db
}

// Truncate は指定テナントの行を消す。
func Truncate(t testing.TB, db *sql.DB, tenant string) {
	t.Helper()
	for _, tbl := range []string{"robot_state", "robot_state_history", "worker_lease"} {
		if _, err := db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", tenant); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
}
