// Package assignlab は EXP-63（テナント割り当てを DB の lease で持つ）の実験本体。
//
// 方針: どのテナントをどの worker が担当するかを DB の1テーブル（1テナント1行）に lease として持つ。
// 各 worker は毎ティック「target = CEIL(全テナント数 / 生存 worker 数)」を計算し、
// target まで claim・超えたら shed するだけ。これで
//   ・平常は均等（動的 claim で 10/10 に収束）
//   ・失敗時は survivor が全責任（live が減り target が上がる＝特別コード不要）
//   ・二重所有は起きない（tenant_id が PK＝行がロック）／fence 単調で古い担当の上書きを弾く
// を満たす。静的ピン（owner を固定）は失敗時に担当テナントが宙に浮くので不可、を対照で測る。
package assignlab

import (
	"context"
	"database/sql"
	"fmt"
)

// Setup は割り当てテーブル（テナントの lease）と worker の心拍テーブルを作る。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS ta_tenant",
		"DROP TABLE IF EXISTS ta_worker",
		`CREATE TABLE ta_tenant (
		   tenant_id     VARCHAR(64) NOT NULL,
		   owner_id      VARCHAR(64) NULL,           -- 今の担当 worker（claim の結果として入る。固定しない）
		   fence         BIGINT      NOT NULL DEFAULT 0,  -- 取り直すたび +1（単調・古い担当の上書きを弾く）
		   lease_expires DATETIME(3) NULL,           -- DB 時計の期限（TTL）
		   pin           VARCHAR(64) NULL,           -- 静的ピンの対照実験でのみ使用
		   PRIMARY KEY (tenant_id)
		 ) ENGINE=InnoDB`,
		`CREATE TABLE ta_worker (
		   worker_id      VARCHAR(64) NOT NULL,
		   last_heartbeat DATETIME(3) NOT NULL,
		   PRIMARY KEY (worker_id)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("assign setup: %w", err)
		}
	}
	return nil
}

// SeedTenants は n 件のテナント行を入れる（owner なし＝未割り当て）。pin!="" ならピン列も埋める。
func SeedTenants(ctx context.Context, db *sql.DB, n int, pinHalf bool, wA, wB string) error {
	//smlint:allow rowsaffected 理由: 実験の seed リセット。全消去で当たり行数は不問
	if _, err := db.ExecContext(ctx, "DELETE FROM ta_tenant"); err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("tenant-%04d", i)
		var pin any
		if pinHalf { // 前半を A、後半を B にピン（対照実験用）
			if i <= n/2 {
				pin = wA
			} else {
				pin = wB
			}
		}
		//smlint:allow loopquery 理由: 実験の seed。固定 n 件を1行ずつ投入（行の N+1 ではない）
		if _, err := db.ExecContext(ctx,
			"INSERT INTO ta_tenant (tenant_id, pin) VALUES (?,?)", id, pin); err != nil {
			return err
		}
	}
	return nil
}

// Heartbeat は worker の心拍を打つ（生存メンバー数の分母になる）。
func Heartbeat(ctx context.Context, db *sql.DB, workerID string) error {
	//smlint:allow rowsaffected 理由: 心拍 upsert。当たり行数は判定に使わない
	_, err := db.ExecContext(ctx,
		"INSERT INTO ta_worker (worker_id,last_heartbeat) VALUES (?,NOW(3)) "+
			"ON DUPLICATE KEY UPDATE last_heartbeat=NOW(3)", workerID)
	return err
}

// LiveWorkers は staleSec 秒以内に心拍のある worker 数（＝生存メンバー数）。
func LiveWorkers(ctx context.Context, db *sql.DB, staleSec int) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM ta_worker WHERE last_heartbeat > NOW(3) - INTERVAL %d SECOND", staleSec)).Scan(&n)
	return n, err
}

// TotalTenants は全テナント数。
func TotalTenants(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ta_tenant").Scan(&n)
	return n, err
}

// HeldCount は「自分が今 有効に担当している」テナント数（owner=me かつ期限内）。
func HeldCount(ctx context.Context, db *sql.DB, workerID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ta_tenant WHERE owner_id=? AND lease_expires>=NOW(3)", workerID).Scan(&n)
	return n, err
}

// RenewLeases は自分が有効に持つ lease の期限を延長する（生きている証）。期限切れは延長しない。
func RenewLeases(ctx context.Context, db *sql.DB, workerID string, ttlSec int) error {
	//smlint:allow rowsaffected 理由: 一括 renew。当たり行数は判定に使わない
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE ta_tenant SET lease_expires=NOW(3)+INTERVAL %d SECOND "+
			"WHERE owner_id=? AND lease_expires>=NOW(3)", ttlSec), workerID)
	return err
}

// Target は fair-share の目標担当数 = CEIL(total / live)。
func Target(total, live int) int {
	if live < 1 {
		live = 1
	}
	return (total + live - 1) / live
}

// ClaimUpTo は空き/期限切れのテナントを最大 need 件 claim する（SKIP LOCKED で取り合い回避・fence+1）。
// onlyPinned=true のときは自分にピンされたテナントだけを対象にする（静的ピンの対照実験用）。
// 返り値は実際に claim できた件数。
func ClaimUpTo(ctx context.Context, db *sql.DB, workerID string, need, ttlSec int, onlyPinned bool) (int, error) {
	if need <= 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	sel := "SELECT tenant_id FROM ta_tenant WHERE (owner_id IS NULL OR lease_expires < NOW(3)) "
	args := []any{}
	if onlyPinned {
		sel += "AND pin=? "
		args = append(args, workerID)
	}
	sel += "ORDER BY tenant_id LIMIT ? FOR UPDATE SKIP LOCKED"
	args = append(args, need)

	//smlint:allow loopquery 理由: 空きプールの列挙（1クエリ）＋各行を CAS で claim。fair-share の本体
	rows, err := tx.QueryContext(ctx, sel, args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	claimed := 0
	for _, id := range ids {
		//smlint:allow loopquery 理由: SKIP LOCKED で確定した id 群を1件ずつ CAS claim（fair-share の本体）
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			"UPDATE ta_tenant SET owner_id=?, fence=fence+1, lease_expires=NOW(3)+INTERVAL %d SECOND "+
				"WHERE tenant_id=? AND (owner_id IS NULL OR lease_expires < NOW(3))", ttlSec), workerID, id)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			claimed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return claimed, nil
}

// ShedDown は持ちすぎ分（excess 件）を返却する（owner を外す＝均等化のダウン側）。返り値は返した件数。
func ShedDown(ctx context.Context, db *sql.DB, workerID string, excess int) (int, error) {
	if excess <= 0 {
		return 0, nil
	}
	//smlint:allow rowsaffected 理由: 余剰の返却。返した行数を返す（判定に使う）
	res, err := db.ExecContext(ctx,
		"UPDATE ta_tenant SET owner_id=NULL, lease_expires=NULL "+
			"WHERE owner_id=? AND lease_expires>=NOW(3) ORDER BY tenant_id DESC LIMIT ?", workerID, excess)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OrphanCount は「有効な担当が居ない」テナント数（owner NULL か 期限切れ）。0 が健全。
func OrphanCount(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ta_tenant WHERE owner_id IS NULL OR lease_expires < NOW(3)").Scan(&n)
	return n, err
}

// Ownership は「有効な lease を持つ owner ごとの担当数」。二重所有は tenant_id が PK なので構造的に不可能。
func Ownership(ctx context.Context, db *sql.DB) (map[string]int, error) {
	//smlint:allow loopquery 理由: 集計の列挙（GROUP BY 1クエリ）。検証用
	rows, err := db.QueryContext(ctx,
		"SELECT owner_id, COUNT(*) FROM ta_tenant WHERE lease_expires>=NOW(3) GROUP BY owner_id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var owner sql.NullString
		var c int
		if err := rows.Scan(&owner, &c); err != nil {
			return nil, err
		}
		if owner.Valid {
			out[owner.String] = c
		}
	}
	return out, rows.Err()
}

// FenceMap は tenant_id -> fence（単調性の検証用）。
func FenceMap(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	//smlint:allow loopquery 理由: fence の列挙（1クエリ）。単調性の検証用
	rows, err := db.QueryContext(ctx, "SELECT tenant_id, fence FROM ta_tenant")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var f int64
		if err := rows.Scan(&id, &f); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}
