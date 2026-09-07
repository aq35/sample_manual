// Package dlqlab は EXP-45（poison / dead-letter）の実験本体。
//
// 必ず失敗する命令（poison）を無限リトライすると、キューが永遠に drain せず、リトライが延々 DB と
// 外部を叩き続ける。試行上限を決め、超えたら dead（隔離）へ落としてアラートする。良い命令は流れ続ける。
package dlqlab

import (
	"context"
	"database/sql"
	"strings"
)

func Setup(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS dlq_cmd",
		`CREATE TABLE dlq_cmd (
		   tenant_id VARCHAR(32) NOT NULL,
		   id        BIGINT      NOT NULL,
		   state     ENUM('pending','done','dead') NOT NULL DEFAULT 'pending',
		   attempts  INT         NOT NULL DEFAULT 0,
		   PRIMARY KEY (tenant_id, id),
		   KEY pend (tenant_id, state, id)
		 ) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Seed は n 件 pending を入れる（poison かどうかは処理側の isPoison で判定）。
func Seed(ctx context.Context, db *sql.DB, tenant string, n int) error {
	//smlint:allow rowsaffected 理由: シードの後片付け。件数は使わない
	if _, err := db.ExecContext(ctx, "DELETE FROM dlq_cmd WHERE tenant_id=?", tenant); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("INSERT INTO dlq_cmd (tenant_id, id) VALUES ")
	var args []any
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?)")
		args = append(args, tenant, i)
	}
	_, err := db.ExecContext(ctx, b.String(), args...)
	return err
}

// Process は pending を最大 maxPasses 周ぶん処理する。
//   - 良い命令は成功 → done。
//   - poison は失敗 → attempts+1。useDLQ かつ attempts が maxAttempts に達したら dead（隔離）。
//   - useDLQ=false だと poison は pending のまま残り、周回のたびに再試行され続ける（無限リトライ）。
//
// 失敗しても後続は処理する（先頭で止めない）ので、良い命令は poison に関係なく流れる。
func Process(ctx context.Context, db *sql.DB, tenant string, isPoison func(int64) bool,
	maxAttempts int, useDLQ bool, maxPasses int) error {
	useDLQi := 0
	if useDLQ {
		useDLQi = 1
	}
	for pass := 0; pass < maxPasses; pass++ {
		//smlint:allow loopquery 理由: 周ごとに未処理を引き直す（キュー処理そのもの・実験対象）
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM dlq_cmd WHERE tenant_id=? AND state='pending' ORDER BY id", tenant)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil // drain 完了
		}
		for _, id := range ids {
			if isPoison(id) {
				// 失敗: 試行を増やし、上限に達したら dead（useDLQ のときだけ）
				//smlint:allow loopquery 理由: 命令ごとの状態遷移（処理そのもの・実験対象）
				//smlint:allow rowsaffected 理由: 遷移の件数は使わない
				// ★state を attempts より先に書く。MySQL は SET を左→右で評価し、後の式は更新後の値を
				// 見る（代入順序の罠・調査の ON DUPLICATE KEY と同じ）。state を先にして「増やす前の
				// attempts+1」で判定する。
				if _, err := db.ExecContext(ctx,
					`UPDATE dlq_cmd
					    SET state = IF(? = 1 AND attempts + 1 >= ?, 'dead', 'pending'),
					        attempts = attempts + 1
					  WHERE tenant_id=? AND id=?`,
					useDLQi, maxAttempts, tenant, id); err != nil {
					return err
				}
				continue
			}
			//smlint:allow loopquery 理由: 命令ごとの状態遷移（処理そのもの・実験対象）
			//smlint:allow rowsaffected 理由: 遷移の件数は使わない
			if _, err := db.ExecContext(ctx,
				"UPDATE dlq_cmd SET state='done' WHERE tenant_id=? AND id=?", tenant, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stats は状態別の件数と、poison（id）の試行回数を返す。
func Stats(ctx context.Context, db *sql.DB, tenant string, poisonID int64) (pending, done, dead, poisonAttempts int, err error) {
	q := func(state string) (int, error) {
		var n int
		e := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM dlq_cmd WHERE tenant_id=? AND state=?", tenant, state).Scan(&n)
		return n, e
	}
	if pending, err = q("pending"); err != nil {
		return
	}
	if done, err = q("done"); err != nil {
		return
	}
	if dead, err = q("dead"); err != nil {
		return
	}
	err = db.QueryRowContext(ctx,
		"SELECT attempts FROM dlq_cmd WHERE tenant_id=? AND id=?", tenant, poisonID).Scan(&poisonAttempts)
	return
}
