// Package deploylab は EXP-32（ゼロダウンタイムのスキーマ変更＝expand/contract）の実験本体。
//
// ローリングデプロイ中は、旧アプリ(v1)と新アプリ(v2)が同じ DB を同時に触る。
// 「列を一気に張り替える」と、その瞬間に旧アプリが壊れる。expand/contract（足す→両対応→
// 切替→消す）なら、どの瞬間も両バージョンが動く。列 status を state に改名する例で確かめる。
//
// DDL 主体なので生の *sql.DB を直接使う。
package deploylab

import (
	"context"
	"database/sql"
)

// Reset は実験表を作り直し、1行入れる（v1 スキーマ: id, status）。
func Reset(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DROP TABLE IF EXISTS ecq",
		`CREATE TABLE ecq (id BIGINT PRIMARY KEY, status INT NOT NULL) ENGINE=InnoDB`,
		"INSERT INTO ecq (id, status) VALUES (1, 1)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// v1: 旧アプリ。status 列を読み書きする。
func V1Read(ctx context.Context, db *sql.DB) error {
	var id int64
	var status int
	return db.QueryRowContext(ctx, "SELECT id, status FROM ecq WHERE id=1").Scan(&id, &status)
}

func V1Write(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, "INSERT INTO ecq (id, status) VALUES (?, 1)", id)
	return err
}

// v2: 新アプリ。state 列を読み、書き込みは移行期は両方に書く（dual-write）。
func V2ReadState(ctx context.Context, db *sql.DB) error {
	var id int64
	var state int
	return db.QueryRowContext(ctx, "SELECT id, state FROM ecq WHERE id=1").Scan(&id, &state)
}

// V2WriteDual は status と state の両方に書く（移行期。旧も新も読めるように）。
func V2WriteDual(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, "INSERT INTO ecq (id, status, state) VALUES (?, 1, 1)", id)
	return err
}

// V2WriteStateOnly は state だけに書く（contract 後。status は消えている）。
func V2WriteStateOnly(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, "INSERT INTO ecq (id, state) VALUES (?, 1)", id)
	return err
}

// --- DDL 手順 ---

// RenameInOneStep は「一気に張り替える」危険なやり方（status → state）。
func RenameInOneStep(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "ALTER TABLE ecq CHANGE status state INT NOT NULL")
	return err
}

// Expand は state 列を足して backfill する（status はまだ残す。v1 は無傷）。
func Expand(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "ALTER TABLE ecq ADD COLUMN state INT NULL"); err != nil {
		return err
	}
	//smlint:allow rowsaffected 理由: backfill。件数は使わない
	_, err := db.ExecContext(ctx, "UPDATE ecq SET state = status WHERE state IS NULL")
	return err
}

// Contract は旧列 status を落とす（v1 が全て退役した後にだけやる）。
func Contract(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "ALTER TABLE ecq DROP COLUMN status")
	return err
}
