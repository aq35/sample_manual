package repo

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/store"
)

// Options はリポジトリ層の調整値。既定値のままで安全側に倒れるようにしてある。
type Options struct {
	Pool store.PoolConfig

	// SlowQuery: これを超えた問い合わせを警告として記録する（既定 100ms）。
	SlowQuery time.Duration
	// MaxTxDuration: トランザクションを開いたままにしてよい上限（既定 1s）。
	// 超えたら警告する。長いトランザクションは undo を伸ばす（調査 §4.6）。
	MaxTxDuration time.Duration
	// MaxRetries: デッドロック時の再試行回数（既定 3）。
	MaxRetries int
	// MaxRows: 1回の取得で許す行数の上限（既定 1000）。超えたら ErrTooManyRows。
	MaxRows int
	// MaxLockHold: 排他ロックをこれより長く持っていたら警告する（既定 1分）。
	// GET_LOCK は接続を1本占有するので、長く持つ用途には向かない（docs/locking.md 3.7）。
	MaxLockHold time.Duration
	// MigrateLockWait: マイグレーションの排他ロックを待つ時間（既定 30秒）。
	// 先に起動したコンテナが当て終わるのを待つ時間なので、いちばん長い
	// マイグレーションより長くしておく。
	MigrateLockWait time.Duration

	Logger *slog.Logger

	// MigrationHook は、マイグレーションの各段階で呼ばれる（既定は nil）。
	// 実験（EXP-6）で「この段階でプロセスを落とす」ために使う。
	// 本番のコードは何も渡さないので、実行時の挙動は変わらない。
	MigrationHook func(stage string)
}

func (o *Options) setDefaults() {
	if o.Pool.MaxOpenConns == 0 {
		o.Pool = store.DefaultPool()
	}
	if o.SlowQuery == 0 {
		o.SlowQuery = 100 * time.Millisecond
	}
	if o.MaxTxDuration == 0 {
		o.MaxTxDuration = time.Second
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 3
	}
	if o.MaxRows == 0 {
		o.MaxRows = 1000
	}
	if o.MaxLockHold == 0 {
		o.MaxLockHold = time.Minute
	}
	if o.MigrateLockWait == 0 {
		o.MigrateLockWait = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Stats は運用で見る数字。テストでも使う（N+1 の検出）。
type Stats struct {
	Queries      int64 // SELECT の回数
	Execs        int64 // INSERT/UPDATE/DELETE の回数
	Txs          int64 // トランザクション数
	Retries      int64 // デッドロック等でやり直した回数
	SlowQueries  int64 // SlowQuery を超えた回数
	LongTxs      int64 // MaxTxDuration を超えた回数
	Blocked      int64 // 検査で止めた回数（テナント忘れ・危険な SQL・行数違い）
	CrossTenant  int64 // テナントを跨ぐ操作を明示的に行った回数
	RowsAffected int64
	Locks        int64 // 排他ロック（GET_LOCK）を取った回数
}

type counters struct {
	queries, execs, txs, retries atomic.Int64
	slow, longTx, blocked, cross atomic.Int64
	rowsAffected                 atomic.Int64
	locks                        atomic.Int64
}

func (c *counters) snapshot() Stats {
	return Stats{
		Queries:      c.queries.Load(),
		Execs:        c.execs.Load(),
		Txs:          c.txs.Load(),
		Retries:      c.retries.Load(),
		SlowQueries:  c.slow.Load(),
		LongTxs:      c.longTx.Load(),
		Blocked:      c.blocked.Load(),
		CrossTenant:  c.cross.Load(),
		RowsAffected: c.rowsAffected.Load(),
		Locks:        c.locks.Load(),
	}
}

// DB はプロセスに1つ持つハンドル。★テナントごとに作らない（調査 §5.1）。
type DB struct {
	sqldb  *sql.DB
	opt    Options
	cnt    counters
	schema atomic.Value // string。ロック名に付けるスキーマ名（初回に一度だけ問い合わせる）
}

// Open は接続プールを1つ作る。
//
// DSN には次を強制する。
//   - parseTime=true / loc=UTC     時刻を time.Time で受け、時差の事故を避ける
//   - sql_safe_updates=1           キー列を使わない UPDATE/DELETE を MySQL 側でも止める
//
// 実験（TestExperiment_safe_updatesは何を止めるか）のとおり、これは万能ではないが、
// 「WHERE を書き忘れた」型の事故はここで止まる。
func Open(dsn string, opt Options) (*DB, error) {
	opt.setDefaults()
	if err := opt.Pool.Validate(0); err != nil {
		return nil, err
	}
	full, err := hardenDSN(dsn)
	if err != nil {
		return nil, err
	}
	sqldb, err := sql.Open("mysql", full)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(opt.Pool.MaxOpenConns)
	sqldb.SetMaxIdleConns(opt.Pool.MaxIdleConns)
	sqldb.SetConnMaxLifetime(opt.Pool.ConnMaxLifetime)
	sqldb.SetConnMaxIdleTime(opt.Pool.ConnMaxIdleTime)
	return &DB{sqldb: sqldb, opt: opt}, nil
}

// NewFromDB は既にある *sql.DB を包む（移行時に使う）。
// この経路では DSN の強制ができないので、sql_safe_updates は自分で設定すること。
func NewFromDB(sqldb *sql.DB, opt Options) *DB {
	opt.setDefaults()
	return &DB{sqldb: sqldb, opt: opt}
}

func hardenDSN(dsn string) (string, error) {
	at := strings.LastIndex(dsn, "@")
	base, query := dsn, ""
	if i := strings.Index(dsn[max(at, 0):], "?"); i >= 0 {
		i += max(at, 0)
		base, query = dsn[:i], dsn[i+1:]
	}
	v, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("DSN のパラメータを解釈できない: %w", err)
	}
	v.Set("parseTime", "true")
	if v.Get("loc") == "" {
		v.Set("loc", "UTC")
	}
	v.Set("sql_safe_updates", "1")
	return base + "?" + v.Encode(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (d *DB) Close() error     { return d.sqldb.Close() }
func (d *DB) SQL() *sql.DB     { return d.sqldb }
func (d *DB) Stats() Stats     { return d.cnt.snapshot() }
func (d *DB) Options() Options { return d.opt }

func (d *DB) Ping(ctx context.Context) error { return d.sqldb.PingContext(ctx) }

// Tenant はテナントに束縛したハンドルを返す。**SQL はここからしか出せない。**
//
// 実験（TestExperiment_テナント指定を忘れる）で見たとおり、
// 「毎回 tenant_id を書く」設計はいつか忘れる。書く場所を1か所に閉じ込める。
func (d *DB) Tenant(t model.TenantID) *Scope {
	return &Scope{db: d, tenant: t, ex: d.sqldb}
}

// Unscoped はテナントを跨ぐ操作のための逃げ道（移行・管理業務・集計）。
//
// 使うたびに理由つきで記録される。**業務コードから呼ばれていたら設計を疑う。**
func (d *DB) Unscoped(reason string) *Scope {
	d.cnt.cross.Add(1)
	d.opt.Logger.Warn("テナントを跨ぐ操作", "理由", reason)
	return &Scope{db: d, tenant: "*", ex: d.sqldb, crossTenant: true, reason: reason}
}
