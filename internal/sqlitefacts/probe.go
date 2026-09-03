package sqlitefacts

// 同じ問いを MySQL と SQLite に投げて、観測値を突き合わせる。
//
// ★分類は「知識」ではなく**観測値の一致・不一致**で決める。
// 片方が測れなかったら SAME とは言わず UNVERIFIED を返す。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Class は移植判断のための分類（実験指示書の4分類）。
type Class string

const (
	// SameSemantics: 同じコードで同じ結果。そのまま持ち込める。
	SameSemantics Class = "SAME_SEMANTICS"
	// DifferentMechanism: 結果は同じにできるが、**書き方を変える必要がある**。
	DifferentMechanism Class = "DIFFERENT_MECHANISM"
	// DifferentResult: 同じコードで**違う結果**。持ち込むと壊れる。
	DifferentResult Class = "DIFFERENT_RESULT"
	// Unverified: 片方（または両方）を測れていない。
	Unverified Class = "UNVERIFIED"
)

// Probe は1つの問い。
//
// Kind は「もし観測値が食い違ったら、それは書き方の違いなのか結果の違いなのか」を
// あらかじめ決めておくもの。結果を見てから分類を選べないようにするため、
// Probe の定義（＝仮説）とテスト実行は分けてある。
type Probe struct {
	ID   string
	Ask  string // 何を聞いているか
	Kind Class  // 食い違ったときの分類（SameSemantics なら「食い違わないはず」）

	// MySQL / SQLite は、同じ問いに対する観測値を文字列で返す。
	// 文字列にするのは、突き合わせを機械にやらせるため。
	MySQL  func(ctx context.Context, db *sql.DB) (string, error)
	SQLite func(ctx context.Context, db *sql.DB) (string, error)

	// Why は、食い違ったときに何が壊れるか（移植する人が読む部分）。
	Why string
}

// Observation は1つの問いの結果。
type Observation struct {
	ID        string `json:"id"`
	Ask       string `json:"ask"`
	MySQL     string `json:"mysql"`
	SQLite    string `json:"sqlite"`
	Class     Class  `json:"class"`
	Why       string `json:"why,omitempty"`
	MySQLErr  string `json:"mysql_err,omitempty"`
	SQLiteErr string `json:"sqlite_err,omitempty"`
}

// Line は Markdown / ログ用の1行。
func (o Observation) Line() string {
	return fmt.Sprintf("[%s] %s | MySQL: %s | SQLite: %s", o.Class, o.ID, o.MySQL, o.SQLite)
}

// Run は問いを両方のエンジンへ投げて分類する。
//
// my が nil（MYSQL_DSN 未設定）なら MySQL 側は測れないので UNVERIFIED。
// **「SQLite で動いたから同じ」とは絶対に言わない。**
func (p Probe) Run(ctx context.Context, my, lite *sql.DB) Observation {
	o := Observation{ID: p.ID, Ask: p.Ask, Why: p.Why}

	if my == nil {
		o.MySQL, o.MySQLErr = "(未測定)", "MYSQL_DSN 未設定"
	} else if v, err := p.MySQL(ctx, my); err != nil {
		o.MySQL, o.MySQLErr = "(失敗)", err.Error()
	} else {
		o.MySQL = v
	}

	if lite == nil {
		o.SQLite, o.SQLiteErr = "(未測定)", "SQLite を開けなかった"
	} else if v, err := p.SQLite(ctx, lite); err != nil {
		o.SQLite, o.SQLiteErr = "(失敗)", err.Error()
	} else {
		o.SQLite = v
	}

	switch {
	case o.MySQLErr != "" || o.SQLiteErr != "":
		o.Class = Unverified
	case o.MySQL == o.SQLite:
		o.Class = SameSemantics
	default:
		o.Class = p.Kind
		if o.Class == SameSemantics {
			// 「食い違わないはず」の問いで食い違った ＝ 仮説の誤り。
			// 勝手に分類を書き換えず、DIFFERENT_RESULT として目立たせる。
			o.Class = DifferentResult
			o.Why = "★仮説では一致するはずだった。" + o.Why
		}
	}
	return o
}

// ---- 個々の問い ----

