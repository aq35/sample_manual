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

// ErrMigrationUnfinished は、前回のマイグレーションが途中で終わっているときのエラー。
//
// 記録（started_at）はあるのに完了（finished_at）していない状態。
// 黙って先へ進むと「永久に当たらないマイグレーション」になるので、起動を止める。
var ErrMigrationUnfinished = errors.New("前回のマイグレーションが途中で終わっている")

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
//
// マイグレーションの段階。EXP-6 でこの各点にプロセスの死を注入する。
const (
	StageBeforeLock         = "before_lock"
	StageAfterLock          = "after_lock"
	StageAfterStarted       = "after_started"
	StageDuringDDL          = "during_ddl"
	StageAfterDDLBeforeDone = "after_ddl_before_done"
	StageAfterDone          = "after_done"
	StageAfterAll           = "after_all"
)

// MigrationStages は全段階（実験が総当たりするための一覧）。
var MigrationStages = []string{
	StageBeforeLock, StageAfterLock, StageAfterStarted, StageDuringDDL,
	StageAfterDDLBeforeDone, StageAfterDone, StageAfterAll,
}

func Migrate(ctx context.Context, d *DB) error {
	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}
	hook := d.opt.MigrationHook
	if hook == nil {
		hook = func(string) {}
	}
	hook(StageBeforeLock)

	wait := d.opt.MigrateLockWait
	if wait <= 0 {
		wait = 30 * time.Second
	}

	// ★同時起動しても当てるのは1つだけ。
	// ここで GET_LOCK を使う理由（実験で確かめた、docs/locking.md）:
	//   - マイグレーションは DDL を流す。DDL は暗黙にコミットするので、
	//     「ロック用の行を SELECT ... FOR UPDATE」方式は最初の DDL で外れてしまう
	//   - ユーザーロックはトランザクションと無関係なので、DDL をまたいで保持される
	//   - 接続が切れれば即座に解放されるので、途中でプロセスが落ちても残らない
	// WithLock が接続の固定を引き受ける（*sql.DB のまま使うと解放できない）。
	return d.WithLock(ctx, "migrate", wait, func(ctx context.Context) error {
		hook(StageAfterLock)
		if err := ensureSchemaMigrations(ctx, d); err != nil {
			return err
		}

		applied, unfinished, err := appliedMigrations(ctx, d)
		if err != nil {
			return err
		}
		// 前回が途中で終わっている（記録はあるが完了していない）。
		// 実験（TestExperimentLock_先に記録する方式）のとおり、ここを黙って進むと
		// 「永久に当たらないマイグレーション」か「Table already exists で毎回落ちる」になる。
		// ロックは『同時に走らせない』ためのもので、『途中で落ちる』には効かない。
		if len(unfinished) > 0 {
			return fmt.Errorf("%w: %v（DB の状態を確認し、直してから schema_migrations を手で直すこと）",
				ErrMigrationUnfinished, unfinished)
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
			if err := applyMigration(ctx, d, m, hook); err != nil {
				return err
			}
			d.opt.Logger.Info("マイグレーションを適用した",
				"version", m.Version, "name", m.Name, "所要", time.Since(start).Round(time.Millisecond))
		}
		hook(StageAfterAll)
		return nil
	})
}

