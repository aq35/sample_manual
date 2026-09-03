package sqlitefacts_test

// EXP-10: SQLite companion。
//
//	MYSQL_DSN=... go test ./internal/sqlitefacts/ -run TestEXP10 -v
//
// 問いは「SQLite でも動くか」ではなく、
// **MySQL で確かめた結論のうち、どれをそのまま持ち込めるか**。
//
// ★このファイルの禁じ手: MySQL の結論を SQLite の結論として流用すること。
// 同じ問いを両方へ同じ条件で投げ、観測値が食い違ったところだけを「違う」と言う。
// MYSQL_DSN が無い環境では MySQL 側は測れないので、SAME とは言わず UNVERIFIED になる。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/sqlitefacts"
)

const tenant = "exp10"

// hypothesis は**結果を見る前に**書いたもの。
// 反証されたものは書き換えず、「反証された」と記録する。
var hypothesis = strings.Join([]string{
	"1) pure Go 版と cgo 版のドライバは、同じ SQL・同じ PRAGMA に対して同じ観測値を返す",
	"（違いは速度と配布のしやすさだけ）。",
	"2) 書き込みはデータベース全体で1つずつしか進まない。MaxOpenConns を増やしても",
	"書き込みスループットは上がらず、SQLITE_BUSY が増えるだけ。",
	"3) 既定の journal_mode=delete では読み手と書き手が互いを止める。WAL にすると止めなくなる。",
	"4) MySQL で確かめた結論のうち、影響行数・UPSERT の戻り値・型の厳格さ・",
	"DDL のトランザクション性・外部キーの既定は、そのままでは持ち込めない。",
	"5) GET_LOCK 相当は無い。マイグレーションの排他は BEGIN IMMEDIATE で作れて、",
	"しかも DDL をロールバックできるぶん EXP-6 より単純になる。",
	"6) WAL のまま .db だけをコピーしたバックアップは、コミット済みのデータを失う。",
	"VACUUM INTO なら失わない。",
	"7) PRAGMA を db.Exec で入れると、プールの中の一部の接続にしか効かない。",
	"8) 別プロセス3本が同じファイルを書くと、busy_timeout=0 では SQLITE_BUSY が大量に出る。",
	"WAL と busy_timeout を両方入れると 0 になる。",
	"9) synchronous を下げると書き込みは速くなるが、失うのは電源断への耐性であって",
	"プロセス死への耐性ではない。synchronous=OFF でも、コミットが返った直後に SIGKILL された",
	"程度ではコミット済みのデータは消えない。",
}, " ")

// dropProbeTables は問いのために作った表を片づける。
//
// 実験が MySQL 側に残骸を残すと、次の実験（EXP-11 のスキーマ指紋）が
// 「知らない表がある」と言い出す。実験の後片付けは実験の一部。
func dropProbeTables(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	for _, t := range []string{"exp10_scratch", "exp10_ddl", "exp10_child", "exp10_parent",
		"exp10_log", "exp10_dt", "exp10_mt"} {
		//smlint:allow loopquery 理由: 後片付け。固定の表名を順に落とすだけ
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+t)
	}
	_, _ = db.ExecContext(ctx, "DROP TRIGGER IF EXISTS exp10_no_update")
}

func openMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Log("MYSQL_DSN が未設定。MySQL 側は測れないので UNVERIFIED になる")
		return nil
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Logf("MySQL を開けない: %v", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Logf("MySQL に繋がらない: %v", err)
		_ = db.Close()
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEXP10_SQLiteCompanion(t *testing.T) {
	ctx := context.Background()
	my := openMySQL(t)
	t.Cleanup(func() { dropProbeTables(context.Background(), my) })

	rec := expkit.NewRecorder("EXP-10", "sqlite-companion",
		"MySQL で確かめた結論のうち、SQLite へ持ち込めるものと持ち込めないものを切り分ける")
	env := expkit.CaptureEnv(ctx, my)

	// ---- ドライバの素性を先に記録する（測定条件の一部） ----
	for _, d := range sqlitefacts.Drivers() {
		db, _, cleanup, err := d.OpenTemp(sqlitefacts.DefaultPragmas())
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		env.Notes = append(env.Notes,
			fmt.Sprintf("%s: sqlite %s / cgo=%v", d.Name, sqlitefacts.Version(ctx, db), d.CGO))
		cleanup()
	}
	rec.Env(env)
	rec.Freeze(hypothesis)

	rec.Workload("rows", 500).
		Workload("write_load", "1行更新のトランザクションを1秒").
		Workload("probes", len(sqlitefacts.Probes())).
		Injection("concurrency", "同一プロセス4本 / 別プロセス3本").
		Injection("backup", "WAL に載ったまま .db だけをコピーする")

	// 以降のサブテストは順番に依存しない。それぞれが rec に variant を足す。
	t.Run("問いを両エンジンへ", func(t *testing.T) { probeBoth(t, ctx, rec, my) })
	t.Run("ドライバ比較", func(t *testing.T) { driverCompare(t, ctx, rec) })
	t.Run("配布のしやすさ", func(t *testing.T) { distribution(t, rec) })
	t.Run("書き込みの並行度", func(t *testing.T) { writeConcurrency(t, ctx, rec) })
	t.Run("WALと読み書き同時", func(t *testing.T) { walReaders(t, ctx, rec) })
	t.Run("別プロセスの競合", func(t *testing.T) { multiProcess(t, ctx, rec) })
	t.Run("接続ごとのPRAGMA", func(t *testing.T) { pragmaPerConn(t, ctx, rec) })
	t.Run("遅延トランザクションの昇格", func(t *testing.T) { deferredUpgrade(t, ctx, rec) })
	t.Run("マイグレーションの途中失敗", func(t *testing.T) { migrationInTx(t, ctx, rec) })
	t.Run("synchronousの水準", func(t *testing.T) { synchronousLevels(t, ctx, rec) })
	t.Run("コミット後のプロセス死", func(t *testing.T) { commitThenDie(t, ctx, rec) })
	t.Run("バックアップと復元", func(t *testing.T) { backupRestore(t, ctx, rec) })

	rec.Scope(
		"SQLite "+sqliteVersions(ctx, t)+" / modernc.org/sqlite v1.34.5 / github.com/mattn/go-sqlite3 v1.14.24",
		"1台のホストのローカルファイル。ネットワークファイルシステム（NFS・EFS）は未検証",
		"MySQL 側の観測値は、この実行と同じ時刻に同じ問いを投げて取ったもの（過去の結果を流用していない）",
	)
	rec.Uncertain(
		"**MySQL の結論を SQLite の結論として流用していない。** 逆も同じ",
		"ネットワーク越しのファイル（NFS/EFS/SMB）でのロックは未検証。SQLite 本家が推奨していない構成",
		"複数ホストからの同時書き込みは、この実験の対象外（SQLite では成り立たない前提）",
		"レプリケーション（litestream・LiteFS 等）は未検証",
		"暗号化・全文検索など拡張が要る機能は、pure Go 版と cgo 版で差が出る可能性がある（未測定）",
		"絶対値（ops/sec）はこのホストのものであって、engine 間の速度比較には使えない",
	)
	rec.Artifact(
		"internal/sqlitefacts: 同じ問いを両エンジンへ投げて分類する Probe（他プロジェクトの移植判断に流用できる）",
		"cmd/sqlitelab: 別プロセスから同じファイルを書く子プロセス",
		"cmd/sizeprobe: ドライバ1つぶんの実行ファイルの大きさとクロスコンパイルの可否を測る最小の main",
	)
	rec.Next("EXP-11 backup / restore / corruption")

	files, err := rec.Save(verdict)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

// verdict は最後に書く（結果を見てから書いてよいのはここだけ）。
var verdict = strings.Join([]string{
	"SQLite は「MySQL の小さい版」ではない。",
	"影響行数・UPSERT の戻り値・型の厳格さ・外部キーの既定・DDL のトランザクション性が違うため、",
	"repo 層の Expect（影響行数で持ち主を判定する仕組み）は書き換えないと持ち込めない。",
	"一方でマイグレーションは SQLite のほうが単純になる（DDL がロールバックできる）。",
	"詳細は docs/sqlite.md。",
}, "")

func sqliteVersions(ctx context.Context, t *testing.T) string {
	var out []string
	for _, d := range sqlitefacts.Drivers() {
		db, _, cleanup, err := d.OpenTemp(sqlitefacts.BarePragmas())
		if err != nil {
			continue
		}
		out = append(out, d.Name+"="+sqlitefacts.Version(ctx, db))
		cleanup()
	}
	return strings.Join(out, " ")
}

// ---- ① 同じ問いを両方へ ----

func probeBoth(t *testing.T, ctx context.Context, rec *expkit.Recorder, my *sql.DB) {
	// ドライバごとに観測値を集める。仮説1（ドライバが違っても観測値は同じ）を
	// 「同じだろう」で済ませず、両方に同じ問いを投げて突き合わせる。
	byDriver := map[string]map[string]string{}
	var observations []sqlitefacts.Observation

	for _, d := range sqlitefacts.Drivers() {
		lite, _, cleanup, err := d.OpenTemp(sqlitefacts.DefaultPragmas())
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		// 外部キーの既定を見る問いのために、素のプラグマでも1つ開ける
		bare, _, cleanupBare, err := d.OpenTemp(sqlitefacts.BarePragmas())
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}

		vals := map[string]string{}
		for _, p := range sqlitefacts.Probes() {
			db := lite
			if p.ID == "foreign-keys-default" {
				db = bare // 「何も設定しないとき」を聞いているので、設定していないほうへ投げる
			}
			// MySQL 側は1回だけ測ればよい（同じ問い・同じ相手）。
			// 2周目は SQLite 側だけを取り直して、ドライバ間の比較に使う。
			mydb := my
			if len(observations) >= len(sqlitefacts.Probes()) {
				mydb = nil
			}
			o := p.Run(ctx, mydb, db)
			vals[p.ID] = o.SQLite
			if mydb != nil || my == nil {
				observations = append(observations, o)
			}
		}
		byDriver[d.Name] = vals
		cleanup()
		cleanupBare()
	}

	counts := map[string]int64{}
	var notes []string
	for _, o := range observations {
		counts[string(o.Class)]++
		notes = append(notes, o.Line())
		if o.MySQLErr != "" {
			notes = append(notes, "    MySQL 側: "+o.MySQLErr)
		}
		if o.SQLiteErr != "" {
			notes = append(notes, "    SQLite 側: "+o.SQLiteErr)
		}
		if o.Why != "" && o.Class != sqlitefacts.SameSemantics {
			notes = append(notes, "    → "+o.Why)
		}
		t.Log(o.Line())
	}

	rec.Add(expkit.Variant{
		Name:     "同じ問いを両エンジンへ投げた結果",
		Desc:     "分類は観測値の一致・不一致で機械的に決めている（知識で決めていない）",
		Counters: counts,
		Accident: counts[string(sqlitefacts.DifferentResult)] > 0,
		Notes:    notes,
	})

	// ---- 仮説1: ドライバが違っても観測値は同じか ----
	var diffs []string
	for _, p := range sqlitefacts.Probes() {
		a := byDriver[sqlitefacts.PureGo.Name][p.ID]
		b := byDriver[sqlitefacts.CGO.Name][p.ID]
		if a != b {
			diffs = append(diffs, fmt.Sprintf("%s: pure Go=%q / cgo=%q", p.ID, a, b))
		}
	}
	if diffs == nil {
		diffs = []string{"すべての問いで、2つのドライバの観測値が一致した"}
	}
	rec.Add(expkit.Variant{
		Name:     "ドライバを入れ替えて同じ問いを投げた（仮説1の検証）",
		Desc:     "modernc(pure Go) と mattn(cgo) に、同じ問いを同じ順で投げて突き合わせた",
		Counters: map[string]int64{"食い違った問い": int64(len(diffs)), "問いの総数": int64(len(sqlitefacts.Probes()))},
		Accident: len(diffs) > 0 && !strings.HasPrefix(diffs[0], "すべての問い"),
		Notes:    diffs,
	})
	for _, d := range diffs {
		t.Log("ドライバ差: " + d)
	}
}

// ---- ② ドライバ比較 ----

func driverCompare(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const rows = 500
	for _, d := range sqlitefacts.Drivers() {
		db, _, cleanup, err := sqlitefacts.TempDB(ctx, d, sqlitefacts.DefaultPragmas(), tenant, rows)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		db.SetMaxOpenConns(1) // 書き込みは1つずつしか進まないので、揃えて比べる
		w := sqlitefacts.WriteLoad(ctx, db, tenant, rows, 1, time.Second)
		r := sqlitefacts.ReadLoad(ctx, db, tenant, rows, 1, time.Second)
		cleanup()

		rec.Add(expkit.Variant{
			Name: "ドライバ: " + d.Name,
			Desc: fmt.Sprintf("MaxOpenConns=1 / WAL / synchronous=NORMAL / %d 行", rows),
			Counters: map[string]int64{
				"write_ops": w.Ops, "write_busy": w.Busy, "write_errs": w.Errs,
				"read_ops": r.Ops, "read_busy": r.Busy, "read_errs": r.Errs,
			},
			Metrics: map[string]float64{
				"write_per_sec": w.PerSec(), "read_per_sec": r.PerSec(),
				"write_p50_us": float64(w.Latency.P50.Microseconds()),
				"write_p99_us": float64(w.Latency.P99.Microseconds()),
				"read_p50_us":  float64(r.Latency.P50.Microseconds()),
				"read_p99_us":  float64(r.Latency.P99.Microseconds()),
			},
			Latency: &w.Latency,
			Notes: []string{
				"同じホスト・同じ条件で測ったドライバ間の比較。**MySQL との比較には使わない**",
			},
		})
		t.Logf("%s write=%.0f/s p50=%v read=%.0f/s p50=%v",
			d.Name, w.PerSec(), w.Latency.P50, r.PerSec(), r.Latency.P50)
	}
}

// ---- ③ 配布のしやすさ（大きさとクロスコンパイル） ----

func distribution(t *testing.T, rec *expkit.Recorder) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	type build struct {
		label   string
		env     []string
		tags    []string
		wantErr bool
	}
	builds := []build{
		{label: "pure Go / linux-amd64", env: []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}},
		{label: "pure Go / linux-arm64", env: []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64"}},
		{label: "pure Go / darwin-arm64", env: []string{"CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64"}},
		{label: "cgo / linux-amd64 (CGO_ENABLED=1)", env: []string{"CGO_ENABLED=1", "GOOS=linux", "GOARCH=amd64"}, tags: []string{"sqlite_cgo"}},
		{label: "cgo / linux-amd64 (CGO_ENABLED=0)", env: []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, tags: []string{"sqlite_cgo"}},
		{label: "cgo / linux-arm64 (CGO_ENABLED=1, クロス)", env: []string{"CGO_ENABLED=1", "GOOS=linux", "GOARCH=arm64"}, tags: []string{"sqlite_cgo"}},
	}

	counters := map[string]int64{}
	var notes []string
	for i, b := range builds {
		out := filepath.Join(dir, fmt.Sprintf("probe%d", i))
		args := []string{"build", "-o", out}
		if len(b.tags) > 0 {
			args = append(args, "-tags", strings.Join(b.tags, ","))
		}
		args = append(args, "./cmd/sizeprobe")
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), b.env...)
		buildOut, err := cmd.CombinedOutput()
		if err != nil {
			note := fmt.Sprintf("%s: **ビルドできない** — %s", b.label, firstLine(string(buildOut)))
			notes = append(notes, note)
			t.Log(note)
			continue
		}
		fi, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		mb := float64(fi.Size()) / (1 << 20)
		counters[b.label+" bytes"] = fi.Size()
		note := fmt.Sprintf("%s: ビルドできる / %.1f MiB", b.label, mb)

		// このホストで動かせるものは、実行までしてみる。
		// ★「ビルドが通った」を成功条件にしない。
		if strings.Contains(b.label, "linux-amd64") {
			run := exec.Command(out, filepath.Join(dir, "probe.db"))
			runOut, runErr := run.CombinedOutput()
			if runErr != nil {
				note += " / **実行すると失敗する**: " + firstLine(string(runOut))
			} else {
				note += " / 実行できる: " + firstLine(string(runOut))
			}
		}
		notes = append(notes, note)
		t.Log(note)
	}

	rec.Add(expkit.Variant{
		Name:     "配布のしやすさ（実行ファイルの大きさとクロスコンパイル）",
		Desc:     "cmd/sizeprobe（開いて sqlite_version() を1回聞くだけの main）を各条件でビルドした",
		Counters: counters,
		Notes:    notes,
	})
}