// Probes は移植判断に効く問いの一覧。
//
// 選んだ基準: **このリポジトリの実験（EXP-1..EXP-9）で「これが効く」と分かったもの**。
// 一般的な差分表を写すのではなく、自分たちが依存している性質だけを聞く。
func Probes() []Probe {
	return []Probe{
		{
			ID:   "affected-rows-noop",
			Ask:  "同じ値へ UPDATE したとき、影響行数はいくつか",
			Kind: DifferentResult,
			Why: "repo.Expect（EXP-2 の fencing、楽観ロック）は影響行数で持ち主を判定している。" +
				"MySQL は既定で『実際に変わった行数』を返すので 0 になり、" +
				"『自分が持ち主でない』と誤判定する（EXP-2 で踏んだ罠。clientFoundRows=true で回避した）。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchMySQL(ctx, db); err != nil {
					return "", err
				}
				return affectedNoop(ctx, db, "UPDATE exp10_scratch SET v = 1 WHERE k = 'a'")
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchSQLite(ctx, db); err != nil {
					return "", err
				}
				return affectedNoop(ctx, db, "UPDATE exp10_scratch SET v = 1 WHERE k = 'a'")
			},
		},
		{
			ID:   "upsert-affected-rows",
			Ask:  "UPSERT の影響行数は、挿入・更新・変化なしでそれぞれいくつか",
			Kind: DifferentResult,
			Why: "MySQL の ON DUPLICATE KEY UPDATE は 挿入=1 / 更新=2 / 変化なし=0 を返す。" +
				"『2 なら更新だった』という判定を書いていると、SQLite ではすべて 1 で常に『挿入』に見える。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchMySQL(ctx, db); err != nil {
					return "", err
				}
				return upsertCounts(ctx, db,
					"INSERT INTO exp10_scratch (k, v) VALUES (?, ?) AS new ON DUPLICATE KEY UPDATE v = new.v")
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchSQLite(ctx, db); err != nil {
					return "", err
				}
				return upsertCounts(ctx, db,
					"INSERT INTO exp10_scratch (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v")
			},
		},
		{
			ID:   "type-affinity",
			Ask:  "整数列へ 'abc' を入れるとどうなるか",
			Kind: DifferentResult,
			Why: "SQLite は型宣言を**助言**として扱う（型親和性）。文字列がそのまま入る。" +
				"MySQL は厳格モードで拒否する。『列の型で守られている』という前提が SQLite では成り立たない。",
			MySQL:  func(ctx context.Context, db *sql.DB) (string, error) { return typeAffinity(ctx, db, scratchMySQL) },
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) { return typeAffinity(ctx, db, scratchSQLite) },
		},
		{
			ID:   "ddl-rollback",
			Ask:  "トランザクションの中で DDL を流して ROLLBACK したら、表は消えるか",
			Kind: DifferentMechanism,
			Why: "MySQL の DDL は暗黙にコミットされる（EXP-6 のマイグレーション事故の原因）。" +
				"SQLite の DDL はトランザクションに入るので、『マイグレーション全体を1つのトランザクション』にできる。" +
				"つまり EXP-6 で必要だった『段階の記録』は、SQLite では別の形になる。",
			MySQL:  func(ctx context.Context, db *sql.DB) (string, error) { return ddlRollback(ctx, db, "ENGINE=InnoDB") },
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) { return ddlRollback(ctx, db, "") },
		},
		{
			ID:   "foreign-keys-default",
			Ask:  "何も設定しないとき、外部キーは検査されるか",
			Kind: DifferentMechanism,
			Why: "SQLite の外部キー検査は既定で OFF、しかも**接続ごとの設定**。" +
				"DSN ではなく db.Exec(\"PRAGMA foreign_keys=ON\") で入れると、" +
				"プールの中の1本にしか効かない（Accident_接続ごとのPRAGMA で再現した）。",
			MySQL:  func(ctx context.Context, db *sql.DB) (string, error) { return fkEnforced(ctx, db, true) },
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) { return fkEnforced(ctx, db, false) },
		},
		{
			ID:   "empty-where-update",
			Ask:  "WHERE の無い UPDATE は止まるか",
			Kind: DifferentMechanism,
			Why: "MySQL には sql_safe_updates があるが、SQLite には無い。" +
				"『間違えて全件更新』を止める仕組みが1つ減るので、" +
				"repo の文字列検査（EXP-8）と Expect による影響行数の確認の比重が上がる。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchMySQL(ctx, db); err != nil {
					return "", err
				}
				if _, err := db.ExecContext(ctx, "SET SESSION sql_safe_updates = 1"); err != nil {
					return "", err
				}
				defer func() { _, _ = db.ExecContext(ctx, "SET SESSION sql_safe_updates = 0") }()
				return blockedOrNot(ctx, db, "UPDATE exp10_scratch SET v = 99")
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				if err := scratchSQLite(ctx, db); err != nil {
					return "", err
				}
				return blockedOrNot(ctx, db, "UPDATE exp10_scratch SET v = 99")
			},
		},
		{
			ID:   "append-only-trigger",
			Ask:  "追記専用の表を、トリガで守れるか",
			Kind: DifferentMechanism,
			Why: "止められること自体は同じだが、書き方が違う（MySQL は SIGNAL、SQLite は RAISE(ABORT)）。" +
				"『トリガで守る』方針は持ち込めるが、DDL は書き直しになる。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				return appendOnlyMySQL(ctx, db)
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				return appendOnly(ctx, db, sqliteAppendOnlyDDL, "")
			},
		},
		{
			ID:   "advisory-lock",
			Ask:  "接続に紐づく名前つきロック（GET_LOCK 相当）はあるか",
			Kind: DifferentMechanism,
			Why: "EXP-6 のマイグレーションは GET_LOCK で『同時に1つだけ』を作っている（docs/locking.md）。" +
				"SQLite には相当物が無い。代わりに**書き込みロックがデータベース全体で1つ**なので、" +
				"BEGIN IMMEDIATE を取った側だけが進む、という別の作り方になる。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				var got sql.NullInt64
				if err := db.QueryRowContext(ctx, "SELECT GET_LOCK('exp10_probe', 0)").Scan(&got); err != nil {
					return "", err
				}
				defer func() { _, _ = db.ExecContext(ctx, "SELECT RELEASE_LOCK('exp10_probe')") }()
				return fmt.Sprintf("あり（GET_LOCK=%d）", got.Int64), nil
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				var got sql.NullInt64
				err := db.QueryRowContext(ctx, "SELECT GET_LOCK('exp10_probe', 0)").Scan(&got)
				if err == nil {
					return fmt.Sprintf("あり（GET_LOCK=%d）", got.Int64), nil
				}
				return "なし（GET_LOCK という関数が無い）", nil
			},
		},
		{
			ID:   "returning",
			Ask:  "UPDATE ... RETURNING は使えるか",
			Kind: DifferentResult,
			Why: "『更新して、更新後の行を1往復で取る』が SQLite では書ける。MySQL 8.0 では書けない。" +
				"SQLite 前提で書くと MySQL へ戻せなくなる（片道の移植になる）。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				return returningSupported(ctx, db, scratchMySQL)
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				return returningSupported(ctx, db, scratchSQLite)
			},
		},
		{
			ID:   "datetime-type",
			Ask:  "『時刻の列』に入れた値は、読み戻すと何になるか",
			Kind: DifferentMechanism,
			Why: "SQLite に日時型は無い（TEXT / INTEGER / REAL のいずれかに落ちる）。" +
				"MySQL の DATETIME(3) と同じ精度・同じ比較ができるかは、入れ方しだいで変わる。" +
				"lease の期限比較（EXP-2）は文字列比較になるので、必ず同じ書式で入れる必要がある。",
			MySQL: func(ctx context.Context, db *sql.DB) (string, error) {
				return datetimeRoundTrip(ctx, db, "DATETIME(3)", "'2026-01-02 03:04:05.678'")
			},
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) {
				return datetimeRoundTrip(ctx, db, "DATETIME", "'2026-01-02 03:04:05.678'")
			},
		},
		{
			ID:   "datetime-mixed-format",
			Ask:  "Go の time.Time で入れた値と、SQL のリテラルで入れた値を大小比較できるか",
			Kind: DifferentResult,
			Why: "lease の期限判定（EXP-2）は `WHERE expires_at < ?` の形。" +
				"SQLite に日時型は無く、比較は**入っている文字列の辞書順**になる。" +
				"入れ方が2通り混ざると、`2026-01-02 03:04:05.678` と " +
				"`2026-01-02T03:04:05.678Z` が同じ時刻として比較されない。",
			MySQL:  func(ctx context.Context, db *sql.DB) (string, error) { return mixedTime(ctx, db, "DATETIME(3)") },
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) { return mixedTime(ctx, db, "DATETIME") },
		},
		{
			ID:   "datetime-text-column",
			Ask:  "列の宣言を『時刻』にしないと、同じことができるか",
			Kind: DifferentResult,
			Why: "SQLite の列の型は助言でしかないが、**ドライバはその宣言を見て time.Time へ変換している**。" +
				"つまり『時刻として扱われるか』は列の宣言しだいで変わる。" +
				"MySQL では VARCHAR に入れても入れた文字列がそのまま返るだけで、変換は起きない。",
			MySQL:  func(ctx context.Context, db *sql.DB) (string, error) { return mixedTime(ctx, db, "VARCHAR(32)") },
			SQLite: func(ctx context.Context, db *sql.DB) (string, error) { return mixedTime(ctx, db, "TEXT") },
		},
	}
}

