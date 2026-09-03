package repo_test

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

var rnd = rand.New(rand.NewSource(20260902))

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\nSQL: %s", err, q)
	}
}

// explainLine は実行計画を1行にまとめる。
func explainLine(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN " + q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		get := func(name string) string {
			for i, c := range cols {
				if c == name {
					if vals[i] == nil {
						return "-"
					}
					if b, ok := vals[i].([]byte); ok {
						return string(b)
					}
					return fmt.Sprint(vals[i])
				}
			}
			return "-"
		}
		out += fmt.Sprintf("type=%s key=%s rows=%s filtered=%s extra=%s",
			get("type"), get("key"), get("rows"), get("filtered"), get("Extra"))
	}
	return out
}

// tableSize は表＋索引の物理サイズ（MB）。
func tableSize(t *testing.T, db *sql.DB, table string) float64 {
	t.Helper()
	var mb sql.NullFloat64
	err := db.QueryRow(`SELECT (data_length + index_length) / 1024 / 1024
	                    FROM information_schema.tables
	                    WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&mb)
	if err != nil {
		t.Fatal(err)
	}
	return mb.Float64
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func countRows(t *testing.T, rows *sql.Rows) int {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustDSN(b *testing.B) string {
	b.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		b.Skip("MYSQL_DSN が未設定のため skip")
	}
	return dsn
}
