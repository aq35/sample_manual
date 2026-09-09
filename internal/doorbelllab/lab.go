// Package doorbelllab は EXP-62（イベント＝doorbell、正しさは DB CAS）の実験本体。
//
// 方針: SQS/EventBridge のようなイベントは「起こす・配る・周期」に使い（doorbell）、
// 「決める（完了・lease・fence）」は DB CAS のまま。イベントは floor（poll/reconcile）の上に載せる
// 遅延最適化であって置換ではない。ここでは DB を使う②完了 exactly-once と③lease 失効回収を測る。
package doorbelllab

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Setup は goal 表を作る（完了フラグ・完了回数・lease）。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS wl_goal",
		`CREATE TABLE wl_goal (
		   id             BIGINT       NOT NULL,
		   tenant_id      VARCHAR(32)  NOT NULL,
		   done           TINYINT      NOT NULL DEFAULT 0,
		   complete_count INT          NOT NULL DEFAULT 0,
		   lease_owner    VARCHAR(64)  NULL,
		   lease_expires  DATETIME(3)  NULL,
		   PRIMARY KEY (id)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("doorbell setup: %w", err)
		}
	}
	return nil
}

// Seed は goal を1件入れる（done=0）。
func Seed(ctx context.Context, db *sql.DB, id int64, tenant string) error {
	//smlint:allow rowsaffected 理由: 実験の seed。1行 INSERT
	_, err := db.ExecContext(ctx, "INSERT INTO wl_goal (id, tenant_id) VALUES (?,?)", id, tenant)
	return err
}

// CompleteCAS は「まだ done でなければ done にする」完了を CAS で行う（正しい）。
// 返り値 true は「今回自分が完了させた」。重複ドアベルで何度呼ばれても、成功は1回だけ。
func CompleteCAS(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		"UPDATE wl_goal SET done=1, complete_count=complete_count+1 WHERE id=? AND done=0", id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CompleteNaive は「読んでから書く」を分けた素朴版（並行で二重完了しうる・事故）。
func CompleteNaive(ctx context.Context, db *sql.DB, id int64) error {
	var done int
	if err := db.QueryRowContext(ctx, "SELECT done FROM wl_goal WHERE id=?", id).Scan(&done); err != nil {
		return err
	}
	if done == 0 {
		time.Sleep(15 * time.Millisecond) // 読みと書きの隙間（並行だと複数がここを通る）
		//smlint:allow rowsaffected 理由: 事故を再現する素朴 UPDATE。当たる行数は見ない
		if _, err := db.ExecContext(ctx,
			"UPDATE wl_goal SET done=1, complete_count=complete_count+1 WHERE id=?", id); err != nil {
			return err
		}
	}
	return nil
}

// CompleteCount は完了回数（exactly-once なら 1）。
func CompleteCount(ctx context.Context, db *sql.DB, id int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT complete_count FROM wl_goal WHERE id=?", id).Scan(&n)
	return n, err
}

// ClaimLease は DB 時計で lease を取る（空 or 失効しているときだけ・CAS）。true=取れた。
func ClaimLease(ctx context.Context, db *sql.DB, id int64, owner string, ttl time.Duration) (bool, error) {
	sec := int(ttl / time.Second)
	if sec < 1 {
		sec = 1
	}
	//smlint:allow rowsaffected 理由: CAS。当たれば1行=取得成功、0行=取れず（判定に使う）
	res, err := db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE wl_goal SET lease_owner=?, lease_expires=DATE_ADD(NOW(3), INTERVAL %d SECOND) "+
			"WHERE id=? AND done=0 AND (lease_owner IS NULL OR lease_expires < NOW(3))", sec),
		owner, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReconcileExpired は「done でなく、lease が空 or DB 時計で失効している」goal の id を返す。
// crash した worker の担当は「イベントが来ない」ので、この sweep でしか回収できない。
func ReconcileExpired(ctx context.Context, db *sql.DB) ([]int64, error) {
	//smlint:allow loopquery 理由: reconcile sweep 本体。1クエリで候補を列挙する
	rows, err := db.QueryContext(ctx,
		"SELECT id FROM wl_goal WHERE done=0 AND (lease_owner IS NULL OR lease_expires < NOW(3)) LIMIT 1000")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
