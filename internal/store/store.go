// Package store は MySQL への書き込みを担当する。
//
// 実装しているのは §2.7（冪等な書き込み）、§4.3（バッチ）、§5（接続プール）、
// §4.6（InnoDB 固有の注意）。比較用に「やってはいけない書き方」も同居させてあり、
// ベンチマークで差を測れるようにしてある。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/worker"
)

//go:embed schema.sql
var schemaSQL string

// PoolConfig は §5.2 の設定。既定値のまま放置すると事故になる項目だけを持つ。
type PoolConfig struct {
	// MaxOpenConns: コンテナ数 × これ ≤ DB 予算（§5.1）。
	MaxOpenConns int
	// MaxIdleConns: ★MaxOpenConns と同じにする。
	// database/sql の既定値は 2 で、放置すると上限20で走らせてもアイドルは
	// 2本しか保たれず、残りは毎回切断・再接続になる（§5.2）。
	MaxIdleConns int
	// ConnMaxLifetime: 30分程度。
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime: ★MySQL の wait_timeout より短くする（§5.2）。
	// 長いと、サーバが切った接続をアプリが掴んだままになり、使った瞬間に落ちる。
	ConnMaxIdleTime time.Duration
}

// DefaultPool は §5.1 の例（DB 割当100・4コンテナ、20 は移行/調査用に空ける）。
func DefaultPool() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    20,
		MaxIdleConns:    20,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// Validate は §5.2 のチェックリストをコードで表したもの。
