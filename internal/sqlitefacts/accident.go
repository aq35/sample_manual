package sqlitefacts

// SQLite に持ち込んだときだけ起きる事故を、実際に再現する。
//
// 「差分表を書く」のではなく、**壊れるところまでやる**。
// どれも「MySQL では起きない／別の形で起きる」もの。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ---- ① 接続ごとの PRAGMA ----

// PragmaPerConn は「PRAGMA を db.Exec で入れた」ときに何が起きるかを測る。
//
// SQLite の PRAGMA は**接続ごと**の設定。*sql.DB はプールなので、
// db.Exec("PRAGMA foreign_keys=ON") は、そのとき割り当てられた1本にしか効かない。
// 「起動時に1回入れた」つもりが、実際には接続の一部だけ設定されている。
//
// 戻り値は (設定が効いていた接続数, 調べた接続数)。
func PragmaPerConn(ctx context.Context, db *sql.DB, conns int, viaExec bool) (int, int, error) {
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)

	if viaExec {
		// ★事故のもと: 「起動時に1回」入れる
		if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			return 0, 0, err
		}
	}

	// プールの中の接続を conns 本すべて同時に握って、1本ずつ聞く。
	// 同時に握らないと同じ接続を使い回してしまい、事故が見えない（測定器の罠）。
	held := make([]*sql.Conn, 0, conns)
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	on := 0
	for i := 0; i < conns; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			return 0, 0, err
		}
		held = append(held, c)
		var v int
		//smlint:allow loopquery 理由: プールの各接続に1回ずつ聞くのが実験の目的そのもの
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&v); err != nil {
			return 0, 0, err
		}
		if v == 1 {
			on++
		}
	}
	return on, conns, nil
}

// ---- ② 遅延トランザクションの昇格 ----

// DeferredUpgrade は「読んでから書く」トランザクションを2本同時に走らせる。
//
// SQLite の BEGIN は既定で DEFERRED。読んだ時点では読み取りロックしか取らず、
// 最初の書き込みで書き込みロックへ**昇格**しようとする。
// 両方が読んでから書こうとすると、片方は SQLITE_BUSY になる。
// しかも busy_timeout は**待っても解決しない**（相手も待っているので、待つだけ無駄）。
//
// immediate=true なら BEGIN IMMEDIATE で最初から書き込みロックを取る。
// 待てば必ず順番が来るので、busy_timeout が効く。
//
// 戻り値は (成功したトランザクション数, busy で失敗した数)。
func DeferredUpgrade(ctx context.Context, db *sql.DB, tenant string, immediate bool, rounds int) (int, int, error) {
	if _, err := db.ExecContext(ctx,
		"INSERT OR REPLACE INTO robot_state (tenant_id, robot_id, version, payload) VALUES (?,?,0,'{}')",
		tenant, "conflict"); err != nil {
		return 0, 0, err
	}

	var ok, busy atomic.Int64
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := readThenWrite(ctx, db, tenant, immediate)
				switch {
				case err == nil:
					ok.Add(1)
				case IsBusy(err):
					busy.Add(1)
				default:
					busy.Add(1) // 失敗はすべて「進めなかった」に寄せる（内訳は Notes に書く）
				}
			}()
		}
		close(start)
		wg.Wait()
	}
	return int(ok.Load()), int(busy.Load()), nil
}

func readThenWrite(ctx context.Context, db *sql.DB, tenant string, immediate bool) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	begin := "BEGIN"
	if immediate {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err := conn.ExecContext(ctx, begin); err != nil {
		return err
	}
	var v int64
	if err := conn.QueryRowContext(ctx,
		"SELECT version FROM robot_state WHERE tenant_id = ? AND robot_id = 'conflict'", tenant).Scan(&v); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	// 「読んでから決める」処理があるつもりの間（相手にも読ませる）
	time.Sleep(2 * time.Millisecond)

	//smlint:allow rowsaffected 理由: 測っているのは「昇格できたか」。行数ではなく SQLITE_BUSY の有無を見ている
	if _, err := conn.ExecContext(ctx,
		"UPDATE robot_state SET version = ? WHERE tenant_id = ? AND robot_id = 'conflict'",
		v+1, tenant); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}

// ---- ③ マイグレーションの途中で落ちる ----

// MigrateInTx は「マイグレーション全体を1つのトランザクションで流し、途中で失敗させる」。
//
// MySQL では DDL が暗黙にコミットされるので、これは**できない**（EXP-6）。
// SQLite ではできる。できるなら、EXP-6 で必要だった「段階の記録」は要らなくなるのか？
// を確かめるための関数。戻り値は失敗後に残っている表の一覧。
func MigrateInTx(ctx context.Context, db *sql.DB, stmts []string) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var failed bool
	for _, s := range stmts {
		//smlint:allow loopquery 理由: マイグレーションを順に流す実験そのもの
		if _, err := tx.ExecContext(ctx, s); err != nil {
			failed = true
			break
		}
	}
	if failed {
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
	} else if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tables(ctx, db, "exp10_m%")
}

