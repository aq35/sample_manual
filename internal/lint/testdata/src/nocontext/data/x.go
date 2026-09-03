package data

import "database/sql"

func Run(db *sql.DB) error {
	if _, err := db.Query("SELECT 1"); err != nil { // want "context を渡していない"
		return err
	}
	tx, err := db.Begin() // want "context を渡していない"
	if err != nil {
		return err
	}
	return tx.Rollback()
}