// ensureSchemaMigrations は記録用の表を用意する（この表だけはコードが面倒を見る）。
func ensureSchemaMigrations(ctx context.Context, d *DB) error {
	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version     INT          NOT NULL,
		name        VARCHAR(191) NOT NULL,
		checksum    CHAR(64)     NOT NULL,
		started_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		finished_at DATETIME(3)  NULL,
		-- state は「開始したが終わっていない」を見分けるため。
		-- ★既定値を 'done' にしてあるのが要点: 後から列を足したとき、
		--   既存の行は自動的に 'done' になる（別途 UPDATE で埋める必要がない）。
		--   埋めるための UPDATE は、件数が増えると重く、途中で落ちると中途半端になる
		state       ENUM('started','done') NOT NULL DEFAULT 'done',
		PRIMARY KEY (version)
	) ENGINE=InnoDB`
	if _, err := d.sqldb.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("schema_migrations の作成に失敗: %w", err)
	}
	// 古い形（started_at / finished_at が無い）で作られている場合に足す。
	// MySQL は ADD COLUMN IF NOT EXISTS を持たないので、自分で確認する
	for _, col := range []struct{ name, ddl string }{
		{"started_at", "ALTER TABLE schema_migrations ADD COLUMN started_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)"},
		{"finished_at", "ALTER TABLE schema_migrations ADD COLUMN finished_at DATETIME(3) NULL"},
		{"state", "ALTER TABLE schema_migrations ADD COLUMN state ENUM('started','done') NOT NULL DEFAULT 'done'"},
	} {
		var n int
		//smlint:allow loopquery 理由: 列の有無を確認する固定回数のループ。行の N+1 ではない
		err := d.sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'schema_migrations' AND column_name = ?`, col.name).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err := d.sqldb.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("schema_migrations に %s を足せない: %w", col.name, err)
			}
			// ここで「既存行を埋める UPDATE」を書かずに済んでいるのは、
			// state の既定値を 'done' にしてあるから（上の CREATE TABLE のコメント参照）
		}
	}
	return nil
}

// appliedMigrations は「完了した記録」と「開始したが完了していない記録」を返す。
func appliedMigrations(ctx context.Context, d *DB) (map[int]string, []string, error) {
	rows, err := d.sqldb.QueryContext(ctx,
		"SELECT version, name, checksum, state = 'done' FROM schema_migrations")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[int]string{}
	var unfinished []string
	for rows.Next() {
		var (
			v        int
			name     string
			sum      string
			finished bool
		)
		if err := rows.Scan(&v, &name, &sum, &finished); err != nil {
			return nil, nil, err
		}
		if !finished {
			unfinished = append(unfinished, fmt.Sprintf("%04d_%s", v, name))
			continue
		}
		out[v] = sum
	}
	return out, unfinished, rows.Err()
}

// applyMigration は1つのマイグレーションを当てる。
//
// ★MySQL の DDL は暗黙にコミットされるので、「複数の DDL をまとめてロールバック」はできない。
// だから **1ファイル1つの変更**にしておく。失敗したときに、どこまで進んだかが分かる。
func applyMigration(ctx context.Context, d *DB, m Migration, hook func(string)) error {
	// ① 始めることを記録する（finished_at は NULL のまま）
	if _, err := d.sqldb.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, state) VALUES (?,?,?,'started')",
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("マイグレーション %04d の開始を記録できない: %w", m.Version, err)
	}
	hook(StageAfterStarted)

	// ② 当てる
	for i, stmt := range splitSQL(m.SQL) {
		if i > 0 {
			// 複数の文を持つマイグレーションの途中（＝中途半端な形が存在しうる点）
			hook(StageDuringDDL)
		}
		if _, err := d.sqldb.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("マイグレーション %04d_%s に失敗: %w\nSQL: %s", m.Version, m.Name, err, stmt)
		}
	}
	hook(StageAfterDDLBeforeDone)
	// ③ 終わったことを記録する
	// ★影響行数を確かめる（EXP-9 の検査が見つけた。記録できていないのに
	// 「完了」と思い込むと、次の起動が黙って先へ進む）
	res, err := d.sqldb.ExecContext(ctx,
		"UPDATE schema_migrations SET finished_at = CURRENT_TIMESTAMP(3), state = 'done' WHERE version = ?",
		m.Version)
	if err != nil {
		return fmt.Errorf("マイグレーション %04d の完了を記録できない: %w", m.Version, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("マイグレーション %04d の完了記録が %d 行に当たった（1 行のはず）", m.Version, n)
	}
	hook(StageAfterDone)
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
