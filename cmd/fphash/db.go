package main

import "database/sql"

func openDB(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) }