func tables(ctx context.Context, db *sql.DB, like string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ? ORDER BY name", like)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- ④ バックアップ ----

// BackupResult は復元してみた結果。
type BackupResult struct {
	Method     string
	Bytes      int64
	Rows       int64
	Err        string
	Integrity  string
	SchemaHash string
}

// CopyMainFile は「.db だけをコピーする」（よくやる間違い）。
//
// WAL モードでは、コミット済みのデータは -wal 側に載っている。
// .db だけを持っていくと、**成功したはずの書き込みが消える**。
func CopyMainFile(src, dst string) (int64, error) { return copyFile(src, dst) }

// CopyWithWAL は .db / -wal / -shm を全部コピーする。
//
// これでも「コピー中に書き込みが進む」と壊れる（ファイルの間で時刻がずれる）。
func CopyWithWAL(src, dst string) (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		n, err := copyFile(src+suffix, dst+suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return total, err
		}
		total += n
	}
	return total, nil
}

// VacuumInto は SQLite に整合したコピーを作らせる（3.27 以降）。
//
// 読み取りトランザクションの中で作られるので、コピー中の書き込みと混ざらない。
func VacuumInto(ctx context.Context, db *sql.DB, dst string) (int64, error) {
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return 0, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func copyFile(src, dst string) (int64, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

// Verify は復元したファイルを開いて中身を確かめる。
//
// ★「バックアップコマンドが成功した」を成功条件にしない。
// 開いて、integrity_check を通して、行数ではなく**中身のハッシュ**まで見る。
func Verify(ctx context.Context, d Driver, path, tenant string) BackupResult {
	res := BackupResult{Bytes: fileSize(path)}
	db, err := d.Open(path, Pragmas{BusyTimeout: 2 * time.Second})
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer func() { _ = db.Close() }()

	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res.Integrity); err != nil {
		res.Err = "integrity_check: " + err.Error()
		return res
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM robot_state WHERE tenant_id = ?", tenant).Scan(&res.Rows); err != nil {
		res.Err = err.Error()
		return res
	}
	// 行数だけでは「同じ中身」と言えない。値まで含めて畳む。
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(GROUP_CONCAT(h, ''), '') FROM (
			SELECT robot_id || ':' || version || ':' || payload AS h
			FROM robot_state WHERE tenant_id = ? ORDER BY robot_id)`, tenant).Scan(&res.SchemaHash); err != nil {
		res.Err = err.Error()
		return res
	}
	res.SchemaHash = shortHash(res.SchemaHash)
	return res
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TempDB は使い捨ての DB を用意し、テナントと n 行を入れる。
func TempDB(ctx context.Context, d Driver, p Pragmas, tenant string, rows int) (*sql.DB, string, func(), error) {
	dir, err := os.MkdirTemp("", "exp10")
	if err != nil {
		return nil, "", nil, err
	}
	path := filepath.Join(dir, "exp.db")
	db, err := d.Open(path, p)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, "", nil, err
	}
	cleanup := func() { _ = db.Close(); _ = os.RemoveAll(dir) }
	if err := Setup(ctx, db, tenant); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	if rows > 0 {
		if err := Seed(ctx, db, tenant, rows); err != nil {
			cleanup()
			return nil, "", nil, err
		}
	}
	return db, path, cleanup, nil
}

func shortHash(s string) string {
	// 実験の記録に載せる目的なので、暗号学的な強度は要らない（FNV-1a）。
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// CommitN は n 行を「1行1トランザクション」で入れる。
//
// 「コミットが返った」ことの意味を測るための関数。
// 返ってきたのに、プロセスが落ちたら消えているのか？ を後から数える。
func CommitN(ctx context.Context, db *sql.DB, tenant string, n int) error {
	for i := 0; i < n; i++ {
		//smlint:allow loopquery 理由: 1行1トランザクションで書くのが実験の条件そのもの
		//smlint:allow rowsaffected 理由: 入るかどうかはエラーで判る。あとで件数を数え直す
		if _, err := db.ExecContext(ctx,
			"INSERT OR REPLACE INTO receipt (tenant_id, idem_key, body) VALUES (?,?,?)",
			tenant, fmt.Sprintf("k%06d", i), `{"i":`+fmt.Sprint(i)+`}`); err != nil {
			return err
		}
	}
	return nil
}

// CountReceipts は受領記録の件数（落ちたあとに数え直す用）。
func CountReceipts(ctx context.Context, db *sql.DB, tenant string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM receipt WHERE tenant_id = ?", tenant).Scan(&n)
	return n, err
}
