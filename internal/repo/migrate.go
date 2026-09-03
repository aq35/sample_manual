package repo

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrMigrationChanged は、適用済みのマイグレーションが後から書き換えられたときのエラー。
//
// これは運用でいちばん静かに壊れる事故。「開発環境では動く（作り直したから）」
// 「本番だけ表の形が違う」になる。適用済みのファイルは書き換えず、新しい番号を足す。
var ErrMigrationChanged = errors.New("適用済みのマイグレーションが書き換えられている")

// Migration は1つのマイグレーション。
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// LoadMigrations は埋め込んだ migrations/*.sql を番号順に返す。
// ファイル名は `0001_なにをするか.sql`。
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(e.Name(), ".sql"), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("マイグレーション名が %q。`0001_説明.sql` の形にすること", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("マイグレーション名の番号が読めない: %q", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  v,
			Name:     parts[1],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("マイグレーション番号 %04d が重複している", out[i].Version)
		}
	}
	return out, nil
}

// Migrate は未適用のマイグレーションを順に当てる。
//
// 運用で起きることを前提にしてある。
//   - **複数コンテナが同時に起動する** → GET_LOCK で1つだけが当てる（他は待って、何もしない）
//   - **適用済みファイルの書き換え** → チェックサムで検出して止める
//   - **途中で失敗** → そこで止める。以降は当てない（半端な状態で先へ進まない）
func Migrate(ctx context.Context, d *DB) error {
	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}

	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INT          NOT NULL,
		name       VARCHAR(191) NOT NULL,
		checksum   CHAR(64)     NOT NULL,
		applied_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		PRIMARY KEY (version)
	) ENGINE=InnoDB`
	if _, err := d.sqldb.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("schema_migrations の作成に失敗: %w", err)
	}

	// 同時起動しても当てるのは1つだけ。取れなければ、他が当て終わるのを待つ
	const lockName = "repo_migrate"
	var got int
	if err := d.sqldb.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", lockName, 30).Scan(&got); err != nil {
		return fmt.Errorf("マイグレーションのロック取得に失敗: %w", err)
	}
	if got != 1 {
		return errors.New("マイグレーションのロックを 30 秒待っても取れなかった")
	}
	defer func() {
		_, _ = d.sqldb.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", lockName)
	}()

	applied, err := appliedMigrations(ctx, d)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if prev, ok := applied[m.Version]; ok {
			if prev != m.Checksum {
				return fmt.Errorf("%w: %04d_%s（適用済みのファイルは書き換えず、新しい番号を足すこと）",
					ErrMigrationChanged, m.Version, m.Name)
			}
			continue
		}
		start := time.Now()
		if err := applyMigration(ctx, d, m); err != nil {
			return err
		}
		d.opt.Logger.Info("マイグレーションを適用した",
			"version", m.Version, "name", m.Name, "所要", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func appliedMigrations(ctx context.Context, d *DB) (map[int]string, error) {
	rows, err := d.sqldb.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// applyMigration は1つのマイグレーションを当てる。
//
// ★MySQL の DDL は暗黙にコミットされるので、「複数の DDL をまとめてロールバック」はできない。
// だから **1ファイル1つの変更**にしておく。失敗したときに、どこまで進んだかが分かる。
func applyMigration(ctx context.Context, d *DB, m Migration) error {
	for _, stmt := range splitSQL(m.SQL) {
		if _, err := d.sqldb.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("マイグレーション %04d_%s に失敗: %w\nSQL: %s", m.Version, m.Name, err, stmt)
		}
	}
	_, err := d.sqldb.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES (?,?,?)",
		m.Version, m.Name, m.Checksum)
	if err != nil {
		return fmt.Errorf("マイグレーション %04d の記録に失敗: %w", m.Version, err)
	}
	return nil
}

func splitSQL(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";") {
		var lines []string
		for _, ln := range strings.Split(part, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		if stmt := strings.TrimSpace(strings.Join(lines, "\n")); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