// ---- ④ 書き込みの並行度 ----

func writeConcurrency(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const rows = 500
	for _, c := range []struct {
		conc, maxOpen int
	}{{1, 1}, {4, 1}, {4, 4}, {8, 8}} {
		db, _, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, sqlitefacts.DefaultPragmas(), tenant, rows)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(c.maxOpen)
		db.SetMaxIdleConns(c.maxOpen)
		w := sqlitefacts.WriteLoad(ctx, db, tenant, rows, c.conc, time.Second)
		cleanup()

		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("書き込み %d 並行 / MaxOpenConns=%d", c.conc, c.maxOpen),
			Counters: map[string]int64{"ops": w.Ops, "busy": w.Busy, "errs": w.Errs},
			Metrics: map[string]float64{
				"per_sec": w.PerSec(),
				"p50_us":  float64(w.Latency.P50.Microseconds()),
				"p99_us":  float64(w.Latency.P99.Microseconds()),
			},
			Accident: w.Busy > 0 || w.Errs > 0,
		})
		t.Logf("conc=%d maxopen=%d ops=%d busy=%d errs=%d %.0f/s p99=%v",
			c.conc, c.maxOpen, w.Ops, w.Busy, w.Errs, w.PerSec(), w.Latency.P99)
	}
}

// ---- ⑤ WAL と「読みながら書く」 ----

