// Package ingestlab は EXP-65（不正データの「入口」）の実験本体。
//
// 通常の動作確認では入らないデータが混入して worker の処理が前に進まなくなる。原因は2つ:
//   - 作成時のバリデーションの不備（アプリ経路のチェック漏れ）
//   - 直接 DB 入力（手動 SQL・別ツール・移行スクリプトがアプリを迂回して書く）
//
// アプリのバリデーションは「アプリ経路」しか守れない。直接 DB 入力はそれを素通りする。
// DB 制約（ENUM / CHECK / FK / NOT NULL）だけが、経路に関係なく不正な書き込みを入口で弾く。
// この実験は「同じ不正 INSERT が、ガード無し表には landing し、ガード有り表には弾かれる」を測る。
package ingestlab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed schema.sql
var schemaSQL string

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定 DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ingest schema: %w\n%s", err, stmt)
		}
	}
	// FK の参照先を1件用意する（正しい行はこの ref_id=1 を指す）。
	//smlint:allow rowsaffected 理由: マスタ投入。件数は使わない
	if _, err := db.ExecContext(ctx, "INSERT IGNORE INTO ing_ref (id) VALUES (1)"); err != nil {
		return err
	}
	return nil
}

// Reset は両表のテナント行を消す。
func Reset(ctx context.Context, db *sql.DB, tenant string) error {
	for _, t := range []string{"ing_open", "ing_guarded"} {
		//smlint:allow loopquery 理由: 実験前の後片付け。固定2表
		//smlint:allow rowsaffected 理由: 後片付け
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id=?", tenant); err != nil {
			return err
		}
	}
	return nil
}

// AppValidate は「アプリ経路のバリデーション」を Go で表したもの。
// 直接 DB 入力（手動 SQL 等）はこの関数を通らない＝迂回できることが要点。
func AppValidate(status string, amount, refID int64) error {
	switch status {
	case "pending", "in_progress", "completed":
	default:
		return fmt.Errorf("不正な状態: %q", status)
	}
	if amount < 0 {
		return fmt.Errorf("金額が負: %d", amount)
	}
	if refID != 1 {
		return fmt.Errorf("存在しない参照: %d", refID)
	}
	return nil
}

// DirectInsert は「アプリのバリデーションを迂回した直接 DB 入力」。AppValidate を通さずに INSERT する。
// 返り値 err が nil なら landing（＝poison が DB に残った）、非 nil なら DB 制約が入口で弾いた。
func DirectInsert(ctx context.Context, db *sql.DB, table, tenant string, id int64, status string, amount, refID int64) error {
	//smlint:allow loopquery 理由: 1件ずつ入口の可否を測る（実験対象）
	//smlint:allow rowsaffected 理由: landing したか否かは err で判定。件数は使わない
	_, err := db.ExecContext(ctx,
		"INSERT INTO "+table+" (tenant_id, id, status, amount, ref_id) VALUES (?,?,?,?,?)",
		tenant, id, status, amount, refID)
	return err
}

// BadCase は入口で試す不正データの1種。
type BadCase struct {
	Name   string
	Status string
	Amount int64
	RefID  int64
}

// BadCases は代表的な混入パターン（未知状態・負値・存在しない参照）。
func BadCases() []BadCase {
	return []BadCase{
		{Name: "未知の状態(frozen)", Status: "frozen", Amount: 10, RefID: 1},
		{Name: "負の金額(-1)", Status: "pending", Amount: -1, RefID: 1},
		{Name: "存在しない参照(ref=999)", Status: "pending", Amount: 10, RefID: 999},
	}
}

// Landed は表に landing した行数（poison を含む全行）。
func Landed(ctx context.Context, db *sql.DB, table, tenant string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", tenant).Scan(&n)
	return n, err
}

// SQLMode は現在の sql_mode（STRICT が入っているかの確認用）。
func SQLMode(ctx context.Context, db *sql.DB) string {
	var m string
	_ = db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&m)
	return m
}
