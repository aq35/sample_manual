package gql

import (
	"context"
	"errors"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/repo"
)

// ServerConfig は GraphQL サーバのベストプラクティス設定。
type ServerConfig struct {
	// ComplexityLimit: 1クエリの複雑度上限。深い/広いクエリを実行前に弾く（DoS 対策・EXP-24）。
	ComplexityLimit int
	// Introspection: スキーマ内観を許すか。本番は false 推奨（攻撃面を減らす）。
	Introspection bool
	// MaxPageSize: robots(first) の上限。
	MaxPageSize int
	// AllowList: 非 nil なら「登録済みクエリ以外を実行しない」（永続化クエリ・EXP-26）。
	AllowList *AllowList
	// RateLimiter: 非 nil ならテナント単位のレート制限（EXP-26）。
	RateLimiter *RateLimiter
}

// DefaultServerConfig は本番向けの既定（内観オフ・複雑度上限あり）。
func DefaultServerConfig() ServerConfig {
	return ServerConfig{ComplexityLimit: 200, Introspection: false, MaxPageSize: defaultMaxPageSize}
}

// NewServer は best-practice を適用した gqlgen ハンドラを作る。
func NewServer(db *repo.DB, cfg ServerConfig) *handler.Server {
	r := &Resolver{DB: db, MaxPageSize: cfg.MaxPageSize}

	c := Config{Resolvers: r}
	// @auth ディレクティブ（フィールドの必要ロールを解決前に検査する）。
	c.Directives.Auth = authDirective
	// 複雑度: commands / robots は取得件数(first)に比例して重い。childComplexity×first を計上する。
	c.Complexity.Robot.Commands = func(childComplexity int, first *int) int {
		n := 20
		if first != nil {
			n = *first
		}
		return childComplexity * n
	}
	c.Complexity.Query.Robots = func(childComplexity int, first int, after *string) int {
		return childComplexity * first
	}

	srv := handler.New(NewExecutableSchema(c))
	srv.AddTransport(transport.POST{})
	// パース済みクエリをキャッシュ（同じクエリの再パースを避ける）。
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))

	// 永続化クエリ allowlist（登録済み以外は実行しない）。
	if cfg.AllowList != nil {
		srv.Use(cfg.AllowList)
	}
	// テナント単位のレート制限。
	if cfg.RateLimiter != nil {
		srv.Use(cfg.RateLimiter)
	}
	// 複雑度の上限（DoS 対策）。
	if cfg.ComplexityLimit > 0 {
		srv.Use(extension.FixedComplexityLimit(cfg.ComplexityLimit))
	}
	// スキーマ内観は本番では切る。
	if cfg.Introspection {
		srv.Use(extension.Introspection{})
	}

	// エラーは中身を漏らさない（SQL や内部エラーを client に出さない）。
	// 想定内のもの（テナント無し等）だけメッセージを見せ、他は一般化する。
	srv.SetErrorPresenter(func(ctx context.Context, e error) *gqlerror.Error {
		gqlErr := graphql.DefaultErrorPresenter(ctx, e)
		switch {
		case errors.Is(e, ErrNoTenant):
			gqlErr.Message = "unauthenticated"
		case errors.Is(e, repo.ErrTooManyRows) || errors.Is(e, repo.ErrTooCostly):
			gqlErr.Message = "request too large"
		case isPublic(gqlErr):
			// クライアント起因（入力不正・認可拒否・受付拒否）はそのまま見せてよい。
		default:
			// 内部エラーは詳細を隠す（ログには別途出す前提）。
			gqlErr.Message = "internal error"
		}
		return gqlErr
	})
	return srv
}

// Middleware は「認証済みテナントの取り出し」と「リクエストごとのローダ設置」を行う HTTP ミドルウェア。
//
// tenantOf は実際の認証（JWT/セッション）からテナントを返す関数に差し替える。テナントを
// GraphQL 引数から取ってはいけない（詐称される・EXP-24）。withLoaders=false なら素朴経路（N+1）。
func Middleware(db *repo.DB, tenantOf func(*http.Request) (model.TenantID, bool), withLoaders bool, next http.Handler) http.Handler {
	return MiddlewareWithRole(db, tenantOf, nil, withLoaders, next)
}

// MiddlewareWithRole は Middleware に加えて、認証済みロールを context に載せる（@auth 用）。
// roleOf が nil、または (,false) を返した場合はロール未設定（@auth フィールドは拒否される）。
func MiddlewareWithRole(db *repo.DB, tenantOf func(*http.Request) (model.TenantID, bool),
	roleOf func(*http.Request) (Role, bool), withLoaders bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t, ok := tenantOf(req)
		ctx := req.Context()
		if ok && t != "" {
			ctx = WithTenant(ctx, t)
			if roleOf != nil {
				if role, rok := roleOf(req); rok {
					ctx = WithRole(ctx, role)
				}
			}
			if withLoaders {
				ctx = WithLoaders(ctx, newLoaders(db.Tenant(t)))
			}
		}
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}
