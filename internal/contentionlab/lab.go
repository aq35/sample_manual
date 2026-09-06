// Package contentionlab は EXP-15（テーブル分割・パーティションと UPDATE 競合）の実験本体。
//
// 問い: テーブルをどう分割すると、テーブルへの競合が減るか・増えるか。
//   - 幅広い行（熱い状態と大きなペイロードを同居）vs 狭い行（熱い状態だけ分ける、§4.4）
//   - 履歴の掃除: DELETE vs DROP PARTITION
package contentionlab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

//go:embed schema.sql
var schemaSQL string

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定の DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("contention schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// LockStats は InnoDB の行ロック待ちの状態（前後で差を取る）。
type LockStats struct {
	RowLockWaits int64
	RowLockTime  int64 // ms
}

func Locks(ctx context.Context, db *sql.DB) LockStats {
	var s LockStats
	read := func(name string) int64 {
		var k string
		var v int64
		// ★プレースホルダを使わず名前を直接埋める（name は固定の定数）。
		// 最初 SHOW GLOBAL STATUS LIKE ? としていたら Scan が滑って常に 0 を返し、
		// 行ロック待ちが「0」に見えていた（測定器のバグ。グローバル値は増えていた）。
		//smlint:allow loopquery 理由: 測定条件の採取。固定の状態変数を読むだけ
		if err := db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(&k, &v); err != nil {
			return 0
		}
		return v
	}
	s.RowLockWaits = read("Innodb_row_lock_waits")
	s.RowLockTime = read("Innodb_row_lock_time")
	return s
}

// UpdateResult は更新負荷の結果。
type UpdateResult struct {
	Ops       int64
	Deadlocks int64
	Errs      int64
	Latency   expkit.LatencyStats
	LockWaits int64 // この負荷の間に増えた行ロック待ち回数
	LockTime  int64 // 増えた行ロック待ち時間(ms)
	Elapsed   time.Duration
}

// SeedState は wide/narrow に同じ robots を入れる。
func SeedState(ctx context.Context, db *sql.DB, tenant string, robots int, payload string) error {
	for _, t := range []string{"wide_state", "narrow_state", "narrow_payload"} {
		//smlint:allow loopquery 理由: 実験前の後片付け。固定3表
		//smlint:allow rowsaffected 理由: 後片付け
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	for i := 0; i < robots; i++ {
		rid := fmt.Sprintf("r%05d", i)
		//smlint:allow loopquery 理由: 実験データ投入
		//smlint:allow rowsaffected 理由: 投入
		if _, err := db.ExecContext(ctx,
			"INSERT INTO wide_state (tenant_id, robot_id, status, battery, payload) VALUES (?,?,0,50,?)",
			tenant, rid, payload); err != nil {
			return err
		}
		//smlint:allow loopquery 理由: 実験データ投入
		//smlint:allow rowsaffected 理由: 投入
		if _, err := db.ExecContext(ctx,
			"INSERT INTO narrow_state (tenant_id, robot_id, status, battery) VALUES (?,?,0,50)",
			tenant, rid); err != nil {
			return err
		}
		//smlint:allow loopquery 理由: 実験データ投入
		//smlint:allow rowsaffected 理由: 投入
		if _, err := db.ExecContext(ctx,
			"INSERT INTO narrow_payload (tenant_id, robot_id, payload) VALUES (?,?,?)",
			tenant, rid, payload); err != nil {
			return err
		}
	}
	return nil
}

// RunUpdate は「熱い robots を多数のワーカーが同時に UPDATE する」負荷。
// table は "wide_state" か "narrow_state"。newPayload!="" のとき wide は payload も書く。
func RunUpdate(ctx context.Context, db *sql.DB, tenant, table string, robots, concurrency int, dur time.Duration, writePayload bool) UpdateResult {
	lat := expkit.NewLatency()
	var ops, deadlocks, errs atomic.Int64
	before := Locks(ctx, db)

	var query string
	switch table {
	case "wide_state":
		if writePayload {
			query = "UPDATE wide_state SET status=?, battery=?, payload=? WHERE tenant_id=? AND robot_id=?"
		} else {
			query = "UPDATE wide_state SET status=?, battery=? WHERE tenant_id=? AND robot_id=?"
		}
	default:
		query = "UPDATE narrow_state SET status=?, battery=? WHERE tenant_id=? AND robot_id=?"
	}

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	pay := strings.Repeat("x", 1500)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := w
			for time.Now().Before(deadline) && ctx.Err() == nil {
				i += concurrency
				rid := fmt.Sprintf("r%05d", i%robots) // 少数の熱い行を奪い合う
				t0 := time.Now()
				var err error
				if table == "wide_state" && writePayload {
					//smlint:allow loopquery 理由: 更新負荷の生成。ループで打ち続けるのが実験の条件
					//smlint:allow rowsaffected 理由: スループットと行ロックを測る。行数は見ない
					_, err = db.ExecContext(ctx, query, i%5, i%100, pay, tenant, rid)
				} else {
					//smlint:allow loopquery 理由: 更新負荷の生成。ループで打ち続けるのが実験の条件
					//smlint:allow rowsaffected 理由: スループットと行ロックを測る。行数は見ない
					_, err = db.ExecContext(ctx, query, i%5, i%100, tenant, rid)
				}
				switch {
				case err == nil:
					lat.Record(time.Since(t0))
					ops.Add(1)
				case isDeadlock(err):
					deadlocks.Add(1)
				default:
					errs.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	after := Locks(ctx, db)
	return UpdateResult{
		Ops: ops.Load(), Deadlocks: deadlocks.Load(), Errs: errs.Load(),
		Latency: lat.Stats(), Elapsed: time.Since(start),
		LockWaits: after.RowLockWaits - before.RowLockWaits,
		LockTime:  after.RowLockTime - before.RowLockTime,
	}
}

// RunUpdateContended は「トランザクションで行ロックを hold だけ保持してから COMMIT」する負荷。
// 少数の熱い行を奪い合わせて、行ロック待ち（Innodb_row_lock_waits）を実際に観測する。
// table 分割で行ロック競合が減るか（減らないはず）を見るための測定器。
func RunUpdateContended(ctx context.Context, db *sql.DB, tenant, table string, robots, concurrency int, dur, hold time.Duration) UpdateResult {
	lat := expkit.NewLatency()
	var ops, deadlocks, errs atomic.Int64
	before := Locks(ctx, db)
	col := "narrow_state"
	if table == "wide_state" {
		col = "wide_state"
	}
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := w
			for time.Now().Before(deadline) && ctx.Err() == nil {
				i += concurrency
				rid := fmt.Sprintf("r%05d", i%robots)
				t0 := time.Now()
				err := func() error {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					//smlint:allow rowsaffected 理由: 競合測定。行ロックを取ることが目的
					//smlint:allow loopquery 理由: 競合負荷の生成。ループで打ち続けるのが実験の条件
					if _, err := tx.ExecContext(ctx,
						"UPDATE "+col+" SET status=?, battery=? WHERE tenant_id=? AND robot_id=?",
						i%5, i%100, tenant, rid); err != nil {
						return err
					}
					time.Sleep(hold) // ロックを保持する（他ワーカーを待たせる）
					return tx.Commit()
				}()
				switch {
				case err == nil:
					lat.Record(time.Since(t0))
					ops.Add(1)
				case isDeadlock(err):
					deadlocks.Add(1)
				default:
					errs.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	after := Locks(ctx, db)
	return UpdateResult{
		Ops: ops.Load(), Deadlocks: deadlocks.Load(), Errs: errs.Load(),
		Latency: lat.Stats(), Elapsed: time.Since(start),
		LockWaits: after.RowLockWaits - before.RowLockWaits,
		LockTime:  after.RowLockTime - before.RowLockTime,
	}
}

func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadlock") || strings.Contains(s, "1213") ||
		strings.Contains(s, "lock wait timeout") || strings.Contains(s, "1205")
}

// CleanupResult は履歴の掃除の結果。
type CleanupResult struct {
	Method  string
	Removed int64
	Took    time.Duration
}

// SeedHistory は hist_part / hist_plain に、日付をまたいで rows 件入れる。
// histPartDDL は hist_part を毎回まっさらに作り直すための DDL（p1 が前回 DROP されていても復活する）。
const histPartDDL = `CREATE TABLE hist_part (
  tenant_id     VARCHAR(32) NOT NULL,
  robot_id      VARCHAR(32) NOT NULL,
  observed_date DATE        NOT NULL,
  observed_at   DATETIME(3) NOT NULL,
  status        TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, robot_id, observed_date, observed_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(observed_date) (
  PARTITION p1 VALUES LESS THAN ('2026-01-02'),
  PARTITION p2 VALUES LESS THAN ('2026-01-03'),
  PARTITION p3 VALUES LESS THAN ('2026-01-04'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE))`

func SeedHistory(ctx context.Context, db *sql.DB, tenant string, rows int) error {
	// ★hist_part は毎回作り直す（前回 DROP PARTITION した p1 を復活させるため）。
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS hist_part"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, histPartDDL); err != nil {
		return err
	}
	//smlint:allow rowsaffected 理由: 後片付け
	if _, err := db.ExecContext(ctx, "DELETE FROM hist_plain WHERE tenant_id = ?", tenant); err != nil {
		return err
	}
	dates := []string{"2026-01-01", "2026-01-02", "2026-01-03"}
	for _, tbl := range []string{"hist_part", "hist_plain"} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx,
			"INSERT INTO "+tbl+" (tenant_id, robot_id, observed_date, observed_at, status) VALUES (?,?,?,?,0)")
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for i := 0; i < rows; i++ {
			d := dates[i%len(dates)]
			//smlint:allow loopquery 理由: 実験データ投入
			//smlint:allow rowsaffected 理由: 投入
			if _, err := stmt.ExecContext(ctx, tenant, fmt.Sprintf("r%05d", i),
				d, d+" 00:00:00.000"); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// CleanupDelete は「古い日付を DELETE で消す」（undo・purge が高くつく）。
func CleanupDelete(ctx context.Context, db *sql.DB, tenant, cutoff string) (CleanupResult, error) {
	start := time.Now()
	res, err := db.ExecContext(ctx,
		"DELETE FROM hist_plain WHERE tenant_id=? AND observed_date < ?", tenant, cutoff)
	if err != nil {
		return CleanupResult{}, err
	}
	n, _ := res.RowsAffected()
	return CleanupResult{Method: "DELETE", Removed: n, Took: time.Since(start)}, nil
}

// CleanupDropPartition は「古いパーティションを DROP で丸ごと外す」（ほぼ一瞬）。
func CleanupDropPartition(ctx context.Context, db *sql.DB, part string) (CleanupResult, error) {
	// 件数を先に数える（比較のため）
	var n int64
	//smlint:allow rowsaffected 理由: 件数取得のための SELECT
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hist_part PARTITION ("+part+")").Scan(&n)
	start := time.Now()
	//smlint:allow rowsaffected 理由: DDL。影響行数の概念がない
	if _, err := db.ExecContext(ctx, "ALTER TABLE hist_part DROP PARTITION "+part); err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{Method: "DROP PARTITION", Removed: n, Took: time.Since(start)}, nil
}