// waitTimeout は MySQL の wait_timeout（秒）。0 なら検査しない。
func (c PoolConfig) Validate(waitTimeout time.Duration) error {
	var problems []string
	if c.MaxOpenConns <= 0 {
		problems = append(problems, "MaxOpenConns が未設定（無制限）: コンテナ数×上限 が DB 予算を超える")
	}
	if c.MaxIdleConns < c.MaxOpenConns {
		problems = append(problems, fmt.Sprintf(
			"MaxIdleConns(%d) < MaxOpenConns(%d): 既定値は 2 で、差のぶんは毎回切断・再接続になる（§5.2）",
			c.MaxIdleConns, c.MaxOpenConns))
	}
	if c.ConnMaxIdleTime <= 0 {
		problems = append(problems, "ConnMaxIdleTime が未設定: サーバに切られた接続を掴み続ける（§5.2）")
	}
	if waitTimeout > 0 && c.ConnMaxIdleTime >= waitTimeout {
		problems = append(problems, fmt.Sprintf(
			"ConnMaxIdleTime(%s) >= wait_timeout(%s): サーバが先に切る（§5.2）",
			c.ConnMaxIdleTime, waitTimeout))
	}
	if len(problems) > 0 {
		return fmt.Errorf("接続プールの設定に問題がある:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Store は「プロセスに1つ」のプール（§5.1）。
// ★テナントごとに作らないこと。コンテナ数 × テナント数 × 接続数 で破綻する。
type Store struct {
	db *sql.DB
}

// Open はプールを1つ作る。テナントごとに呼ばないこと。
func Open(dsn string, cfg PoolConfig) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	return &Store{db: db}, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

// Migrate はスキーマを作る。schema.sql は go:embed で埋め込んである。
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

func splitStatements(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";") {
		// 行頭コメントを落としてから空判定する
		var lines []string
		for _, ln := range strings.Split(part, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// UpsertBatch は §2.7 + §4.3 の推奨実装。
//
//   - 1トランザクションにまとめる（fsync 削減）
//   - 主キー順にソートしてから投げる（デッドロック回避）
//   - ON DUPLICATE KEY UPDATE + IF で冪等（古い observed_at で上書きしない）
//
// 呼び出し側は成功したときだけ Tracker.Commit を呼ぶこと（§4.2）。
func (s *Store) UpsertBatch(ctx context.Context, tenant model.TenantID, rows []worker.Row) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// ★ソート（§4.3）。バッチ同士がすれ違う順序で行を触るとデッドロックになる。
	sorted := make([]worker.Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	query, args := buildUpsert(tenant, sorted)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// buildUpsert は §2.7 の SQL を組み立てる。
//
// MySQL の ON DUPLICATE KEY UPDATE には WHERE が書けないので IF で表現する。
// `AS new` の行エイリアスは MySQL 8.0.20 以降（それ以前は VALUES(col)）。
func buildUpsert(tenant model.TenantID, rows []worker.Row) (string, []any) {
	var sb strings.Builder
	sb.WriteString("INSERT INTO robot_state (tenant_id, robot_id, status, online, battery, observed_at, source) VALUES ")
	args := make([]any, 0, len(rows)*7)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?)")
		args = append(args,
			string(tenant), string(r.ID),
			uint8(r.State.Status), r.State.Online, r.State.Battery,
			r.ObservedAt.UTC(), uint8(r.Source),
		)
	}
	sb.WriteString(` AS new ON DUPLICATE KEY UPDATE
  status      = IF(robot_state.observed_at < new.observed_at, new.status,  robot_state.status),
  online      = IF(robot_state.observed_at < new.observed_at, new.online,  robot_state.online),
  battery     = IF(robot_state.observed_at < new.observed_at, new.battery, robot_state.battery),
  source      = IF(robot_state.observed_at < new.observed_at, new.source,  robot_state.source),
  observed_at = GREATEST(robot_state.observed_at, new.observed_at)`)
	return sb.String(), args
}

// TouchBatch は「たまの近況報告」（§4.2）。observed_at だけを進める。
func (s *Store) TouchBatch(ctx context.Context, tenant model.TenantID, rows []worker.Row) error {
	if len(rows) == 0 {
		return nil
	}
	sorted := make([]worker.Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var sb strings.Builder
	sb.WriteString("UPDATE robot_state SET observed_at = GREATEST(observed_at, ?) WHERE tenant_id = ? AND robot_id IN (")
	args := []any{sorted[0].ObservedAt.UTC(), string(tenant)}
	for i, r := range sorted {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("?")
		args = append(args, string(r.ID))
	}
	sb.WriteString(")")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendHistory は変化点だけを履歴表に追記する（§4.4）。
// 「毎秒のサンプル全部」を入れないこと。それだけで最重量の表になる。
func (s *Store) AppendHistory(ctx context.Context, tenant model.TenantID, rows []worker.Row) error {
	if len(rows) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT IGNORE INTO robot_state_history (tenant_id, robot_id, observed_date, observed_at, status, online, battery, source) VALUES ")
	args := make([]any, 0, len(rows)*8)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?,?)")
		at := r.ObservedAt.UTC()
		args = append(args, string(tenant), string(r.ID), at.Format("2006-01-02"), at,
			uint8(r.State.Status), r.State.Online, r.State.Battery, uint8(r.Source))
	}
	_, err := s.db.ExecContext(ctx, sb.String(), args...)
	return err
}

// ---- 比較用（推奨しない書き方）。ベンチマークで差を出すために置いてある ----

// UpdateEachAutocommit は「素直に1件ずつ UPDATE」（§4.1 の一番上の行）。
// autocommit なので、コミットのたびにディスク同期が入る。
func (s *Store) UpdateEachAutocommit(ctx context.Context, tenant model.TenantID, rows []worker.Row) error {
	const q = `UPDATE robot_state SET status=?, online=?, battery=?, observed_at=?, source=?
	           WHERE tenant_id=? AND robot_id=?`
	for _, r := range rows {
		//smlint:allow loopquery 理由: 比較用に置いてある「推奨しない書き方」そのもの（docs/measurements.md §6）
		//smlint:allow rowsaffected 理由: 比較用に置いてある「推奨しない書き方」そのもの（docs/measurements.md §6）
		if _, err := s.db.ExecContext(ctx, q,
			uint8(r.State.Status), r.State.Online, r.State.Battery, r.ObservedAt.UTC(), uint8(r.Source),
			string(tenant), string(r.ID)); err != nil {
			return err
		}
	}
	return nil
}

// UpdateEachInTx は「1件ずつだが1トランザクション」。fsync 削減だけを測るための中間形。
func (s *Store) UpdateEachInTx(ctx context.Context, tenant model.TenantID, rows []worker.Row) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const q = `UPDATE robot_state SET status=?, online=?, battery=?, observed_at=?, source=?
	           WHERE tenant_id=? AND robot_id=?`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		//smlint:allow loopquery 理由: 比較用に置いてある「推奨しない書き方」そのもの（docs/measurements.md §6）
		if _, err := stmt.ExecContext(ctx,
			uint8(r.State.Status), r.State.Online, r.State.Battery, r.ObservedAt.UTC(), uint8(r.Source),
			string(tenant), string(r.ID)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateWithWhereGuard は「SQL 側で弾く」やり方（§4.2 の二番手）。
// 書き込みは避けられても、往復とロックは発生する。メモリで弾けば往復自体が消える。
func (s *Store) UpdateWithWhereGuard(ctx context.Context, tenant model.TenantID, r worker.Row) (int64, error) {
	const q = `UPDATE robot_state SET status=?, online=?, battery=?, observed_at=?, source=?
	           WHERE tenant_id=? AND robot_id=? AND (status<>? OR online<>? OR battery<>?)`
	res, err := s.db.ExecContext(ctx, q,
		uint8(r.State.Status), r.State.Online, r.State.Battery, r.ObservedAt.UTC(), uint8(r.Source),
		string(tenant), string(r.ID),
		uint8(r.State.Status), r.State.Online, r.State.Battery)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- 読み取り ----

// Load は現在状態を読む。§4.5 のとおり、主キーだけで引く。
func (s *Store) Load(ctx context.Context, tenant model.TenantID, id model.ID) (model.Observation, error) {
	const q = `SELECT status, online, battery, observed_at, source FROM robot_state
	           WHERE tenant_id=? AND robot_id=?`
	var (
		st, src uint8
		online  bool
		batt    int8
		at      time.Time
	)
	err := s.db.QueryRowContext(ctx, q, string(tenant), string(id)).Scan(&st, &online, &batt, &at, &src)
	if err != nil {
		return model.Observation{}, err
	}
	return model.Observation{
		State:      model.State{Status: model.Status(st), Online: online, Battery: batt},
		ObservedAt: at,
		Source:     model.Source(src),
	}, nil
}

// IsDeadlock は MySQL のデッドロック（ER_LOCK_DEADLOCK, 1213）を判定する。
// デッドロックは即座に検出され、MySQL が片方のトランザクションを巻き戻す。
func IsDeadlock(err error) bool {
	var me *mysql.MySQLError
	if ok := asMySQLError(err, &me); !ok {
		return false
	}
	return me.Number == 1213
}

// IsLockWaitTimeout はロック待ちタイムアウト（ER_LOCK_WAIT_TIMEOUT, 1205）。
// これはデッドロックではなく「待たされすぎた」で、原因も対処も別物。
func IsLockWaitTimeout(err error) bool {
	var me *mysql.MySQLError
	if ok := asMySQLError(err, &me); !ok {
		return false
	}
	return me.Number == 1205
}

func asMySQLError(err error, target **mysql.MySQLError) bool {
	for err != nil {
		if me, ok := err.(*mysql.MySQLError); ok {
			*target = me
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