// ---- 問いの中身（両エンジンで共通の手順にする） ----

func scratchMySQL(ctx context.Context, db *sql.DB) error {
	return scratch(ctx, db,
		"CREATE TABLE exp10_scratch (k VARCHAR(16) NOT NULL PRIMARY KEY, v INT NOT NULL)")
}

func scratchSQLite(ctx context.Context, db *sql.DB) error {
	return scratch(ctx, db,
		"CREATE TABLE exp10_scratch (k TEXT NOT NULL PRIMARY KEY, v INTEGER NOT NULL)")
}

func scratch(ctx context.Context, db *sql.DB, create string) error {
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS exp10_scratch"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, create); err != nil {
		return err
	}
	//smlint:allow rowsaffected 理由: 実験用の初期行を入れるだけ。失敗すれば err で分かる
	_, err := db.ExecContext(ctx, "INSERT INTO exp10_scratch (k, v) VALUES ('a', 1)")
	return err
}

func affectedNoop(ctx context.Context, db *sql.DB, update string) (string, error) {
	res, err := db.ExecContext(ctx, update)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d 行", n), nil
}

//smlint:allow rowsaffected 理由: 後片付けの DELETE。消える行が 0 でも正しい
//smlint:allow loopquery 理由: 挿入・更新・変化なしの 3 回を順に打つのが問いそのもの
func upsertCounts(ctx context.Context, db *sql.DB, upsert string) (string, error) {
	if _, err := db.ExecContext(ctx, "DELETE FROM exp10_scratch WHERE k = 'u'"); err != nil {
		return "", err
	}
	var out []string
	for _, c := range []struct {
		label string
		v     int
	}{{"挿入", 1}, {"更新", 2}, {"変化なし", 2}} {
		res, err := db.ExecContext(ctx, upsert, "u", c.v)
		if err != nil {
			return "", err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", err
		}
		out = append(out, fmt.Sprintf("%s=%d", c.label, n))
	}
	return strings.Join(out, " "), nil
}

