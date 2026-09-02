// コマンド worker は、資料 §2〜§5 の設計をひとまとめに動かすデモ。
//
//	MYSQL_DSN=... go run ./cmd/worker -tenants 3 -robots 200 -duration 60s
//
// 外部サービスは内蔵の代役（fakesvc）を使うので、これだけで動く。
// 途中で次の事故を意図的に起こし、設計が耐えることを目で見られるようにしてある。
//
//	t+10s  サービスが WebSocket への配信を止める（接続は生きたまま）
//	t+20s  API が落ちる（状態は「不明」のまま据え置かれる）
//	t+30s  API 復旧・名簿に新しい対象が増える（A でしか拾えない）
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/app"
	"github.com/aq35/sample_manual/internal/fakesvc"
	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/store"
)

func main() {
	var (
		dsn       = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN（既定は環境変数 MYSQL_DSN）")
		tenants   = flag.Int("tenants", 3, "テナント数（テナントごとにワーカー1台）")
		robots    = flag.Int("robots", 200, "1テナントあたりの対象数")
		duration  = flag.Duration("duration", 60*time.Second, "動かす時間")
		container = flag.String("container", "container-1", "このプロセスの識別子（リースの持ち主名）")
		maxOpen   = flag.Int("max-open", 20, "MaxOpenConns（コンテナ数 × これ ≤ DB 予算, §5.1）")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *dsn == "" {
		log.Error("MYSQL_DSN が未設定。scripts/mysql-up.sh を参照")
		os.Exit(1)
	}

	// ★プールはプロセスに1つ。全テナントのワーカーが共有する（§5.1）
	cfg := store.DefaultPool()
	cfg.MaxOpenConns = *maxOpen
	cfg.MaxIdleConns = *maxOpen
	if err := cfg.Validate(0); err != nil {
		log.Error("接続プールの設定", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(*dsn, cfg)
	if err != nil {
		log.Error("DB に接続できない", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	if err := st.Migrate(ctx); err != nil {
		log.Error("スキーマ作成に失敗", "err", err)
		os.Exit(1)
	}

	// 外部サービスの代役
	svc := fakesvc.New()
	defer svc.Close()

	leases := lease.NewManager(st.DB(), 10*time.Second)

	var wg sync.WaitGroup
	apps := make([]*app.App, 0, *tenants)
	for i := 0; i < *tenants; i++ {
		tenant := model.TenantID(fmt.Sprintf("t-%03d", i))

		// §2.8 リース: 担当を取れたテナントだけ動かす
		if _, ok, err := leases.Acquire(ctx, tenant, *container); err != nil {
			log.Error("リース取得に失敗", "tenant", tenant, "err", err)
			continue
		} else if !ok {
			log.Info("別のコンテナが担当中なので起動しない（二重稼働の防止, §2.8）", "tenant", tenant)
			continue
		}

		for r := 0; r < *robots; r++ {
			svc.Add(fmt.Sprintf("%s-r%04d", tenant, r), "running", true, 80)
		}

		a := app.New(app.Config{
			Tenant:         tenant,
			APIURL:         svc.URL() + "/robots?prefix=" + string(tenant) + "-",
			WSURL:          svc.WSURL() + "?prefix=" + string(tenant) + "-",
			FullSyncEvery:  20 * time.Second, // デモ用に短くしてある（本番は 5〜15分）
			FreshnessEvery: 5 * time.Second,  // 同上（本番は 30〜60秒）
			StaleAfter:     4 * time.Second,
			BatchEvery:     200 * time.Millisecond,
			TouchBase:      10 * time.Second,
			TouchJitter:    10 * time.Second,
			PingEvery:      3 * time.Second,
			PongWait:       2 * time.Second,
			BatteryStep:    5,
			StartupJitter:  2 * time.Second, // §3.4 デプロイ時の山を防ぐ
			Logger:         log.With("tenant", tenant),
		}, st)
		apps = append(apps, a)

		wg.Add(1)
		go func() {
			defer wg.Done()
			// リースの延長を続けている間だけ担当する（§2.8）
			go renew(ctx, leases, tenant, *container, log)
			if err := a.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("ワーカーが停止した", "tenant", tenant, "err", err)
			}
		}()
	}

	// 状態変化を流し続ける（変化率 1% 程度）
	wg.Add(1)
	go func() { defer wg.Done(); publish(ctx, svc, *tenants, *robots) }()

	// 事故のシナリオ
	wg.Add(1)
	go func() { defer wg.Done(); scenario(ctx, svc, log, *tenants, *robots) }()

	// §7 の観測: 毎秒 received / changed / touched / skipped を出す
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			for _, a := range apps {
				fmt.Println(a.Status())
			}
			fmt.Println()
		}
	}
	wg.Wait()
	fmt.Println("最終状態:")
	for _, a := range apps {
		fmt.Println(a.Status())
	}
}

func renew(ctx context.Context, m *lease.Manager, tenant model.TenantID, owner string, log *slog.Logger) {
	t := time.NewTicker(m.TTL() / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = m.Release(context.WithoutCancel(ctx), tenant, owner)
			return
		case <-t.C:
			ok, err := m.Renew(ctx, tenant, owner)
			if err != nil {
				log.Warn("リース延長に失敗", "tenant", tenant, "err", err)
			} else if !ok {
				// 担当を失った = 別のコンテナが拾った。ここで止めるのが正しい。
				log.Warn("担当を失ったのでワーカーを止めるべき状態", "tenant", tenant)
			}
		}
	}
}

func publish(ctx context.Context, svc *fakesvc.Service, tenants, robots int) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	statuses := []string{"running", "stopped", "error"}

	// サービス側の現在値。大半の配信は「同じ状態の再送」で、
	// 実際に値が変わるのは 1% 程度（§4.1 の前提を再現する）。
	type cur struct {
		status string
		batt   float64
	}
	state := map[string]*cur{}
	for tn := 0; tn < tenants; tn++ {
		for r := 0; r < robots; r++ {
			state[fmt.Sprintf("t-%03d-r%04d", tn, r)] = &cur{status: "running", batt: 80}
		}
	}

	t := time.NewTicker(2 * time.Millisecond) // 約500件/秒
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			id := fmt.Sprintf("t-%03d-r%04d", rnd.Intn(tenants), rnd.Intn(robots))
			c, ok := state[id]
			if !ok {
				continue
			}
			if rnd.Float64() < 0.01 { // ここだけが「実際の変化」
				c.status = statuses[rnd.Intn(len(statuses))]
				c.batt = float64(rnd.Intn(100))
			}
			svc.Publish(id, c.status, c.status != "stopped", c.batt)
		}
	}
}

