// Package gqllab は EXP-23/24（gqlgen GraphQL のパフォーマンスとセキュリティ）の土台。
//
// robot_profile（別表・名前）/ robot_state（状態）/ cmd_command（命令）を用意し、
// 2テナントぶんシードする。GraphQL は internal/gql のサーバをそのまま使う。
package gqllab

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/repo"
)

// robot_state と cmd_command は repo.Migrate の対象外なので、ここで用意する（べき等）。
const extraSchema = `
CREATE TABLE IF NOT EXISTS robot_state (
  tenant_id   VARCHAR(32)      NOT NULL,
  robot_id    VARCHAR(32)      NOT NULL,
  status      TINYINT UNSIGNED NOT NULL,
  online      TINYINT(1)       NOT NULL,
  battery     TINYINT          NOT NULL,
  observed_at DATETIME(3)      NOT NULL,
  source      TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS cmd_command (
  tenant_id     VARCHAR(32)  NOT NULL,
  command_id    VARCHAR(40)  NOT NULL,
  robot_id      VARCHAR(32)  NOT NULL,
  type          VARCHAR(32)  NOT NULL,
  payload       VARCHAR(255) NOT NULL DEFAULT '',
  state         ENUM('pending','dispatched','acked','done','failed','unknown') NOT NULL DEFAULT 'pending',
  idem_key      VARCHAR(64)  NOT NULL,
  fence         BIGINT       NOT NULL DEFAULT 0,
  scheduled_for DATETIME(3)  NOT NULL,
  dispatched_at DATETIME(3)  NULL,
  attempts      INT          NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, command_id),
  KEY by_robot (tenant_id, robot_id, scheduled_for),
  UNIQUE KEY uq_idem (tenant_id, idem_key)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS robot_operator (
  tenant_id VARCHAR(32) NOT NULL,
  operator  VARCHAR(64) NOT NULL,
  robot_id  VARCHAR(32) NOT NULL,
  PRIMARY KEY (tenant_id, operator, robot_id)
) ENGINE=InnoDB;`

// Setup は robot_profile（repo.Migrate）と robot_state / cmd_command を用意する。
func Setup(ctx context.Context, db *repo.DB) error {
	if err := repo.Migrate(ctx, db); err != nil {
		return err
	}
	for _, stmt := range strings.Split(extraSchema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.SQL().ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("gqllab schema: %w", err)
		}
	}
	return nil
}

// Seed は tenant に robots 台、各ロボットに cmdsPer 件の命令を入れる。
func Seed(ctx context.Context, db *repo.DB, tenant string, robots, cmdsPer int) error {
	sqldb := db.SQL()
	for _, t := range []string{"robot_state", "robot_profile", "cmd_command", "robot_operator"} {
		if _, err := sqldb.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id=?", tenant); err != nil {
			return err
		}
	}
	now := time.Now()
	// robot_state + robot_profile
	for start := 0; start < robots; start += 500 {
		end := min(start+500, robots)
		var st, pr strings.Builder
		st.WriteString("INSERT INTO robot_state (tenant_id, robot_id, status, online, battery, observed_at, source) VALUES ")
		pr.WriteString("INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial, version, updated_at) VALUES ")
		var sa, pa []any
		for i := start; i < end; i++ {
			rid := fmt.Sprintf("r%04d", i)
			if i > start {
				st.WriteString(",")
				pr.WriteString(",")
			}
			st.WriteString("(?,?,?,?,?,?,?)")
			sa = append(sa, tenant, rid, i%4, i%2, 50+i%50, now, 1)
			pr.WriteString("(?,?,?,?,?,?,?)")
			pa = append(pa, tenant, rid, fmt.Sprintf("ロボット%d", i), "AGV-3000", fmt.Sprintf("%s-SN%04d", tenant, i), 1, now)
		}
		//smlint:allow loopquery 理由: シード。500行/文の一括投入をチャンクごとに流すだけ
		if _, err := sqldb.ExecContext(ctx, st.String(), sa...); err != nil {
			return err
		}
		//smlint:allow loopquery 理由: シード。500行/文の一括投入をチャンクごとに流すだけ
		if _, err := sqldb.ExecContext(ctx, pr.String(), pa...); err != nil {
			return err
		}
	}
	// cmd_command（cmdsPer が 0 なら命令は入れない）
	for i := 0; cmdsPer > 0 && i < robots; i++ {
		rid := fmt.Sprintf("r%04d", i)
		var b strings.Builder
		b.WriteString("INSERT INTO cmd_command (tenant_id, command_id, robot_id, type, state, idem_key, scheduled_for) VALUES ")
		var args []any
		for j := 0; j < cmdsPer; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			cid := fmt.Sprintf("%s-c%04d-%03d", tenant, i, j)
			b.WriteString("(?,?,?,?,?,?,?)")
			args = append(args, tenant, cid, rid, "move", "pending", cid, now.Add(-time.Duration(j)*time.Minute))
		}
		//smlint:allow loopquery 理由: シード。ロボット1台ぶんの命令を1文でまとめて投入
		if _, err := sqldb.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// GrantOperator は operator に robot_id の操作権限を1つ与える（行レベル認可の grant）。
func GrantOperator(ctx context.Context, db *repo.DB, tenant, operator, robotID string) error {
	_, err := db.SQL().ExecContext(ctx,
		"INSERT IGNORE INTO robot_operator (tenant_id, operator, robot_id) VALUES (?,?,?)",
		tenant, operator, robotID)
	return err
}

// CountCommandsByIdem は冪等キーで作られた命令の行数（冪等性テスト用）。
func CountCommandsByIdem(ctx context.Context, db *repo.DB, tenant, idemKey string) (int, error) {
	var n int
	err := db.SQL().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cmd_command WHERE tenant_id=? AND idem_key=?", tenant, idemKey).Scan(&n)
	return n, err
}

// SeedOne は1台だけ足す（テナント分離テストで「B だけに在る id」を作る用）。
func SeedOne(ctx context.Context, db *repo.DB, tenant, robotID string) error {
	sqldb := db.SQL()
	now := time.Now()
	if _, err := sqldb.ExecContext(ctx,
		"INSERT INTO robot_state (tenant_id, robot_id, status, online, battery, observed_at, source) VALUES (?,?,?,?,?,?,?)",
		tenant, robotID, 1, 1, 80, now, 1); err != nil {
		return err
	}
	_, err := sqldb.ExecContext(ctx,
		"INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial, version, updated_at) VALUES (?,?,?,?,?,?,?)",
		tenant, robotID, "只一台", "AGV-3000", tenant+"-"+robotID, 1, now)
	return err
}
