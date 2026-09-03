package repo_test

// リポジトリ層そのものの代金を測る。
//
//	MYSQL_DSN=... go test ./internal/repo/ -bench . -benchmem -run XXX
//
// 検査（SQL の点検と :tenant の束縛）が、DB への往復に対してどれくらいかを見る。
// ここが往復に比べて十分小さくなければ、「安全のために遅くする」ことになってしまう。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/repo"
)

const benchQuery = `SELECT name FROM robot_profile WHERE tenant_id = :tenant AND robot_id = ?`
const benchQueryRaw = `SELECT name FROM robot_profile WHERE tenant_id = ? AND robot_id = ?`

func benchSetup(b *testing.B) *repo.DB {
	b.Helper()
	db, err := repo.Open(mustDSN(b), repo.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Ping(ctx); err != nil {
		b.Skipf("MySQL に接続できないため skip: %v", err)
	}
	if err := repo.Migrate(ctx, db); err != nil {
		b.Fatal(err)
	}
	_, _ = db.SQL().ExecContext(ctx, "DELETE FROM robot_profile WHERE tenant_id = ?", "t-bench")
	_, err = db.SQL().ExecContext(ctx,
		`INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial) VALUES (?,?,?,?,?)`,
		"t-bench", "r0001", "ベンチ", "AGV-3000", "bench-SN0001")
	if err != nil {
		b.Fatal(err)
	}
	return db
}

// ① 素の database/sql
func BenchmarkRawSQL_PointRead(b *testing.B) {
	db := benchSetup(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		var name string
		if err := db.SQL().QueryRowContext(ctx, benchQueryRaw, "t-bench", "r0001").Scan(&name); err != nil {
			b.Fatal(err)
		}
	}
}

// ② リポジトリ層ごし（検査 + :tenant の束縛が入る）
func BenchmarkRepo_PointRead(b *testing.B) {
	db := benchSetup(b)
	ctx := context.Background()
	sc := db.Tenant("t-bench")
	b.ReportAllocs()
	for b.Loop() {
		var name string
		if err := sc.QueryRow(ctx, "bench.get", benchQuery, "r0001").Scan(&name); err != nil {
			b.Fatal(err)
		}
	}
}

// 検査そのもののコストは、repo パッケージ内の BenchmarkCheckAndBind を参照
// （キャッシュあり 147ns / なし 4.8µs）。
