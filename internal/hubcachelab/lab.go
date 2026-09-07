// Package hubcachelab は EXP-52（hub のキャッシュ破棄の粒度）の実験本体。
//
// 1つの hub（1テナント）に複数ロボットがいる。1台の状態が変わったとき、キャッシュ（snapshot）を
// どう破棄・再取得するか。「テナント丸ごと引き直す」と台数 N に比例して読む。「版(ver)で差分だけ
// 引く」と変更数にしか比例しない。DB が読む行数（＝IO・メモリ）で両者を比べる。
package hubcachelab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// PayloadWidth は1ロボットの状態 payload のバイト幅（読み込む行の重さの目安）。
const PayloadWidth = 200

// Setup は hub_state 表を作る。(tenant_id, robot_id) が PK、(tenant_id, ver) に索引。
func Setup(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		"DROP TABLE IF EXISTS hub_state",
		fmt.Sprintf(`CREATE TABLE hub_state (
		   tenant_id VARCHAR(32)  NOT NULL,
		   robot_id  BIGINT       NOT NULL,
		   ver       BIGINT       NOT NULL,
		   payload   VARCHAR(255) NOT NULL,
		   PRIMARY KEY (tenant_id, robot_id),
		   KEY k_ver (tenant_id, ver)
		 ) ENGINE=InnoDB`),
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("hubcache setup: %w", err)
		}
	}
	return nil
}

// Seed は tenant に n 台を入れ、ver=1..n を振る。返り値は最大 ver。
func Seed(ctx context.Context, db *sql.DB, tenant string, n int) (int64, error) {
	pad := strings.Repeat("x", PayloadWidth)
	const chunk = 500
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		var b strings.Builder
		b.WriteString("INSERT INTO hub_state (tenant_id, robot_id, ver, payload) VALUES ")
		var args []any
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			b.WriteString("(?,?,?,?)")
			args = append(args, tenant, i, i+1, pad)
		}
		//smlint:allow loopquery 理由: 実験 seed。500 件ずつまとめた bulk INSERT をチャンクで回している
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			return 0, err
		}
	}
	return int64(n), nil
}

// BumpRobots は robotIDs の ver を startVer+1, +2, ... へ上げる（＝状態が変わった合図）。
// 返り値は更新後の最大 ver。
func BumpRobots(ctx context.Context, db *sql.DB, tenant string, robotIDs []int, startVer int64) (int64, error) {
	v := startVer
	for _, id := range robotIDs {
		v++
		//smlint:allow rowsaffected 理由: 1台ぶんの版更新。当たるのは常に1行（PK 指定）
		//smlint:allow loopquery 理由: 「c 台が変わった」を再現するための状態遷移。まとめられない
		if _, err := db.ExecContext(ctx, "UPDATE hub_state SET ver=? WHERE tenant_id=? AND robot_id=?", v, tenant, id); err != nil {
			return 0, err
		}
	}
	return v, nil
}

// WholeSnapshot はテナント丸ごと引き直す（粗い破棄）。返り値は読んだ行数。
func WholeSnapshot(ctx context.Context, db *sql.DB, tenant string) (int, error) {
	//smlint:allow loopquery 理由: これ自体が「1回の poll」。行はループで読むだけ（N+1 ではない）
	rows, err := db.QueryContext(ctx, "SELECT robot_id, ver, payload FROM hub_state WHERE tenant_id=? LIMIT 100000", tenant)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id, ver int64
		var payload string
		if err := rows.Scan(&id, &ver, &payload); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

// DeltaSince は ver > lastVer の行だけ引く（版で差分だけ・細かい破棄）。
// 返り値は読んだ行数と、読んだ中の最大 ver（次回の lastVer）。
func DeltaSince(ctx context.Context, db *sql.DB, tenant string, lastVer int64) (int, int64, error) {
	//smlint:allow loopquery 理由: これ自体が「1回の poll」。索引レンジで差分行だけ読む
	rows, err := db.QueryContext(ctx,
		"SELECT robot_id, ver, payload FROM hub_state WHERE tenant_id=? AND ver>? ORDER BY ver LIMIT 100000", tenant, lastVer)
	if err != nil {
		return 0, lastVer, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	maxVer := lastVer
	for rows.Next() {
		var id, ver int64
		var payload string
		if err := rows.Scan(&id, &ver, &payload); err != nil {
			return 0, lastVer, err
		}
		n++
		if ver > maxVer {
			maxVer = ver
		}
	}
	return n, maxVer, rows.Err()
}