func typeAffinity(ctx context.Context, db *sql.DB, setup func(context.Context, *sql.DB) error) (string, error) {
	if err := setup(ctx, db); err != nil {
		return "", err
	}
	//smlint:allow rowsaffected 理由: 入るか拒否されるかを見る問い。行数ではなくエラーの有無を見ている
	if _, err := db.ExecContext(ctx, "INSERT INTO exp10_scratch (k, v) VALUES ('t', 'abc')"); err != nil {
		return "拒否される: " + firstLine(err.Error()), nil
	}
	var v any
	if err := db.QueryRowContext(ctx, "SELECT v FROM exp10_scratch WHERE k = 't'").Scan(&v); err != nil {
		return "", err
	}
	return fmt.Sprintf("入る（読み戻すと %q）", fmt.Sprint(asString(v))), nil
}

func ddlRollback(ctx context.Context, db *sql.DB, suffix string) (string, error) {
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS exp10_ddl"); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "CREATE TABLE exp10_ddl (id INT PRIMARY KEY) "+suffix); err != nil {
		_ = tx.Rollback()
		return "", err
	}
	if err := tx.Rollback(); err != nil {
		return "", err
	}
	var n int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM exp10_ddl").Scan(&n)
	if err != nil {
		return "ROLLBACK で消えた（DDL はトランザクションに入る）", nil
	}
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS exp10_ddl")
	return "ROLLBACK しても残る（DDL は暗黙にコミットされる）", nil
}

