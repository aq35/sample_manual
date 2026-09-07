// Command web は「Web（リクエスト応答）」プロセス。Worker とは別プロセスにする。
//
// なぜ分けるか（実験の結論）:
//   - Web はユーザートラフィックでスケールし、Worker はテナント数（固定）でスケールする（性質が違う）。
//   - 混ぜると、Web のスパイクでオートスケールした瞬間に Worker も増え、lease 二重・接続飽和（EXP-5）。
//   - 分ければプールを別々にサイズでき、障害も独立する。
//
// 接続予算（EXP-5 / poolbudget）:
//
//	  web と worker を別プロセスにしても、合計が DB の上限を超えてはいけない。
//	  起動時に poolbudget.Guard で確かめ、超えるなら fail-fast する。
//
//		MYSQL_DSN=... WEB_POOL=20 DB_MAX_CONNECTIONS=1000 go run ./cmd/web
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aq35/sample_manual/internal/config"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/poolbudget"
	"github.com/aq35/sample_manual/internal/repo"
	"github.com/aq35/sample_manual/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// ---- 設定を1箇所で読む（config）。必須は fail-fast、任意は既定で動く ----
	l := config.New()
	dsn := l.Required("MYSQL_DSN")
	port := l.Optional("PORT", "8080")
	webPool := l.Int("WEB_POOL", 20)
	dbMax := l.Int("DB_MAX_CONNECTIONS", 1000)
	reserved := l.Int("DB_RESERVED_CONNECTIONS", 100)
	webReplicas := l.Int("WEB_REPLICAS", 40) // このデプロイの最大レプリカ数（予算計算用）
	workerReplicas := l.Int("WORKER_REPLICAS", 2)
	workerPool := l.Int("WORKER_POOL", 20)
	if err := l.Err(); err != nil {
		log.Error("設定が足りない", "err", err)
		os.Exit(1)
	}
	log.Info("設定", "audit", strings.Join(l.Audit(), " ")) // 値は出さない

	// ---- 接続予算を起動時に確かめる（web + worker の合計が DB 上限に収まるか）----
	if err := poolbudget.Guard(dbMax, reserved,
		poolbudget.Role{Name: "web", Containers: webReplicas, PerContainer: webPool},
		poolbudget.Role{Name: "worker", Containers: workerReplicas, PerContainer: workerPool},
	); err != nil {
		log.Error("接続予算オーバー（web/worker の合計が DB 上限を超える）", "err", err)
		os.Exit(1)
	}

	// ---- Web 専用のプール（Worker とは別。小さく保つ = EXP-5 の膝）----
	pool := store.DefaultPool()
	pool.MaxOpenConns = webPool
	pool.MaxIdleConns = webPool
	d, err := repo.Open(dsn, repo.Options{Pool: pool, Logger: log})
	if err != nil {
		log.Error("DB を開けない", "err", err)
		os.Exit(1)
	}
	defer func() { _ = d.Close() }()

	srv := &http.Server{Addr: ":" + port, Handler: routes(d, log), ReadHeaderTimeout: 5 * time.Second}

	// ---- graceful shutdown（EXP-3）: 受付を止め、処理中を流し切ってから閉じる ----
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		log.Info("web 起動", "port", port, "web_pool", webPool)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	log.Info("停止シグナル。受付を止めて流し切る")
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Error("graceful shutdown 失敗", "err", err)
	}
}

func routes(d *repo.DB, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// 生存確認（DB ping）
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Ping(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	// 準備確認（プールの空きを見る。飽和していれば readiness を落とす）
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		st := d.SQL().Stats()
		writeJSON(w, map[string]any{"open": st.OpenConnections, "in_use": st.InUse, "idle": st.Idle, "wait": st.WaitCount})
	})
	// テナント別のロボット一覧。★テナントは URL から取り、repo.Scope に束縛する（越えられない）。
	mux.HandleFunc("GET /tenants/{tenant}/robots", func(w http.ResponseWriter, r *http.Request) {
		tenant := model.TenantID(r.PathValue("tenant"))
		if tenant == "" {
			http.Error(w, "tenant required", http.StatusBadRequest)
			return
		}
		sc := d.Tenant(tenant) // ★このハンドルは tenant に束縛。別テナントの行は引けない
		page, err := repo.ProfileRepo{}.List(r.Context(), sc, repo.Keyset{Limit: 50, After: r.URL.Query().Get("after")})
		if err != nil {
			log.Error("list", "tenant", tenant, "err", err)
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"items": page.Items, "next": page.Next})
	})
	return logging(mux, log)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func logging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("req", "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
	})
}
