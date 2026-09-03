// コマンド migratelab は EXP-6（マイグレーション途中の crash）の子プロセス。
//
// 本番のマイグレーション処理（internal/repo.Migrate）をそのまま呼び、
// 段階ごとの hook でプロセスを落とす。実験のために別実装を作らないのが要点。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/repo"
)

func main() {
	var (
		dsn    = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN")
		fresh  = flag.Bool("fresh", false, "先に表を落としてやり直す")
		report = flag.Bool("report", false, "マイグレーションを実行せず、状態だけ出す")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "dsn は必須")
		os.Exit(2)
	}

	ks := expkit.NewKillSwitch()
	db, err := repo.Open(*dsn, repo.Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MigrateLockWait: 20e9, // 20秒
		MigrationHook:   func(stage string) { ks.Point(stage) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if *fresh {
		for _, t := range []string{"schema_migrations", "robot_profile"} {
			if _, err := db.SQL().ExecContext(ctx, "DROP TABLE IF EXISTS "+t); err != nil {
				fmt.Fprintln(os.Stderr, "drop:", err)
				os.Exit(1)
			}
		}
	}
	if *report {
		st, err := state(ctx, db)
		if err != nil {
			fmt.Fprintln(os.Stderr, "report:", err)
			os.Exit(1)
		}
		b, _ := json.Marshal(st)
		fmt.Printf("%sSTATE %s\n", expkit.MarkerPrefix, b)
		return
	}

	if err := repo.Migrate(ctx, db); err != nil {
		fmt.Printf("%sMIGRATE_ERROR %v\n", expkit.MarkerPrefix, err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	fmt.Printf("%sDONE\n", expkit.MarkerPrefix)
}

// State は「いま DB がどうなっているか」。crash 後にこれを読んで説明できることが要件。
type State struct {
	Migrations []MigrationRow  `json:"migrations"`
	Objects    map[string]bool `json:"objects"`
}

type MigrationRow struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	State   string `json:"state"`
}

func state(ctx context.Context, db *repo.DB) (State, error) {
	var st State
	st.Objects = map[string]bool{}

	rows, err := db.SQL().QueryContext(ctx,
		`SELECT version, name, state FROM schema_migrations ORDER BY version`)
	if err == nil {
		for rows.Next() {
			var r MigrationRow
			if err := rows.Scan(&r.Version, &r.Name, &r.State); err != nil {
				_ = rows.Close()
				return st, err
			}
			st.Migrations = append(st.Migrations, r)
		}
		_ = rows.Close()
	}

	// 各マイグレーションが作るはずのもの
	checks := map[string]string{
		"table:robot_profile": `SELECT COUNT(*) FROM information_schema.tables
		                        WHERE table_schema = DATABASE() AND table_name = 'robot_profile'`,
		"index:idx_profile_created": `SELECT COUNT(*) FROM information_schema.statistics
		                        WHERE table_schema = DATABASE() AND table_name = 'robot_profile'
		                          AND index_name = 'idx_profile_created'`,
		"column:retired": `SELECT COUNT(*) FROM information_schema.columns
		                        WHERE table_schema = DATABASE() AND table_name = 'robot_profile'
		                          AND column_name = 'retired'`,
		"index:idx_profile_retired": `SELECT COUNT(*) FROM information_schema.statistics
		                        WHERE table_schema = DATABASE() AND table_name = 'robot_profile'
		                          AND index_name = 'idx_profile_retired'`,
	}
	for name, q := range checks {
		var n int
		if err := db.SQL().QueryRowContext(ctx, q).Scan(&n); err != nil {
			return st, err
		}
		st.Objects[name] = n > 0
	}
	return st, nil
}