func walReaders(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const rows = 500
	for _, mode := range []string{"delete", "WAL"} {
		p := sqlitefacts.DefaultPragmas()
		p.JournalMode = mode
		p.BusyTimeout = 500 * time.Millisecond // 待ちすぎて見えなくならないように短くする
		db, _, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, p, tenant, rows)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)

		// 書き手を1本回しながら、読み手を3本回す
		done := make(chan sqlitefacts.Throughput, 1)
		go func() { done <- sqlitefacts.WriteLoad(ctx, db, tenant, rows, 1, time.Second) }()
		r := sqlitefacts.ReadLoad(ctx, db, tenant, rows, 3, time.Second)
		w := <-done

		var actual string
		_ = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&actual)
		cleanup()

		rec.Add(expkit.Variant{
			Name: "journal_mode=" + actual + " で読みながら書く",
			Desc: "書き手 1 本・読み手 3 本を同時に1秒",
			Counters: map[string]int64{
				"write_ops": w.Ops, "write_busy": w.Busy, "write_errs": w.Errs,
				"read_ops": r.Ops, "read_busy": r.Busy, "read_errs": r.Errs,
			},
			Metrics: map[string]float64{
				"write_per_sec": w.PerSec(), "read_per_sec": r.PerSec(),
				"read_p99_us": float64(r.Latency.P99.Microseconds()),
			},
			Accident: w.Busy > 0 || r.Busy > 0,
		})
		t.Logf("journal=%s write=%d(busy %d) read=%d(busy %d) read_p99=%v",
			actual, w.Ops, w.Busy, r.Ops, r.Busy, r.Latency.P99)
	}
}

