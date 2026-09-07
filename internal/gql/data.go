package gql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/repo"
)

// ここが唯一の DB 入口。すべて *repo.Scope（テナント束縛済み）経由なので、
// :tenant が必ず入り、tenant_id を書き忘れた SQL は構造的に書けない（EXP-24）。

func mapStatus(status uint8, online bool) RobotStatus {
	if !online {
		return RobotStatusOffline
	}
	switch model.Status(status) {
	case model.StatusRunning:
		return RobotStatusRunning
	case model.StatusStopped:
		return RobotStatusIdle
	case model.StatusError:
		return RobotStatusError
	default:
		return RobotStatusUnknown
	}
}

// listRobots は keyset 一覧（robot_id > after を id 順に LIMIT）。first はサーバ側で上限済み。
func listRobots(ctx context.Context, sc *repo.Scope, after string, first int) ([]Robot, error) {
	const q = `SELECT robot_id, status, online, battery
	             FROM robot_state
	            WHERE tenant_id = :tenant AND robot_id > ?
	         ORDER BY robot_id
	            LIMIT ?`
	rows, err := sc.Query(ctx, "gql.robots", q, after, first)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Robot
	for rows.Next() {
		var (
			id      string
			status  uint8
			online  bool
			battery int
		)
		if err := rows.Scan(&id, &status, &online, &battery); err != nil {
			return nil, err
		}
		out = append(out, Robot{ID: id, Status: mapStatus(status, online), Battery: battery, Online: online})
	}
	return out, rows.Err()
}

// getRobot は ID 1件。見つからなければ (nil, nil)。テナント外の id は :tenant で弾かれ、見つからない。
func getRobot(ctx context.Context, sc *repo.Scope, id string) (*Robot, error) {
	const q = `SELECT robot_id, status, online, battery
	             FROM robot_state
	            WHERE tenant_id = :tenant AND robot_id = ?`
	var (
		rid     string
		status  uint8
		online  bool
		battery int
	)
	err := sc.QueryRow(ctx, "gql.robot", q, id).Scan(&rid, &status, &online, &battery)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Robot{ID: rid, Status: mapStatus(status, online), Battery: battery, Online: online}, nil
}

// nameOf は robot_profile（別表）から name を1件。commands と同様、要求時だけ引く（射影）。
func nameOf(ctx context.Context, sc *repo.Scope, robotID string) (string, error) {
	const q = `SELECT name FROM robot_profile WHERE tenant_id = :tenant AND robot_id = ?`
	var name string
	err := sc.QueryRow(ctx, "gql.robot.name", q, robotID).Scan(&name)
	if errors.Is(err, repo.ErrNotFound) {
		return "", nil
	}
	return name, err
}

// commandsForRobot は1台ぶんの命令（素朴版・N+1 の温床）。
func commandsForRobot(ctx context.Context, sc *repo.Scope, robotID string, first int) ([]Command, error) {
	const q = `SELECT command_id, type, state, scheduled_for
	             FROM cmd_command
	            WHERE tenant_id = :tenant AND robot_id = ?
	         ORDER BY scheduled_for DESC
	            LIMIT ?`
	rows, err := sc.Query(ctx, "gql.commands.one", q, robotID, first)
	if err != nil {
		return nil, err
	}
	return scanCommands(rows)
}

// commandsForRobots は複数台ぶんを1クエリでまとめて引く（DataLoader のバッチ本体・EXP-23）。
// robot_id ごとに first 件に切って返す。
func commandsForRobots(ctx context.Context, sc *repo.Scope, robotIDs []string, first int) (map[string][]Command, error) {
	out := make(map[string][]Command, len(robotIDs))
	if len(robotIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(robotIDs)), ",")
	// LIMIT は必須（無制限 SELECT は repo が弾く）。台数×first で頭打ちにする。
	q := `SELECT command_id, type, state, scheduled_for, robot_id
	        FROM cmd_command
	       WHERE tenant_id = :tenant AND robot_id IN (` + ph + `)
	    ORDER BY robot_id, scheduled_for DESC
	       LIMIT ?`
	args := make([]any, 0, len(robotIDs)+1)
	for _, id := range robotIDs {
		args = append(args, id)
	}
	args = append(args, len(robotIDs)*first)
	rows, err := sc.Query(ctx, "gql.commands.batch", q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c       Command
			robotID string
		)
		if err := rows.Scan(&c.ID, &c.Type, &c.State, &c.ScheduledFor, &robotID); err != nil {
			return nil, err
		}
		if len(out[robotID]) < first { // robot ごとに first 件で切る
			out[robotID] = append(out[robotID], c)
		}
	}
	return out, rows.Err()
}

func scanCommands(rows *sql.Rows) ([]Command, error) {
	defer func() { _ = rows.Close() }()
	var out []Command
	for rows.Next() {
		var c Command
		var sched time.Time
		if err := rows.Scan(&c.ID, &c.Type, &c.State, &sched); err != nil {
			return nil, err
		}
		c.ScheduledFor = sched
		out = append(out, c)
	}
	return out, rows.Err()
}
