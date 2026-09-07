package tenantworker_test

// tenantworker の統合テスト（lease × fanout × backoff × テナント別処理 × fence）。
//
//	MYSQL_DSN=... go test ./internal/tenantworker/ -v

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/cadencelab"
	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/tenantworker"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", mysqltest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedCmds(t *testing.T, db *sql.DB, tenant string, n int) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, "DELETE FROM cmd_command WHERE tenant_id=?", tenant)
	_, _ = db.ExecContext(ctx, "DELETE FROM cmd_result WHERE tenant_id=?", tenant)
	_, _ = db.ExecContext(ctx, "DELETE FROM worker_lease WHERE tenant_id=?", tenant)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-c%04d", tenant, i)
		//nolint
		if _, err := db.ExecContext(ctx,
			`INSERT INTO cmd_command (tenant_id, command_id, robot_id, type, payload, state, idem_key, scheduled_for)
			 VALUES (?,?,?, 'move','', 'pending', ?, ?)`,
			tenant, id, fmt.Sprintf("r%02d", i%10), "idem-"+id, time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTenantWorker_統合_分離を保って捌く(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := cadencelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	// worker_lease 表を用意（store のスキーマ）
	mysqltest.Store(t, mustPool())

	tenants := []model.TenantID{"tw-a", "tw-b", "tw-c"}
	for _, tn := range tenants {
		seedCmds(t, db, string(tn), 20)
	}

	// ハンドラ: 受け取った命令のテナントを記録（越えを検出するため）
	var mu sync.Mutex
	seen := map[model.TenantID]map[string]bool{}
	handler := func(ctx context.Context, c tenantworker.Command) error {
		mu.Lock()
		if seen[c.Tenant] == nil {
			seen[c.Tenant] = map[string]bool{}
		}
		seen[c.Tenant][c.ID] = true
		// ★命令 ID には自分のテナント名が接頭辞として入っている。越えがあればここで気づける。
		mu.Unlock()
		return nil
	}

	leases := lease.NewManager(db, 10*time.Second)
	d := tenantworker.New(db, leases, tenants, handler,
		tenantworker.Options{Owner: "worker-1", Batch: 100})

	runCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(runCtx) }()

	// 全部捌けるまで待つ
	waitDrained(t, db, tenants, 60)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// ---- 検証 ----
	// 全命令が処理された
	if got := d.Stats.Dispatched.Load(); got != int64(len(tenants)*20) {
		t.Errorf("dispatched=%d want %d", got, len(tenants)*20)
	}
	// ★テナント越えの処理は 0
	if m := d.Stats.Misrouted.Load(); m != 0 {
		t.Errorf("テナント越えの処理が %d 件（0 のはず）", m)
	}
	// ハンドラは各テナントに、自分の命令だけを受け取った
	mu.Lock()
	for _, tn := range tenants {
		for id := range seen[tn] {
			if got := prefix(id); got != string(tn) {
				t.Errorf("テナント %s のハンドラが %s の命令を受けた（越え）", tn, got)
			}
		}
		if len(seen[tn]) != 20 {
			t.Errorf("テナント %s の処理数 %d（20 のはず）", tn, len(seen[tn]))
		}
	}
	mu.Unlock()
	// fan-out が畳めている（3テナントでもポーリングは数回）
	t.Logf("polls=%d empty=%d dispatched=%d owned=%d misrouted=%d",
		d.Stats.Polls.Load(), d.Stats.EmptyPolls.Load(), d.Stats.Dispatched.Load(),
		d.Stats.Owned.Load(), d.Stats.Misrouted.Load())
	// 実績が記録されている
	var results int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cmd_result WHERE tenant_id IN ('tw-a','tw-b','tw-c')").Scan(&results)
	if results != len(tenants)*20 {
		t.Errorf("cmd_result=%d want %d", results, len(tenants)*20)
	}
}

func TestTenantWorker_二重起動しない_leaseで排他(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db := openDB(t)
	if err := cadencelab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	mysqltest.Store(t, mustPool())

	tenant := model.TenantID("tw-x")
	seedCmds(t, db, string(tenant), 40)

	// 2つの worker が同じテナントを担当しようとする。lease で片方だけが処理する。
	var mu sync.Mutex
	processedBy := map[string]string{} // command_id -> owner
	makeHandler := func(owner string) tenantworker.Handler {
		return func(ctx context.Context, c tenantworker.Command) error {
			mu.Lock()
			processedBy[c.ID] = owner
			mu.Unlock()
			return nil
		}
	}
	leases := lease.NewManager(db, 10*time.Second)
	d1 := tenantworker.New(db, leases, []model.TenantID{tenant}, makeHandler("w1"), tenantworker.Options{Owner: "w1", Batch: 100})
	d2 := tenantworker.New(db, leases, []model.TenantID{tenant}, makeHandler("w2"), tenantworker.Options{Owner: "w2", Batch: 100})

	runCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = d1.Run(runCtx) }()
	go func() { defer wg.Done(); _ = d2.Run(runCtx) }()

	waitDrained(t, db, []model.TenantID{tenant}, 40)
	cancel()
	wg.Wait()

	// 各命令はちょうど1回だけ処理された（二重処理なし）
	mu.Lock()
	if len(processedBy) != 40 {
		t.Errorf("処理された命令 %d（40 のはず）", len(processedBy))
	}
	mu.Unlock()
	// 総 dispatched は 40（両 worker 合計でも二重にならない）
	total := d1.Stats.Dispatched.Load() + d2.Stats.Dispatched.Load()
	if total != 40 {
		t.Errorf("合計 dispatched=%d（40 のはず。二重処理している）", total)
	}
	t.Logf("w1=%d w2=%d 合計=%d（lease で片方だけが担当）",
		d1.Stats.Dispatched.Load(), d2.Stats.Dispatched.Load(), total)
}

func waitDrained(t *testing.T, db *sql.DB, tenants []model.TenantID, want int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		var pending int
		q := "SELECT COUNT(*) FROM cmd_command WHERE state='pending' AND tenant_id IN ("
		args := make([]any, len(tenants))
		for i, tn := range tenants {
			if i > 0 {
				q += ","
			}
			q += "?"
			args[i] = string(tn)
		}
		q += ")"
		_ = db.QueryRowContext(context.Background(), q, args...).Scan(&pending)
		if pending == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("注: 期限内に drain しきらなかった")
}

func prefix(id string) string {
	// "tw-a-c0001" → "tw-a"（最後の "-cNNNN" を落とす）
	if i := lastDashC(id); i >= 0 {
		return id[:i]
	}
	return id
}

func lastDashC(s string) int {
	for i := len(s) - 1; i >= 1; i-- {
		if s[i-1] == '-' && s[i] == 'c' {
			return i - 1
		}
	}
	return -1
}