// ---- ⑥ 別プロセスの競合 ----

func multiProcess(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/sqlitelab")
	for _, c := range []struct {
		label string
		wal   bool
		busy  time.Duration
	}{
		{"WAL 無し・待たない（busy_timeout=0）", false, 0},
		{"WAL 有り・待たない（busy_timeout=0）", true, 0},
		{"WAL 有り・待つ（busy_timeout=5s）", true, 5 * time.Second},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "exp.db")

		// 先に1プロセスでスキーマを作る（3本が同時に CREATE すると、そこで競合してしまう）
		// ★bool フラグは -wal=false の形で渡す。
		// 最初 "-wal", "false" と2引数で渡していて、Go の flag はこれを
		// 「-wal=true と、位置引数 \"false\"」と解釈していた。
		// つまり **journal_mode=delete のつもりが全部 WAL** で、競合が1件も出なかった。
		// 子プロセスに journal_mode を報告させて、要求どおりかを検査するようにした。
		setup := exec.Command(bin, "-db", path, "-setup", "-dur=10ms",
			fmt.Sprintf("-wal=%v", c.wal), "-busy=5s")
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}

		var procs []*exec.Cmd
		var outs []*strings.Builder
		for i := 0; i < 3; i++ {
			var sb strings.Builder
			cmd := exec.Command(bin, "-db", path, "-dur=1s",
				fmt.Sprintf("-wal=%v", c.wal), "-busy="+c.busy.String())
			cmd.Stdout = &sb
			cmd.Stderr = &sb
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			procs = append(procs, cmd)
			outs = append(outs, &sb)
		}
		var ops, busy, errs int64
		var notes []string
		wantMode := "delete"
		if c.wal {
			wantMode = "wal"
		}
		for i, cmd := range procs {
			_ = cmd.Wait()
			line := strings.TrimSpace(outs[i].String())
			o, b, e := parseChild(line)
			ops, busy, errs = ops+o, busy+b, errs+e
			notes = append(notes, fmt.Sprintf("プロセス%d: %s", i+1, line))
			// 要求した journal_mode で動いていたかを検査する（前回はここが違っていた）。
			// 競合が激しいと PRAGMA journal_mode 自体が busy で読めず空になることがあるので、
			// 「別のモードだった」ときだけ落とす。
			if m := childJournal(line); m != "" && m != wantMode {
				t.Errorf("プロセス%d の journal_mode が %q（要求は %q）", i+1, m, wantMode)
			}
		}

		rec.Add(expkit.Variant{
			Name:     "別プロセス3本が同じファイルを書く / " + c.label,
			Desc:     "goroutine ではなく本物の別プロセス（OS のファイルロック越しに競合する）",
			Counters: map[string]int64{"ops": ops, "busy": busy, "errs": errs},
			Accident: busy > 0 || errs > 0,
			Notes:    notes,
		})
		t.Logf("%s → ops=%d busy=%d errs=%d", c.label, ops, busy, errs)
	}
}

