package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aq35/sample_manual/internal/model"
)

// executor は *sql.DB と *sql.Tx の共通部分。
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Scope はテナントに束縛した実行ハンドル。
//
// リポジトリの実装はこれだけを受け取る。*sql.DB を直接触らせないことで、
// 「テナント指定を忘れた SQL」が書けないようにする。
type Scope struct {
	db     *DB
	tenant model.TenantID
	ex     executor

	tx          *sql.Tx
	crossTenant bool
	reason      string
	unbounded   bool
}

// Tenant は束縛されているテナント。
func (s *Scope) Tenant() model.TenantID { return s.tenant }

// InTx はトランザクションの中かどうか。
func (s *Scope) InTx() bool { return s.tx != nil }

// AllowUnbounded は「この1回だけ LIMIT 無しの SELECT を許す」ハンドルを返す。
// 全件同期のように、本当に全部要る場面のための逃げ道。使う場所が目で見えるのが要点。
func (s *Scope) AllowUnbounded() *Scope {
	c := *s
	c.unbounded = true
	return &c
}

func (s *Scope) opts() statementOptions {
	return statementOptions{allowCrossTenant: s.crossTenant, allowUnbounded: s.unbounded}
}

func (s *Scope) optsSingleRow() statementOptions {
	o := s.opts()
	o.singleRow = true
	return o
}

// Query は SELECT。SQL には :tenant を書くこと。
//
//	rows, err := sc.Query(ctx, `SELECT id, name FROM robot_profile
//	                            WHERE tenant_id = :tenant AND id > ? ORDER BY id LIMIT ?`, lastID, limit)
func (s *Scope) Query(ctx context.Context, op, query string, args ...any) (*sql.Rows, error) {
	bound, boundArgs, err := s.prepare(op, query, args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	rows, err := s.ex.QueryContext(ctx, bound, boundArgs...)
	s.db.cnt.queries.Add(1)
	s.observe(op, query, start)
	if err != nil {
		return nil, wrap(op, string(s.tenant), query, err)
	}
	return rows, nil
}

// QueryRow は1件取得。無ければ ErrNotFound を返す（sql.ErrNoRows を包む）。
func (s *Scope) QueryRow(ctx context.Context, op, query string, args ...any) *Row {
	bound, boundArgs, err := s.prepareWith(op, query, args, s.optsSingleRow())
	if err != nil {
		return &Row{err: err}
	}
	start := time.Now()
	row := s.ex.QueryRowContext(ctx, bound, boundArgs...)
	s.db.cnt.queries.Add(1)
	s.observe(op, query, start)
	return &Row{row: row, op: op, tenant: string(s.tenant), query: query}
}

// Row は *sql.Row の薄い包み。Scan のエラーを型に寄せる。
type Row struct {
	row    *sql.Row
	err    error
	op     string
	tenant string
	query  string
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if err := r.row.Scan(dest...); err != nil {
		return wrap(r.op, r.tenant, r.query, err)
	}
	return nil
}

// Exec は INSERT / UPDATE / DELETE。
//
// ★影響行数の宣言（Expect）が必須。実験のとおり、WHERE を間違えても MySQL は
// エラーを返さないので、宣言と突き合わせるしか止める方法が無い。
// トランザクションの外で宣言つきの Exec を呼んだ場合は、暗黙にトランザクションを張って
// 違反時にロールバックする。
func (s *Scope) Exec(ctx context.Context, op, query string, expect Expect, args ...any) (int64, error) {
	if !s.InTx() && expect.checked() {
		var affected int64
		err := s.Tx(ctx, func(tx *Scope) error {
			n, err := tx.Exec(ctx, op, query, expect, args...)
			affected = n
			return err
		})
		return affected, err
	}

	bound, boundArgs, err := s.prepare(op, query, args)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	res, err := s.ex.ExecContext(ctx, bound, boundArgs...)
	s.db.cnt.execs.Add(1)
	s.observe(op, query, start)
	if err != nil {
		return 0, wrap(op, string(s.tenant), query, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, wrap(op, string(s.tenant), query, err)
	}
	s.db.cnt.rowsAffected.Add(n)
	if err := expect.check(n); err != nil {
		s.db.cnt.blocked.Add(1)
		s.db.opt.Logger.Error("影響行数が想定と違うのでロールバックする",
			"op", op, "tenant", s.tenant, "affected", n, "sql", query)
		return n, wrap(op, string(s.tenant), query, err)
	}
	return n, nil
}

// prepare は検査と :tenant の束縛。ここを通らない SQL は実行されない。
func (s *Scope) prepare(op, query string, args []any) (string, []any, error) {
	return s.prepareWith(op, query, args, s.opts())
}

func (s *Scope) prepareWith(op, query string, args []any, o statementOptions) (string, []any, error) {
	// SQL 文はコード中の定数なので、検査と書き換えの結果は覚えておける
	bound, boundArgs, err := compile(query, o).bind(string(s.tenant), args)
	if err != nil {
		s.db.cnt.blocked.Add(1)
		return "", nil, wrap(op, string(s.tenant), query, err)
	}
	return bound, boundArgs, nil
}

func (s *Scope) observe(op, query string, start time.Time) {
	d := time.Since(start)
	if d >= s.db.opt.SlowQuery {
		s.db.cnt.slow.Add(1)
		s.db.opt.Logger.Warn("遅い問い合わせ", "op", op, "tenant", s.tenant, "所要", d, "sql", query)
	}
}

// Tx はトランザクション。関数が nil を返せばコミット、エラーならロールバックする。
//
//   - デッドロック（1213）とロック待ちタイムアウト（1205）は自動でやり直す
//   - すでにトランザクションの中なら、そのまま同じトランザクションで実行する（入れ子にしない）
//   - 開いている時間が長いと警告する（長いトランザクションは undo を伸ばす）
//
// ★ここで外部 API を呼ばないこと（調査 §4.6）。やり直しが二重送信になる。
func (s *Scope) Tx(ctx context.Context, fn func(*Scope) error) error {
	if s.InTx() {
		return fn(s) // 入れ子は「参加」にする。二重コミットを防ぐ
	}

	var lastErr error
	for attempt := 0; attempt <= s.db.opt.MaxRetries; attempt++ {
		if attempt > 0 {
			s.db.cnt.retries.Add(1)
			select {
			case <-time.After(time.Duration(attempt*attempt) * 10 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := s.runTx(ctx, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if !Retryable(err) {
			return err
		}
		s.db.opt.Logger.Warn("やり直す", "op", "tx", "tenant", s.tenant, "回数", attempt+1, "err", err)
	}
	return fmt.Errorf("%d 回やり直しても失敗した: %w", s.db.opt.MaxRetries+1, lastErr)
}

func (s *Scope) runTx(ctx context.Context, fn func(*Scope) error) (err error) {
	tx, err := s.db.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return wrap("tx.begin", string(s.tenant), "", err)
	}
	s.db.cnt.txs.Add(1)
	start := time.Now()

	scoped := *s
	scoped.ex = tx
	scoped.tx = tx

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if d := time.Since(start); d > s.db.opt.MaxTxDuration {
			s.db.cnt.longTx.Add(1)
			s.db.opt.Logger.Warn("トランザクションが長い（undo が伸びる。外部 API を呼んでいないか）",
				"tenant", s.tenant, "所要", d)
		}
	}()

	if err := fn(&scoped); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w（ロールバックにも失敗: %v）", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrap("tx.commit", string(s.tenant), "", err)
	}
	return nil
}