func fkEnforced(ctx context.Context, db *sql.DB, mysql bool) (string, error) {
	for _, s := range []string{"DROP TABLE IF EXISTS exp10_child", "DROP TABLE IF EXISTS exp10_parent"} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return "", err
		}
	}
	engine := ""
	if mysql {
		engine = " ENGINE=InnoDB"
	}
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE exp10_parent (id INT NOT NULL PRIMARY KEY)"+engine); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE exp10_child (id INT NOT NULL PRIMARY KEY, pid INT NOT NULL, "+
			"FOREIGN KEY (pid) REFERENCES exp10_parent(id))"+engine); err != nil {
		return "", err
	}
	//smlint:allow rowsaffected 理由: 拒否されるかどうかを見る問い。行数ではなくエラーの有無を見ている
	_, err := db.ExecContext(ctx, "INSERT INTO exp10_child (id, pid) VALUES (1, 999)")
	if err != nil {
		return "検査される（親の無い行は拒否）", nil
	}
	return "検査されない（親の無い行が入る）", nil
}

func blockedOrNot(ctx context.Context, db *sql.DB, stmt string) (string, error) {
	//smlint:allow rowsaffected 理由: 止まるかどうかを見る問い。行数ではなくエラーの有無を見ている
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return "止まる: " + firstLine(err.Error()), nil
	}
	return "止まらない（全件更新が通る）", nil
}

const mysqlAppendOnlyDDL = `CREATE TRIGGER exp10_no_update BEFORE UPDATE ON exp10_log
FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '追記専用の表は更新できない'`

const sqliteAppendOnlyDDL = `CREATE TRIGGER exp10_no_update BEFORE UPDATE ON exp10_log
BEGIN SELECT RAISE(ABORT, '追記専用の表は更新できない'); END`

func appendOnly(ctx context.Context, db *sql.DB, triggerDDL, engine string) (string, error) {
	for _, s := range []string{"DROP TRIGGER IF EXISTS exp10_no_update", "DROP TABLE IF EXISTS exp10_log"} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return "", err
		}
	}
	create := "CREATE TABLE exp10_log (id INT NOT NULL PRIMARY KEY, body TEXT NOT NULL)"
	if engine != "" {
		create += " ENGINE=" + engine
	}
	if _, err := db.ExecContext(ctx, create); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, triggerDDL); err != nil {
		return "トリガを作れない: " + firstLine(err.Error()), nil
	}
	//smlint:allow rowsaffected 理由: 実験用の初期行
	if _, err := db.ExecContext(ctx, "INSERT INTO exp10_log (id, body) VALUES (1, 'x')"); err != nil {
		return "", err
	}
	//smlint:allow rowsaffected 理由: 止まるかどうかを見る問い
	if _, err := db.ExecContext(ctx, "UPDATE exp10_log SET body = 'y' WHERE id = 1"); err != nil {
		return "守れる（更新が拒否される）", nil
	}
	return "守れない（更新が通ってしまう）", nil
}

func returningSupported(ctx context.Context, db *sql.DB, setup func(context.Context, *sql.DB) error) (string, error) {
	if err := setup(ctx, db); err != nil {
		return "", err
	}
	var v int
	err := db.QueryRowContext(ctx, "UPDATE exp10_scratch SET v = v + 1 WHERE k = 'a' RETURNING v").Scan(&v)
	if err != nil {
		return "使えない: " + firstLine(err.Error()), nil
	}
	return fmt.Sprintf("使える（更新後の値 %d が1往復で取れる）", v), nil
}

