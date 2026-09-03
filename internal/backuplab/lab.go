// Package backuplab は EXP-11（バックアップ・復元・破損）の実験本体。
//
// この実験の一行め:
// **「バックアップコマンドが成功した」を成功条件にしない。**
//
// mysqldump は、書き込みの最中に取っても終了コード 0 を返す。
// 途中で切れたファイルも、古いファイルも、スキーマがずれたファイルも、
// 「バックアップが取れている」ように見える。
// だから確かめるのは、**別の環境へ戻して、中身のハッシュを突き合わせ、
// アプリケーションを起動して読み戻せるところまで**。
package backuplab

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ---- 接続情報 ----

// Target は復元先。DSN から CLI に渡す形へ組み替える。
type Target struct {
	DSN    string
	cfg    *mysql.Config
	Schema string
}

// Parse は DSN を読む。
func Parse(dsn string) (Target, error) {
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		return Target{}, err
	}
	return Target{DSN: dsn, cfg: c, Schema: c.DBName}, nil
}

// WithSchema は同じサーバの別データベースを指す Target を返す（＝別の環境）。
func (t Target) WithSchema(name string) Target {
	c := t.cfg.Clone()
	c.DBName = name
	return Target{DSN: c.FormatDSN(), cfg: c, Schema: name}
}

func (t Target) args() []string {
	host, port := "127.0.0.1", "3306"
	if t.cfg.Net == "tcp" {
		if i := strings.LastIndex(t.cfg.Addr, ":"); i >= 0 {
			host, port = t.cfg.Addr[:i], t.cfg.Addr[i+1:]
		} else {
			host = t.cfg.Addr
		}
	}
	return []string{"-h", host, "-P", port, "-u", t.cfg.User, "--protocol=TCP"}
}

func (t Target) env() []string {
	// パスワードをコマンド行に置かない（ps に出る）。
	return append(os.Environ(), "MYSQL_PWD="+t.cfg.Passwd)
}

// ---- バックアップ ----

// DumpOptions は取り方。**ここの1つの違いが、復元できるかどうかを分ける。**
type DumpOptions struct {
	// SingleTransaction: 一貫した断面を取るか。
	// これが無いと、表ごとにばらばらの時刻の内容が混ざる（InnoDB でも）。
	SingleTransaction bool
	// Tables: 対象。空ならデータベース全体。
	Tables []string
}

// Dump は取ったバックアップ。
type Dump struct {
	Path  string
	Bytes int64
	Took  time.Duration
	Cmd   string
}

// MySQLDump は mysqldump でバックアップを取る。
//
// ★戻り値のエラーが nil でも「バックアップが取れた」ことにはならない。
// 確かめるのは Restore して Compare してから。
func MySQLDump(ctx context.Context, t Target, path string, opt DumpOptions) (Dump, error) {
	args := t.args()
	if opt.SingleTransaction {
		args = append(args, "--single-transaction")
	}
	args = append(args, "--skip-lock-tables", "--no-tablespaces", "--set-gtid-purged=OFF", t.Schema)
	args = append(args, opt.Tables...)

	f, err := os.Create(path)
	if err != nil {
		return Dump{}, err
	}
	defer func() { _ = f.Close() }()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "mysqldump", args...)
	cmd.Env = t.env()
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Dump{}, fmt.Errorf("mysqldump: %w\n%s", err, stderr.String())
	}
	took := time.Since(start)
	fi, err := f.Stat()
	if err != nil {
		return Dump{}, err
	}
	return Dump{Path: path, Bytes: fi.Size(), Took: took,
		Cmd: "mysqldump " + strings.Join(args, " ")}, nil
}

// Restore はバックアップを復元先へ流し込む。
//
// 復元先の該当表を**先に落としてから**流す（上書きではなく作り直し）。
// mysqldump は表ごとに `DROP TABLE IF EXISTS` を吐くので流すだけでも上書きされるが、
// 「バックアップに含まれない表が復元先に残っている」状態を消すために、
// 対象表を明示的に落としておく（復元は『その環境をこの断面にする』ことなので、
// 余分な表が残ると別環境として扱えない）。
func Restore(ctx context.Context, t Target, path string, tables []string) error {
	if err := DropTables(ctx, t, tables); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	cmd := exec.CommandContext(ctx, "mysql", append(t.args(), t.Schema)...)
	cmd.Env = t.env()
	cmd.Stdin = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("復元に失敗: %w\n%s", err, firstLines(stderr.String(), 3))
	}
	return nil
}

// DropTables は復元先の該当表を落とす（前の中身を残さない）。
//
// DROP DATABASE を使わないのは、テーブル単位の権限しか無い利用者
// （本番でよくある構成）でも復元できるようにするため。
// 外部キーの順序を気にせず落とせるよう、一時的に foreign_key_checks を切る。
func DropTables(ctx context.Context, t Target, tables []string) error {
	var b strings.Builder
	b.WriteString("SET FOREIGN_KEY_CHECKS=0;")
	for _, tbl := range tables {
		fmt.Fprintf(&b, "DROP TABLE IF EXISTS `%s`;", tbl)
	}
	b.WriteString("SET FOREIGN_KEY_CHECKS=1;")
	cmd := exec.CommandContext(ctx, "mysql", append(t.args(), t.Schema, "-e", b.String())...)
	cmd.Env = t.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("復元先の表を落とせない: %w\n%s", err, out)
	}
	return nil
}

// ---- 壊し方 ----

// Truncate はバックアップの末尾を削る（転送が途中で切れた、ディスクが足りなかった）。
func Truncate(path string, keepRatio float64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.Truncate(path, int64(float64(fi.Size())*keepRatio))
}

