// Package sqlitefacts は EXP-10（SQLite companion）の実験本体。
//
// 目的は「SQLite でも同じことができるか」ではなく、
// **MySQL で確かめた結論のうち、どれがそのまま持ち込めて、どれが持ち込めないか**を
// measure で切り分けること。
//
// ★このパッケージの禁じ手: MySQL の結論を SQLite の結論として流用すること。
// 同じ問いを両方のエンジンに同じ条件で投げ、観測値を突き合わせてから分類する。
// 片方しか測れなかった問いは SAME だと言わずに UNVERIFIED として残す。
package sqlitefacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // cgo 版
	_ "modernc.org/sqlite"          // pure Go 版

	"github.com/aq35/sample_manual/internal/expkit"
)

// ---- ドライバ ----

// Driver は比較する2つの SQLite ドライバ。
//
// ★DSN の書き方がドライバごとに違う（それ自体が移植時の落とし穴）。
// modernc: ?_pragma=journal_mode(WAL)
// mattn:   ?_journal_mode=WAL
// PRAGMA は**接続ごと**の設定なので、DSN に書いて「新しい接続すべてに効く」形にする。
// db.Exec("PRAGMA ...") は、そのとき使われた1本にしか効かない（Accident_接続ごとのPRAGMA 参照）。
type Driver struct {
	Name    string // 実験の中で使う名前
	SQLName string // database/sql に登録されている名前
	CGO     bool   // cgo が要るか
	pragma  func(k, v string) string
}

var (
	// PureGo は modernc.org/sqlite（C を Go へ変換したもの。cgo 不要）。
	PureGo = Driver{
		Name: "modernc(pure Go)", SQLName: "sqlite", CGO: false,
		pragma: func(k, v string) string { return "_pragma=" + k + "(" + v + ")" },
	}
	// CGO は github.com/mattn/go-sqlite3（本家 C を cgo で呼ぶ）。
	CGO = Driver{
		Name: "mattn(cgo)", SQLName: "sqlite3", CGO: true,
		pragma: func(k, v string) string { return "_" + k + "=" + v },
	}
)

// Drivers は比較対象の一覧。
func Drivers() []Driver { return []Driver{PureGo, CGO} }

// ByName はドライバ名（"purego" / "cgo"）から Driver を引く（子プロセス用）。
func ByName(s string) (Driver, error) {
	switch s {
	case "purego", "modernc":
		return PureGo, nil
	case "cgo", "mattn":
		return CGO, nil
	}
	return Driver{}, fmt.Errorf("知らないドライバ: %q", s)
}

// Pragmas は接続時に必ず当てる設定。
//
// 既定値は「運用でこれを外すと事故る」と分かっているもの。
//   - journal_mode=WAL       … 読み手と書き手が同時に動ける（既定の delete では読みが書きを止める）
//   - busy_timeout=5000      … 競合したとき即エラーではなく待つ
//   - foreign_keys=1         … SQLite は既定で **外部キーを検査しない**
//   - synchronous=NORMAL     … WAL ではこれが実用的な既定
type Pragmas struct {
	JournalMode string
	BusyTimeout time.Duration
	ForeignKeys bool
	Synchronous string
	Extra       map[string]string
}

// DefaultPragmas は「事故を減らすほうへ倒した」設定。
func DefaultPragmas() Pragmas {
	return Pragmas{
		JournalMode: "WAL",
		BusyTimeout: 5 * time.Second,
		ForeignKeys: true,
		Synchronous: "NORMAL",
	}
}

// BarePragmas は何も指定しない（SQLite の素の既定値を見るため）。
func BarePragmas() Pragmas { return Pragmas{} }

func (p Pragmas) values() [][2]string {
	var out [][2]string
	if p.JournalMode != "" {
		out = append(out, [2]string{"journal_mode", p.JournalMode})
	}
	if p.BusyTimeout > 0 {
		out = append(out, [2]string{"busy_timeout", fmt.Sprint(p.BusyTimeout.Milliseconds())})
	}
	if p.ForeignKeys {
		out = append(out, [2]string{"foreign_keys", "1"})
	}
	if p.Synchronous != "" {
		out = append(out, [2]string{"synchronous", p.Synchronous})
	}
	for k, v := range p.Extra {
		out = append(out, [2]string{k, v})
	}
	return out
}

// DSN は path とプラグマから接続文字列を組み立てる。
func (d Driver) DSN(path string, p Pragmas) string {
	var q []string
	for _, kv := range p.values() {
		q = append(q, d.pragma(kv[0], kv[1]))
	}
	dsn := "file:" + path
	if len(q) > 0 {
		dsn += "?" + strings.Join(q, "&")
	}
	return dsn
}

// Open は DSN で開く。プラグマは**新しい接続すべてに**当たる。
func (d Driver) Open(path string, p Pragmas) (*sql.DB, error) {
	db, err := sql.Open(d.SQLName, d.DSN(path, p))
	if err != nil {
		return nil, err
	}
	// SQLite の書き込みは1つずつしか進まない（データベース全体に1つの書き込みロック）。
	// プールを大きくしても書き込みは速くならず、SQLITE_BUSY が増えるだけ。
	// ここでは既定を作らず、実験ごとに明示する。
	return db, nil
}

// OpenTemp は使い捨てのファイルで開く。戻り値の第2は後片付け。
func (d Driver) OpenTemp(p Pragmas) (*sql.DB, string, func(), error) {
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
	return db, path, func() { _ = db.Close(); _ = os.RemoveAll(dir) }, nil
}

// Version は sqlite ライブラリのバージョン（ドライバによって違う）。
func Version(ctx context.Context, db *sql.DB) string {
	var v string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil {
		return "?"
	}
	return v
}

