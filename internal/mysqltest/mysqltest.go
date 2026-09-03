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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に接続できないため skip: %v", err)
	}
	return db
}

// Truncate は指定テナントの行を消す。
func Truncate(t testing.TB, db *sql.DB, tenant string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tbl := range []string{"robot_state", "robot_state_history", "worker_lease"} {
		//smlint:allow rowsaffected 理由: 後片付け。消える行が 0 でも正しい
		if _, err := db.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE tenant_id = ?", tenant); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
}

// Serialize は、この MySQL を使う実験テストを一度に1つだけ走らせる。
//
// ★これが要る理由（実際に踏んだ）:
// go test は **パッケージを並列に実行する**（既定 -p = CPU 数）。
// 実験テストはグローバル変数（innodb_flush_log_at_trx_commit など）を変え、
// 同じ名前の実験用テーブルを作っては消すので、並列に走ると
// 「他のテストがテーブルを消した」「グローバル設定が書き換わった」で落ちる。
// 落ち方が実行のたびに変わるため、**実装のバグに見えてしまう**のが厄介。
//
// GET_LOCK で直列化する。接続を固定する必要があるので db.Conn を使う（docs/locking.md 3.1）。
func Serialize(t testing.TB) {
	t.Helper()
	db := Raw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("直列化用の接続を取れない: %v", err)
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", serializeLock, 300).Scan(&got); err != nil {
		_ = conn.Close()
		t.Fatalf("直列化ロックを取れない: %v", err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		t.Fatalf("直列化ロックを 300 秒待っても取れなかった")
	}
	t.Cleanup(func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer relCancel()
		var released sql.NullInt64
		_ = conn.QueryRowContext(relCtx, "SELECT RELEASE_LOCK(?)", serializeLock).Scan(&released)
		_ = conn.Close()
	})
}

const serializeLock = "sample_manual.experiment_tests"
