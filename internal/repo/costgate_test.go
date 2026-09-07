package repo_test

// クエリコスト・ゲートのテスト（走査見込みが予算超なら実行前に弾く）。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run TestCostGate -v

import (
	"context"
	"errors"
	"testing"

	"github.com/aq35/sample_manual/internal/repo"
)

func TestCostGate_索引で絞れば通る_舐めると弾く(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	// 本番に近い件数を入れる（実行計画は件数で変わる。EXP-7）
	seedProfiles(t, db, tenantA, 3000)

	sc := db.Tenant(tenantA)

	// ① 索引で絞る keyset 一覧（LIMIT つき）→ 走査見込みは小さい → 通る
	okQ := `SELECT robot_id, name FROM robot_profile
	         WHERE tenant_id = :tenant AND robot_id > ? ORDER BY robot_id LIMIT ?`
	if err := sc.CheckCost(ctx, "profile.list", okQ, 200, "r0001", 50); err != nil {
		t.Errorf("索引で絞った検索が弾かれた: %v", err)
	}

	// ② テナント全体を舐める検索（robot_id で絞らない）→ 走査見込み大 → 予算100で弾く
	wideQ := `SELECT robot_id, name FROM robot_profile
	           WHERE tenant_id = :tenant AND name LIKE ?`
	err := sc.CheckCost(ctx, "profile.search", wideQ, 100, "%ロボット%")
	if !errors.Is(err, repo.ErrTooCostly) {
		t.Errorf("舐める検索が弾かれていない（ErrTooCostly のはず）: %v", err)
	} else {
		t.Logf("舐める検索を実行前に弾いた: %v", err)
	}

	// ③ GuardedQuery: 予算内なら実行できる
	rows, err := sc.GuardedQuery(ctx, "profile.list", 200, okQ, "r0001", 50)
	if err != nil {
		t.Fatalf("GuardedQuery が通らない: %v", err)
	}
	_ = rows.Close()

	// ④ GuardedQuery: 予算超なら Query せずに弾く
	if _, err := sc.GuardedQuery(ctx, "profile.search", 100, wideQ, "%ロボット%"); !errors.Is(err, repo.ErrTooCostly) {
		t.Errorf("GuardedQuery が高コストを弾いていない: %v", err)
	}
}
