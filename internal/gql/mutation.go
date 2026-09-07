package gql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/repo"
)

// 許可する命令種別（入力検証・EXP-27）。未知の型は受け付けない。
var allowedCommandTypes = map[string]struct{}{
	"move": {}, "stop": {}, "charge": {}, "reset": {},
}

const maxPayloadLen = 255

// validateSendCommand は入力を検証し、不正なら「クライアント起因」の明確なエラーを返す
// （内部エラーとして 500 にしない）。DB に触れる前に弾く。
func validateSendCommand(in SendCommandInput) error {
	if strings.TrimSpace(in.RobotID) == "" {
		return userErr("invalid input: robotId は必須")
	}
	if _, ok := allowedCommandTypes[in.Type]; !ok {
		return userErr("invalid input: type=%q は不正（move/stop/charge/reset のいずれか）", in.Type)
	}
	if in.Payload != nil && len(*in.Payload) > maxPayloadLen {
		return userErr("invalid input: payload は %d 文字以内", maxPayloadLen)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return userErr("invalid input: idempotencyKey は必須（二重発行を防ぐため）")
	}
	return nil
}

// canOperate は行レベル認可（EXP-28）: この操作主体が、この robot を操作してよいか。
// robot_operator に (tenant, operator, robot) の許可行があるかを見る。ロール（@auth）が
// 「どのフィールドか」なのに対し、これは「どの行か」。対象と主体に依るのでリゾルバで判定する。
func canOperate(ctx context.Context, sc *repo.Scope, operator, robotID string) (bool, error) {
	const q = `SELECT 1 FROM robot_operator
	            WHERE tenant_id = :tenant AND operator = ? AND robot_id = ? LIMIT 1`
	var one int
	err := sc.QueryRow(ctx, "gql.canOperate", q, operator, robotID).Scan(&one)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// findCommandByIdem は冪等キーで既存の命令を探す（あれば返す＝再送は同じものを返す）。
func findCommandByIdem(ctx context.Context, sc *repo.Scope, idemKey string) (*Command, error) {
	const q = `SELECT command_id, type, state, scheduled_for
	             FROM cmd_command WHERE tenant_id = :tenant AND idem_key = ? LIMIT 1`
	var c Command
	var sched time.Time
	err := sc.QueryRow(ctx, "gql.findByIdem", q, idemKey).Scan(&c.ID, &c.Type, &c.State, &sched)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ScheduledFor = sched
	return &c, nil
}

// sendCommand は冪等な発行（EXP-27）。同じ idem_key の再送は新しい行を作らず既存を返す。
func sendCommand(ctx context.Context, sc *repo.Scope, in SendCommandInput) (*Command, error) {
	// 既にあるなら、それを返す（冪等ヒット）。
	if existing, err := findCommandByIdem(ctx, sc, in.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	payload := ""
	if in.Payload != nil {
		payload = *in.Payload
	}
	cmd := Command{ID: newCommandID(), Type: in.Type, State: "pending", ScheduledFor: time.Now()}
	const ins = `INSERT INTO cmd_command
	   (tenant_id, command_id, robot_id, type, payload, state, idem_key, scheduled_for)
	   VALUES (:tenant, ?, ?, ?, ?, 'pending', ?, ?)`
	_, err := sc.Exec(ctx, "gql.sendCommand", ins, repo.ExpectOne,
		cmd.ID, in.RobotID, in.Type, payload, in.IdempotencyKey, cmd.ScheduledFor)
	if err != nil {
		// 競合（同じ idem_key を同時に送った）→ 一意キーが二重を弾く。既存を返して冪等を保つ。
		if errors.Is(err, repo.ErrConflict) {
			if existing, ferr := findCommandByIdem(ctx, sc, in.IdempotencyKey); ferr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return &cmd, nil
}

func newCommandID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("cmd-%s", hex.EncodeToString(b[:]))
}