// childJournal は子プロセスが報告した journal_mode。読めなければ空。
func childJournal(line string) string {
	const key = `"journal_mode":"`
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func parseChild(line string) (ops, busy, errs int64) {
	// 子プロセスは JSON を1行で出す。壊れていたら 0 のまま返す（数字をでっち上げない）。
	for _, kv := range []struct {
		key string
		dst *int64
	}{{`"ops":`, &ops}, {`"busy":`, &busy}, {`"errs":`, &errs}} {
		i := strings.Index(line, kv.key)
		if i < 0 {
			continue
		}
		rest := line[i+len(kv.key):]
		j := strings.IndexAny(rest, ",}")
		if j < 0 {
			continue
		}
		var v int64
		if _, err := fmt.Sscan(strings.TrimSpace(rest[:j]), &v); err == nil {
			*kv.dst = v
		}
	}
	return
}

// ---- ⑦ 接続ごとの PRAGMA ----

func pragmaPerConn(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const conns = 4

	// 事故: 起動時に db.Exec("PRAGMA foreign_keys=ON") を1回だけ呼ぶ
	bad, _, cleanupBad, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, sqlitefacts.BarePragmas(), tenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	onBad, totalBad, err := sqlitefacts.PragmaPerConn(ctx, bad, conns, true)
	cleanupBad()
	if err != nil {
		t.Fatal(err)
	}

	// 対策: DSN に書く（新しい接続すべてに当たる）
	good, _, cleanupGood, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, sqlitefacts.DefaultPragmas(), tenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	onGood, totalGood, err := sqlitefacts.PragmaPerConn(ctx, good, conns, false)
	cleanupGood()
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "PRAGMA を db.Exec で1回だけ入れた（事故）",
		Desc:     `起動時に db.Exec("PRAGMA foreign_keys = ON") を呼ぶ、よくある書き方`,
		Counters: map[string]int64{"設定が効いていた接続": int64(onBad), "調べた接続": int64(totalBad)},
		Accident: onBad < totalBad,
		Notes: []string{
			"PRAGMA は接続ごとの設定。*sql.DB はプールなので、そのとき使われた1本にしか効かない",
			"外部キー検査が効いている接続と効いていない接続が混ざる。" +
				"**どちらに当たるかは実行のたびに変わる**ので、テストでは通って本番で壊れる",
		},
	})
	rec.Add(expkit.Variant{
		Name:     "PRAGMA を DSN に書いた（対策）",
		Desc:     "file:...?_pragma=foreign_keys(1) — 新しい接続すべてに当たる",
		Counters: map[string]int64{"設定が効いていた接続": int64(onGood), "調べた接続": int64(totalGood)},
		Accident: onGood < totalGood,
	})
	t.Logf("db.Exec: %d/%d 本 / DSN: %d/%d 本", onBad, totalBad, onGood, totalGood)

	if onGood != totalGood {
		t.Errorf("DSN に書いても全接続に効いていない（%d/%d）。対策が効いていない", onGood, totalGood)
	}
}

// ---- ⑧ 遅延トランザクションの昇格 ----

func deferredUpgrade(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	for _, c := range []struct {
		label     string
		immediate bool
	}{{"BEGIN（既定の DEFERRED）", false}, {"BEGIN IMMEDIATE", true}} {
		p := sqlitefacts.DefaultPragmas()
		p.BusyTimeout = 2 * time.Second
		db, _, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, p, tenant, 10)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)
		ok, busy, err := sqlitefacts.DeferredUpgrade(ctx, db, tenant, c.immediate, 20)
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		rec.Add(expkit.Variant{
			Name:     "読んでから書くトランザクションを2本同時 / " + c.label,
			Desc:     "20 回 × 2 本 = 40 トランザクション。busy_timeout=2s",
			Counters: map[string]int64{"成功": int64(ok), "進めなかった": int64(busy)},
			Accident: busy > 0,
			Notes: []string{
				"DEFERRED は読み取りロックから始まり、最初の書き込みで昇格しようとする。" +
					"両方が読んでから書くと、片方は昇格できない",
				"★このとき busy_timeout は助けにならない。相手も同じ場所で待っているので、待っても順番が来ない",
				"IMMEDIATE は最初から書き込みロックを取るので、待てば必ず順番が来る",
			},
		})
		t.Logf("%s → 成功 %d / 進めなかった %d", c.label, ok, busy)
	}
}

