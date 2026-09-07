// Command dispatcher は、実験の結論を結線した「統合ワーカー」を単体で動かす。
//
//	MYSQL_DSN=... go run ./cmd/dispatcher -tenants 5 -seed 20 -duration 20s
//
// internal/tenantworker を使い、lease で担当を決め、担当テナントを1クエリに畳んで
// ポーリングし、テナント別に切り分けて処理し、実績を独立に記録する。
// Web（cmd/web）とは別プロセス。接続予算は起動時に poolbudget.Guard で確かめる。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/cadencelab"
	"github.com/aq35/sample_manual/internal/config"
	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/poolbudget"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/tenantworker"
)

func main() {
	var (
		nTenants = flag.Int("tenants", 5, "担当候補テナント数")
		seed     = flag.Int("seed", 20, "各テナントに入れるデモ命令数（0 で入れない）")
		duration = flag.Duration("duration", 20*time.Second, "動かす時間")
		owner    = flag.String("owner", "dispatcher-1", "この worker の識別子（lease の持ち主）")
		pool     = flag.Int("pool", 20, "MaxOpenConns")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 設定を1箇所で（DSN 必須 → fail-fast）
	l := config.New()
	dsn := l.Optional("MYSQL_DSN", "")
	if dsn == "" {
		log.Error("MYSQL_DSN が未設定。scripts/mysql-up.sh を参照")
		os.Exit(1)
	}
	dbMax := l.Int("DB_MAX_CONNECTIONS", 1000)
	reserved := l.Int("DB_RESERVED_CONNECTIONS", 100)
	workerReplicas := l.Int("WORKER_REPLICAS", 2)
	webReplicas := l.Int("WEB_REPLICAS", 40)
	webPool := l.Int("WEB_POOL", 20)

	// 接続予算（web と合算して DB 上限に収まるか）
	if err := poolbudget.Guard(dbMax, reserved,
		poolbudget.Role{Name: "worker", Containers: workerReplicas, PerContainer: *pool},
		poolbudget.Role{Name: "web", Containers: webReplicas, PerContainer: webPool},
	); err != nil {
		log.Error("接続予算オーバー", "err", err)
		os.Exit(1)
	}
	log.Info("設定", "audit", strings.Join(l.Audit(), " "))

	cfg := store.DefaultPool()
	cfg.MaxOpenConns = *pool
	cfg.MaxIdleConns = *pool
	st, err := store.Open(dsn, cfg)
	if err != nil {
		log.Error("DB に接続できない", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	if err := st.Migrate(runCtx); err != nil {
		log.Error("store migrate", "err", err)
		os.Exit(1)
	}
	if err := cadencelab.Setup(runCtx, st.DB()); err != nil { // cmd_command / cmd_result / cmd_schedule
		log.Error("cmd schema", "err", err)
		os.Exit(1)
	}

	tenants := make([]model.TenantID, *nTenants)
	for i := range tenants {
		tenants[i] = model.TenantID(fmt.Sprintf("disp-t%03d", i))
	}
	if *seed > 0 {
		if err := seedDemo(runCtx, st, tenants, *seed); err != nil {
			log.Error("seed", "err", err)
			os.Exit(1)
		}
		log.Info("デモ命令を投入", "tenants", *nTenants, "each", *seed)
	}

	// ハンドラ: 実際にロボットへ命令を出す代わりに、ログして成功を返す。
	// 本番はここで外部呼び出し。timeout は tenantworker.ErrOutcomeUnknown を返す（自動再実行しない）。
	var handled int
	handler := func(ctx context.Context, c tenantworker.Command) error {
		handled++
		return nil
	}
	leases := lease.NewManager(st.DB(), 10*time.Second)
	d := tenantworker.New(st.DB(), leases, tenants, handler,
		tenantworker.Options{Owner: *owner, Batch: 200,
			MinInterval: 100 * time.Millisecond, MaxInterval: 2 * time.Second})

	// 定期的に状態を出す
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				log.Info("状態", "owned", d.Stats.Owned.Load(), "polls", d.Stats.Polls.Load(),
					"empty", d.Stats.EmptyPolls.Load(), "dispatched", d.Stats.Dispatched.Load(),
					"unknown", d.Stats.Unknown.Load(), "failed", d.Stats.Failed.Load(),
					"misrouted", d.Stats.Misrouted.Load())
			}
		}
	}()

	log.Info("dispatcher 起動", "owner", *owner, "tenants", *nTenants, "pool", *pool)
	if err := d.Run(runCtx); err != nil {
		log.Error("dispatcher", "err", err)
	}
	log.Info("停止", "dispatched", d.Stats.Dispatched.Load(), "misrouted", d.Stats.Misrouted.Load())
}

func seedDemo(ctx context.Context, st *store.Store, tenants []model.TenantID, per int) error {
	db := st.DB()
	for _, tn := range tenants {
		//smlint:allow loopquery 理由: デモ用の初期投入。テナントごとに1回まとめて入れる
		//smlint:allow rowsaffected 理由: 投入
		if _, err := db.ExecContext(ctx, "DELETE FROM cmd_command WHERE tenant_id=?", string(tn)); err != nil {
			return err
		}
		for i := 0; i < per; i++ {
			id := fmt.Sprintf("%s-c%04d", tn, i)
			//smlint:allow loopquery 理由: デモ用の初期投入
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx,
				`INSERT INTO cmd_command (tenant_id, command_id, robot_id, type, payload, state, idem_key, scheduled_for)
				 VALUES (?,?,?, 'move','', 'pending', ?, ?)`,
				string(tn), id, fmt.Sprintf("r%02d", i%10), "idem-"+id, time.Now().Add(-time.Second)); err != nil {
				return err
			}
		}
	}
	return nil
}