// Corrupt はバックアップの中ほどのバイトを書き換える（転送中の破損）。
func Corrupt(path string, at float64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return fmt.Errorf("空のファイル")
	}
	i := int(float64(len(b)) * at)
	b[i] ^= 0xFF
	return os.WriteFile(path, b, 0o644)
}

// ---- 突き合わせ ----

// Fingerprint は「復元できたか」を判定するための指紋。
//
// **行数では判定しない。** 行数が同じで中身が違う復元は、いちばん危ない失敗。
type Fingerprint struct {
	Schema  string            `json:"schema"`  // 表と列の定義のハッシュ
	Tables  map[string]string `json:"tables"`  // 表ごとの中身のハッシュ
	Rows    map[string]int64  `json:"rows"`    // 表ごとの行数（参考。判定には使わない）
	Applied []string          `json:"applied"` // schema_migrations の内容
}

// Equal は指紋が一致するか。差があれば理由を返す。
func (f Fingerprint) Equal(o Fingerprint) (bool, []string) {
	var diff []string
	if f.Schema != o.Schema {
		diff = append(diff, fmt.Sprintf("スキーマが違う（%s → %s）", short(f.Schema), short(o.Schema)))
	}
	names := map[string]bool{}
	for n := range f.Tables {
		names[n] = true
	}
	for n := range o.Tables {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		a, okA := f.Tables[n]
		b, okB := o.Tables[n]
		switch {
		case !okB:
			diff = append(diff, fmt.Sprintf("表 %s が復元後に無い", n))
		case !okA:
			diff = append(diff, fmt.Sprintf("表 %s が復元後に増えている", n))
		case a != b:
			diff = append(diff, fmt.Sprintf("表 %s の中身が違う（行数 %d → %d）", n, f.Rows[n], o.Rows[n]))
		}
	}
	if strings.Join(f.Applied, ",") != strings.Join(o.Applied, ",") {
		diff = append(diff, fmt.Sprintf("適用済みマイグレーションが違う（%v → %v）", f.Applied, o.Applied))
	}
	return len(diff) == 0, diff
}

// Take は指紋を取る。tables は突き合わせる表（順序は問わない）。
func Take(ctx context.Context, db *sql.DB, tables []string) (Fingerprint, error) {
	f := Fingerprint{Tables: map[string]string{}, Rows: map[string]int64{}}

	s, err := schemaHash(ctx, db, tables)
	if err != nil {
		return f, err
	}
	f.Schema = s

	for _, t := range tables {
		h, n, err := tableHash(ctx, db, t)
		if err != nil {
			return f, fmt.Errorf("表 %s: %w", t, err)
		}
		f.Tables[t] = h
		f.Rows[t] = n
	}

	rows, err := db.QueryContext(ctx,
		"SELECT version, name, checksum, state FROM schema_migrations ORDER BY version")
	if err != nil {
		return f, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v int
		var name, sum, state string
		if err := rows.Scan(&v, &name, &sum, &state); err != nil {
			return f, err
		}
		f.Applied = append(f.Applied, fmt.Sprintf("%04d_%s:%.8s:%s", v, name, sum, state))
	}
	return f, rows.Err()
}

// schemaHash は表と列の定義を畳む。
//
// ★schema_migrations が「0003 まで当たっている」と言っていても、
// 実際の表の形が違うことはある（誰かが手で直した／復元が中途半端だった）。
// **記録ではなく実物を見る。**
func schemaHash(ctx context.Context, db *sql.DB, tables []string) (string, error) {
	// ★schema hash に含める列: 型だけでなく、照合順序・文字コード・生成式まで。
	// これらが違えば「別スキーマ」として扱う（HashRules() 参照）。
	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name, column_type,
			is_nullable, COALESCE(column_default, ''), COALESCE(column_key, ''), COALESCE(extra, ''),
			COALESCE(character_set_name, ''), COALESCE(collation_name, ''),
			COALESCE(generation_expression, '')
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name IN (`+placeholders(len(tables))+`)
		ORDER BY table_name, column_name`, toAny(tables)...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	for rows.Next() {
		var vals [10]string
		if err := rows.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[4], &vals[5], &vals[6],
			&vals[7], &vals[8], &vals[9]); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00", strings.Join(vals[:], "\x01"))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tableHash は1つの表の中身を畳む。
//
// 列の順序に依存しないよう、information_schema の順で並べ直してから読む。
// NULL と空文字を区別する（"\x00NULL" と "" は別の文字列になる）。
func tableHash(ctx context.Context, db *sql.DB, table string) (string, int64, error) {
	cols, err := columnsOf(ctx, db, table)
	if err != nil {
		return "", 0, err
	}
	if len(cols) == 0 {
		return "", 0, fmt.Errorf("表が無い（または列が無い）")
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "`" + c + "`"
	}
	list := strings.Join(quoted, ", ")
	//nolint:gosec // 表名・列名は information_schema から取ったもの（外から来た文字列ではない）
	rows, err := db.QueryContext(ctx,
		"SELECT "+list+" FROM `"+table+"` ORDER BY "+list)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	var n int64
	buf := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range buf {
		ptrs[i] = &buf[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", 0, err
		}
		n++
		for _, v := range buf {
			switch x := v.(type) {
			case nil:
				h.Write([]byte("\x00NULL"))
			case []byte:
				h.Write([]byte("\x00"))
				h.Write(x)
			default:
				fmt.Fprintf(h, "\x00%v", x)
			}
		}
		h.Write([]byte("\x02"))
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func columnsOf(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? ORDER BY column_name`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n == 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