// ---- ⑨ マイグレーションの途中失敗 ----

func migrationInTx(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	db, _, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, sqlitefacts.DefaultPragmas(), tenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	stmts := []string{
		"CREATE TABLE exp10_m1 (id INTEGER PRIMARY KEY)",
		"CREATE TABLE exp10_m2 (id INTEGER PRIMARY KEY)",
		"CREATE TABLE exp10_m2 (id INTEGER PRIMARY KEY)", // ★ここで失敗する（すでにある）
		"CREATE TABLE exp10_m3 (id INTEGER PRIMARY KEY)",
	}
	left, err := sqlitefacts.MigrateInTx(ctx, db, stmts)
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "マイグレーション4本を1つのトランザクションで流し、3本目で失敗させた",
		Desc:     "MySQL では DDL が暗黙にコミットされるので、この形は作れない（EXP-6）",
		Counters: map[string]int64{"残った表": int64(len(left))},
		Accident: len(left) > 0,
		Notes: []string{
			"残った表: " + fmt.Sprint(left),
			"0 なら、失敗したマイグレーションは**跡形もなく戻る**。" +
				"EXP-6 で必要だった『どこまで進んだかの記録』は、SQLite では要らなくなる",
			"ただし『複数のマイグレーションをまとめて1トランザクション』にすると、" +
				"その間じゅう書き込みロックを握る。起動時に他のプロセスが書けなくなる点は MySQL と同じ問題",
		},
	})
	t.Logf("失敗後に残った表: %v", left)
}

// ---- ⑩ synchronous の水準 ----

func synchronousLevels(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const rows = 500
	for _, c := range []struct{ journal, sync string }{
		{"delete", "FULL"}, {"delete", "OFF"},
		{"WAL", "FULL"}, {"WAL", "NORMAL"}, {"WAL", "OFF"},
	} {
		p := sqlitefacts.Pragmas{JournalMode: c.journal, Synchronous: c.sync,
			BusyTimeout: 5 * time.Second, ForeignKeys: true}
		db, _, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, p, tenant, rows)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		w := sqlitefacts.WriteLoad(ctx, db, tenant, rows, 1, time.Second)
		var got string
		_ = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&got)
		cleanup()

		rec.Add(expkit.Variant{
			Name:     fmt.Sprintf("journal_mode=%s / synchronous=%s", got, c.sync),
			Desc:     "1行更新のトランザクションを1本で1秒（MaxOpenConns=1）",
			Counters: map[string]int64{"ops": w.Ops},
			Metrics: map[string]float64{
				"per_sec": w.PerSec(),
				"p50_us":  float64(w.Latency.P50.Microseconds()),
				"p99_us":  float64(w.Latency.P99.Microseconds()),
			},
			Notes: []string{
				"この数字は fsync の有無で決まる。**engine 間の速度比較には使えない**",
			},
		})
		t.Logf("journal=%s sync=%s → %.0f/s p50=%v", got, c.sync, w.PerSec(), w.Latency.P50)
	}
	rec.Add(expkit.Variant{
		Name: "synchronous を下げると何を失うか（測っていないことの明示）",
		Notes: []string{
			"ここで測ったのは**速度だけ**。synchronous=OFF/NORMAL が何を失うかは、" +
				"このホストでは測れない（電源断・カーネルパニックが要る）",
			"次のサブテスト（コミット後のプロセス死）で測れるのは『プロセスが落ちても残るか』まで。" +
				"**プロセス死と電源断は別の事故**で、synchronous が効くのは後者",
			"電源断の実験は LIVE_ENV_REQUIRED（手順は docs/sqlite.md に置いた）",
		},
	})
}

// ---- ⑪ コミットが返った直後にプロセスが死ぬ ----