func scenario(ctx context.Context, svc *fakesvc.Service, log *slog.Logger, tenants, robots int) {
	steps := []struct {
		at time.Duration
		do func()
	}{
		{10 * time.Second, func() {
			log.Warn("シナリオ: サービスが WebSocket への配信を止めた（接続は生きたまま）")
			svc.SetSilent(true)
			// 沈黙の間に状態が変わる。WebSocket では気づけない
			for i := 0; i < robots/10; i++ {
				svc.SetQuietly(fmt.Sprintf("t-000-r%04d", i), "error", true, 3)
			}
		}},
		{20 * time.Second, func() {
			log.Warn("シナリオ: API が落ちた（状態は『不明』のまま据え置かれる）")
			svc.SetAPIDown(true)
		}},
		{30 * time.Second, func() {
			log.Warn("シナリオ: API 復旧＋名簿に新しい対象を追加（WebSocket には流れない＝A でしか拾えない）")
			svc.SetAPIDown(false)
			svc.SetSilent(false)
			for i := 0; i < tenants; i++ {
				svc.Add(fmt.Sprintf("t-%03d-r9999", i), "running", true, 100)
			}
		}},
	}
	start := time.Now()
	for _, s := range steps {
		d := s.at - time.Since(start)
		if d < 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			s.do()
		}
	}
}