// ---- 競合の見分け ----

// IsBusy は「他が書いているので進めなかった」エラーか。
//
// ★ドライバによってエラーの型も文言も違う。移植時にここを間違えると
// 「競合」と「本物の失敗」を一緒くたに再試行してしまう。
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked") ||
		strings.Contains(s, "sqlite_busy") ||
		strings.Contains(s, "(5)") && strings.Contains(s, "busy")
}

// ---- スキーマ ----

// Schema は実験に使う表。MySQL 側（internal/repo/migrations）と同じ形を狙う。
const Schema = `
CREATE TABLE IF NOT EXISTS tenant (
	tenant_id TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS robot_state (
	tenant_id  TEXT    NOT NULL,
	robot_id   TEXT    NOT NULL,
	version    INTEGER NOT NULL DEFAULT 0,
	payload    TEXT    NOT NULL,
	updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY (tenant_id, robot_id),
	FOREIGN KEY (tenant_id) REFERENCES tenant(tenant_id)
);
CREATE TABLE IF NOT EXISTS receipt (
	tenant_id TEXT NOT NULL,
	idem_key  TEXT NOT NULL,
	body      TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idem_key)
);
`

// Setup はスキーマを作り、テナントを1つ入れる。
func Setup(ctx context.Context, db *sql.DB, tenant string) error {
	for _, stmt := range strings.Split(Schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定の DDL を順に流すだけで、行の N+1 ではない
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("スキーマ作成: %w\n%s", err, stmt)
		}
	}
	//smlint:allow rowsaffected 理由: 既にあれば 0 行で正しい（INSERT OR IGNORE）
	if _, err := db.ExecContext(ctx,
		"INSERT OR IGNORE INTO tenant (tenant_id) VALUES (?)", tenant); err != nil {
		return err
	}
	return nil
}

// Seed は robot_state に n 行入れる。
func Seed(ctx context.Context, db *sql.DB, tenant string, n int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR REPLACE INTO robot_state (tenant_id, robot_id, version, payload) VALUES (?,?,?,?)")
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for i := 0; i < n; i++ {
		//smlint:allow loopquery 理由: 初期データ投入。準備済み文で n 行入れるのが目的
		//smlint:allow rowsaffected 理由: 初期データ投入。1 行ずつ確実に入る前提
		if _, err := stmt.ExecContext(ctx, tenant, fmt.Sprintf("r%06d", i), 0, `{"v":0}`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- スループット ----

// Throughput は1回の測定結果。
type Throughput struct {
	Ops     int64
	Busy    int64
	Errs    int64
	Elapsed time.Duration
	Latency expkit.LatencyStats
}

// PerSec は1秒あたりの成功数。
func (t Throughput) PerSec() float64 {
	if t.Elapsed <= 0 {
		return 0
	}
	return float64(t.Ops) / t.Elapsed.Seconds()
}

// WriteLoad は「1行更新のトランザクション」を concurrency 本で dur だけ回す。
//
// MySQL 側（EXP-5 poollab.WriteTx）と同じ形の負荷にしてある。
// ただし**絶対値を engine 間で比べない**。比べるのは
// 「同じエンジンの中で設定を変えたときの差」と「競合したときの振る舞い」。
func WriteLoad(ctx context.Context, db *sql.DB, tenant string, rows, concurrency int, dur time.Duration) Throughput {
	lat := expkit.NewLatency()
	var ops, busy, errs atomic.Int64

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := w
			for time.Now().Before(deadline) && ctx.Err() == nil {
				i += concurrency
				id := fmt.Sprintf("r%06d", i%rows)
				t0 := time.Now()
				//smlint:allow rowsaffected 理由: 負荷生成。測っているのはスループットと SQLITE_BUSY の数
				//smlint:allow loopquery 理由: 負荷生成。ループで打ち続けることが実験の条件そのもの
				_, err := db.ExecContext(ctx,
					"UPDATE robot_state SET version = version + 1, payload = ? WHERE tenant_id = ? AND robot_id = ?",
					fmt.Sprintf(`{"v":%d}`, i), tenant, id)
				switch {
				case err == nil:
					lat.Record(time.Since(t0))
					ops.Add(1)
				case IsBusy(err):
					busy.Add(1)
				default:
					errs.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	return Throughput{
		Ops: ops.Load(), Busy: busy.Load(), Errs: errs.Load(),
		Elapsed: time.Since(start), Latency: lat.Stats(),
	}
}

// ReadLoad は主キー1件の読み取りを回す。
func ReadLoad(ctx context.Context, db *sql.DB, tenant string, rows, concurrency int, dur time.Duration) Throughput {
	lat := expkit.NewLatency()
	var ops, busy, errs atomic.Int64

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := w
			for time.Now().Before(deadline) && ctx.Err() == nil {
				i += concurrency
				var v int64
				t0 := time.Now()
				//smlint:allow loopquery 理由: 負荷生成。ループで打ち続けることが実験の条件そのもの
				err := db.QueryRowContext(ctx,
					"SELECT version FROM robot_state WHERE tenant_id = ? AND robot_id = ?",
					tenant, fmt.Sprintf("r%06d", i%rows)).Scan(&v)
				switch {
				case err == nil || errors.Is(err, sql.ErrNoRows):
					lat.Record(time.Since(t0))
					ops.Add(1)
				case IsBusy(err):
					busy.Add(1)
				default:
					errs.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	return Throughput{
		Ops: ops.Load(), Busy: busy.Load(), Errs: errs.Load(),
		Elapsed: time.Since(start), Latency: lat.Stats(),
	}
}