func datetimeRoundTrip(ctx context.Context, db *sql.DB, colType, lit string) (string, error) {
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS exp10_dt"); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE exp10_dt (id INT NOT NULL PRIMARY KEY, at "+colType+" NOT NULL)"); err != nil {
		return "", err
	}
	//smlint:allow rowsaffected 理由: 実験用の初期行
	if _, err := db.ExecContext(ctx, "INSERT INTO exp10_dt (id, at) VALUES (1, "+lit+")"); err != nil {
		return "", err
	}
	var raw any
	if err := db.QueryRowContext(ctx, "SELECT at FROM exp10_dt WHERE id = 1").Scan(&raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%T %q", raw, asString(raw)), nil
}

// appendOnlyMySQL は MySQL でトリガを作る。
//
// binlog が有効で SUPER が無いと CREATE TRIGGER は 1419 で拒否される。
// ★ここで「作れなかった」を観測値として返すと、権限の問題が
// 「MySQL では守れない」という**嘘の結論**になる。だからエラーとして返し、
// 分類を UNVERIFIED に落とす。設定を変えられるなら変えてから測り直す。
func appendOnlyMySQL(ctx context.Context, db *sql.DB) (string, error) {
	v, err := appendOnly(ctx, db, mysqlAppendOnlyDDL, "InnoDB")
	if err != nil || !strings.HasPrefix(v, "トリガを作れない") {
		return v, err
	}
	// log_bin_trust_function_creators を一時的に立てて測り直す（測れたら元へ戻す）。
	var prev int
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.log_bin_trust_function_creators").Scan(&prev); err != nil {
		return "", fmt.Errorf("トリガを作れず、設定も読めない: %s", v)
	}
	if _, err := db.ExecContext(ctx, "SET GLOBAL log_bin_trust_function_creators = 1"); err != nil {
		return "", fmt.Errorf("トリガを作れず、権限も足りない（測定不能）: %s", v)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, fmt.Sprintf("SET GLOBAL log_bin_trust_function_creators = %d", prev))
	}()
	v2, err := appendOnly(ctx, db, mysqlAppendOnlyDDL, "InnoDB")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(v2, "トリガを作れない") {
		return "", fmt.Errorf("トリガを作れない（測定不能）: %s", v2)
	}
	return v2, nil
}

// mixedTime は「入れ方が2通り混ざったときに比較できるか」を見る。
func mixedTime(ctx context.Context, db *sql.DB, colType string) (string, error) {
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS exp10_mt"); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE exp10_mt (id INT NOT NULL PRIMARY KEY, at "+colType+" NOT NULL)"); err != nil {
		return "", err
	}
	early := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// ① Go の time.Time を束縛して入れる（ドライバが書式を決める）
	//smlint:allow rowsaffected 理由: 実験用の初期行
	if _, err := db.ExecContext(ctx, "INSERT INTO exp10_mt (id, at) VALUES (1, ?)", early); err != nil {
		return "", err
	}
	// ② SQL のリテラルで、①より1時間あとを入れる
	//smlint:allow rowsaffected 理由: 実験用の初期行
	if _, err := db.ExecContext(ctx,
		"INSERT INTO exp10_mt (id, at) VALUES (2, '2026-01-02 04:04:05')"); err != nil {
		return "", err
	}
	// ③ ①と**同じ時刻**を、SQL のリテラルで入れる。
	// 「同じ時刻なのに一致しない」が起きるのはここ。
	//smlint:allow rowsaffected 理由: 実験用の初期行
	if _, err := db.ExecContext(ctx,
		"INSERT INTO exp10_mt (id, at) VALUES (3, '2026-01-02 03:04:05')"); err != nil {
		return "", err
	}
	var after, same int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exp10_mt WHERE at > ?", early).Scan(&after); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exp10_mt WHERE at = ?", early).Scan(&same); err != nil {
		return "", err
	}
	var raw1, raw2 any
	if err := db.QueryRowContext(ctx, "SELECT at FROM exp10_mt WHERE id = 1").Scan(&raw1); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx, "SELECT at FROM exp10_mt WHERE id = 2").Scan(&raw2); err != nil {
		return "", err
	}
	return fmt.Sprintf("あと=%d件（正=1） 同時刻=%d件（正=2） 格納形 %q / %q",
		after, same, asString(raw1), asString(raw2)), nil
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}