func commitThenDie(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/sqlitelab")
	const n = 200

	for _, c := range []struct{ journal, sync string }{
		{"WAL", "FULL"}, {"WAL", "NORMAL"}, {"WAL", "OFF"},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "exp.db")
		wal := c.journal == "WAL"

		cmd := exec.Command(bin, "-db", path, "-setup", "-dur=1ms",
			fmt.Sprintf("-wal=%v", wal), "-sync="+c.sync, "-busy=5s",
			fmt.Sprintf("-commit-then-die=%d", n))
		cmd.Env = append(os.Environ(), expkit.KillPointEnv+"=after_commit")
		out, err := cmd.CombinedOutput()

		killed := false
		if ee, ok := err.(*exec.ExitError); ok {
			killed = strings.Contains(ee.String(), "killed") || ee.ExitCode() == -1
		}

		// 落ちたあとに開き直して数える
		db, err2 := sqlitefacts.PureGo.Open(path, sqlitefacts.Pragmas{BusyTimeout: 2 * time.Second})
		if err2 != nil {
			t.Fatal(err2)
		}
		got, err3 := sqlitefacts.CountReceipts(ctx, db, "t1")
		_ = db.Close()
		if err3 != nil {
			t.Fatalf("開き直せない: %v", err3)
		}

		rec.Add(expkit.Variant{
			Name: fmt.Sprintf("%d 件コミットした直後に SIGKILL / journal=%s synchronous=%s",
				n, c.journal, c.sync),
			Desc: "外から時間を見計らって kill せず、コミットが返った地点で自分を殺す",
			Counters: map[string]int64{
				"コミットした件数": n, "落ちたあとに残っていた件数": got,
			},
			Accident: got != n,
			Notes: []string{
				fmt.Sprintf("SIGKILL された: %v / 子プロセスの出力: %s", killed, firstLine(string(out))),
				"★これで測れるのは**プロセスの死**まで。電源断は測れていない",
			},
		})
		t.Logf("journal=%s sync=%s → コミット %d / 残り %d（killed=%v）", c.journal, c.sync, n, got, killed)

		if got != n {
			t.Errorf("プロセスが落ちただけでコミット済みの %d 件が %d 件になった", n, got)
		}
	}
}

// ---- ⑫ バックアップと復元 ----

func backupRestore(t *testing.T, ctx context.Context, rec *expkit.Recorder) {
	const rows = 300
	dir := t.TempDir()

	src, path, cleanup, err := sqlitefacts.TempDB(ctx, sqlitefacts.PureGo, sqlitefacts.DefaultPragmas(), tenant, rows)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// 本物のデータ（この時点でコミット済み。WAL に載っている可能性が高い）
	want := sqlitefacts.Verify(ctx, sqlitefacts.PureGo, path, tenant)

	type backup struct {
		name string
		make func(dst string) (int64, error)
	}
	backups := []backup{
		{".db だけをコピー", func(dst string) (int64, error) { return sqlitefacts.CopyMainFile(path, dst) }},
		{".db + -wal + -shm をコピー", func(dst string) (int64, error) { return sqlitefacts.CopyWithWAL(path, dst) }},
		{"VACUUM INTO", func(dst string) (int64, error) { return sqlitefacts.VacuumInto(ctx, src, dst) }},
	}
	for i, b := range backups {
		dst := filepath.Join(dir, fmt.Sprintf("backup%d.db", i))
		size, err := b.make(dst)
		if err != nil {
			rec.Add(expkit.Variant{
				Name: "バックアップ: " + b.name, Accident: true,
				Notes: []string{"バックアップに失敗: " + err.Error()},
			})
			continue
		}
		got := sqlitefacts.Verify(ctx, sqlitefacts.PureGo, dst, tenant)
		lost := got.SchemaHash != want.SchemaHash

		rec.Add(expkit.Variant{
			Name: "バックアップ: " + b.name,
			Desc: "書いた直後（チェックポイント前）にバックアップして、別の場所で開き直した",
			Counters: map[string]int64{
				"元の行数": want.Rows, "復元後の行数": got.Rows, "バックアップの大きさ": size,
			},
			Accident: lost,
			Notes: []string{
				fmt.Sprintf("integrity_check = %q", got.Integrity),
				fmt.Sprintf("中身のハッシュ: 元 %s / 復元後 %s", want.SchemaHash, got.SchemaHash),
				"★『バックアップコマンドが成功した』も『integrity_check が ok』も、" +
					"中身が同じであることを意味しない",
			},
		})
		t.Logf("%s → 行数 %d→%d integrity=%s hash %s→%s (欠損=%v)",
			b.name, want.Rows, got.Rows, got.Integrity, want.SchemaHash, got.SchemaHash, lost)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.TrimSpace(s)
}
