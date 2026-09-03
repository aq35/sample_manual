package domain

import "database/sql"

func Handle(db *sql.DB, id string) error { // want "生の \\*sql.DB"
	_ = db
	_ = id
	return nil
}

type Service struct {
	DB *sql.DB // want "生の \\*sql.DB"
}

// 逃げ道（理由つき）は通す
//
//smlint:allow rawdb 理由: 移行中。2026-12 までに repo.Scope へ置き換える
func Legacy(db *sql.DB) {}
