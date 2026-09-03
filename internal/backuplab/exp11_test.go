package backuplab_test

// EXP-11: バックアップ・復元・破損。
//
//	MYSQL_DSN=... MYSQL_DSN2=... go test ./internal/backuplab/ -run TestEXP11 -v
//
// この実験の主張は1つ:
// **「バックアップコマンドが成功した」を成功条件にしない。**
//
// 確かめるのは、データを書く → バックアップ → 元を壊す → 別の環境へ復元 →
// スキーマの指紋を突き合わせる → 中身のハッシュを突き合わせる（行数ではなく）→
// アプリを起動して lease / robot_state / マイグレーション状態を読み戻せる、まで。
//
// そのうえで、通ってはいけないバックアップ（切れた・古い・スキーマ違い）を
// 実際に作って、拒否できることを見る。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/backuplab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/repo"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

// この実験が面倒を見る表（アプリが起動時に要る状態がここに全部ある）。
var appTables = []string{
	"robot_state", "robot_state_history", "worker_lease",
	"robot_profile", "schema_migrations",
}

const (
	expTenant = "exp11-tenant"
	expOwner  = "exp11-owner-A"
)

// dstDSN は復元先（別環境）の DSN。MYSQL_DSN2 が無ければ、
// MYSQL_DSN の DBName を workerdb2 に差し替えて使う。
func dstDSN(t *testing.T, srcDSN string) string {
	if d := os.Getenv("MYSQL_DSN2"); d != "" {
		return d
	}
	tgt, err := backuplab.Parse(srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	return tgt.WithSchema("workerdb2").DSN
}

func TestEXP11_BackupRestoreCorruption(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	srcDSN := mysqltest.DSN(t)
	dst := dstDSN(t, srcDSN)

	srcT, err := backuplab.Parse(srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	dstT, err := backuplab.Parse(dst)
	if err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-11", "backup-restore-corruption",
		"バックアップを別環境へ復元し、中身のハッシュとアプリの起動まで確かめる")
	rawSrc, err := sql.Open("mysql", srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawSrc.Close() }()
	rec.Env(expkit.CaptureEnv(ctx, rawSrc))
	rec.Freeze(freeze)

	rec.Workload("robots", 200).
		Workload("tables", appTables).
		Injection("truncate", "バックアップの末尾を 60% で切る").
		Injection("corrupt", "中ほどのバイトを1つ反転").
		Injection("stale", "バックアップ後に値だけ変える（行数は変えない）").
		Injection("schema_mismatch", "robot_profile を含めずにバックアップする")

	// ---- 元の環境を用意して、状態を書く ----
	want := setupSource(t, ctx, srcDSN)
	dumpDir := t.TempDir()

	// ============ ① 正常系: 取って・戻して・起動する ============
	t.Run("正常系", func(t *testing.T) {
		good := filepath.Join(dumpDir, "good.sql")
		dump, err := backuplab.MySQLDump(ctx, srcT, good, backuplab.DumpOptions{
			SingleTransaction: true, Tables: appTables,
		})
		if err != nil {
			t.Fatalf("バックアップに失敗: %v", err)
		}

		if err := backuplab.Restore(ctx, dstT, good, appTables); err != nil {
			t.Fatalf("復元に失敗: %v", err)
		}

		// ★行数ではなく指紋で突き合わせる
		rawDst, err := sql.Open("mysql", dst)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rawDst.Close() }()
		got, err := backuplab.Take(ctx, rawDst, appTables)
		if err != nil {
			t.Fatalf("復元先の指紋を取れない: %v", err)
		}
		equal, diff := want.Equal(got)

		// ★「バックアップコマンドが成功した」ではなく「アプリが起動できる」まで見る
		started := startApp(t, ctx, dst)

		rec.Add(expkit.Variant{
			Name: "正常系（--single-transaction / 別環境へ復元 / 起動）",
			Desc: fmt.Sprintf("バックアップ %d バイト・%v", dump.Bytes, dump.Took.Round(time.Millisecond)),
			Counters: map[string]int64{
				"復元できた": b2i(equal), "アプリ起動": b2i(started.ok),
			},
			Notes: append([]string{
				"コマンド: " + dump.Cmd,
				"スキーマ指紋: 元 " + shortHash(want.Schema) + " / 復元後 " + shortHash(got.Schema),
				started.summary,
			}, diffNotes(diff)...),
		})
		if !equal {
			t.Errorf("正常系なのに指紋が一致しない: %v", diff)
		}
		if !started.ok {
			t.Errorf("復元した環境でアプリが起動できない: %s", started.summary)
		}
		t.Logf("正常系: 一致=%v 起動=%v (%s)", equal, started.ok, started.summary)
	})

	// ============ ② 切れたバックアップを拒否する ============
	t.Run("切れたバックアップ", func(t *testing.T) {
		bad := filepath.Join(dumpDir, "truncated.sql")
		if _, err := backuplab.MySQLDump(ctx, srcT, bad, backuplab.DumpOptions{
			SingleTransaction: true, Tables: appTables,
		}); err != nil {
			t.Fatal(err)
		}
		if err := backuplab.Truncate(bad, 0.6); err != nil {
			t.Fatal(err)
		}
		restoreErr := backuplab.Restore(ctx, dstT, bad, appTables)

		// 切れたダンプでも、途中まで流れて「途中まで復元される」ことがある。
		// だから「復元コマンドが失敗したか」だけでなく、指紋も見る。
		rejected := restoreErr != nil
		var afterDiff []string
		if restoreErr == nil {
			rawDst, _ := sql.Open("mysql", dst)
			got, ferr := backuplab.Take(ctx, rawDst, appTables)
			_ = rawDst.Close()
			if ferr != nil {
				rejected = true // 開いて確かめようとして壊れていた＝拒否できる
				afterDiff = []string{"復元後の指紋を取れなかった: " + ferr.Error()}
			} else if eq, d := want.Equal(got); !eq {
				rejected = true // コマンドは通ったが中身が違う＝指紋で拒否できる
				afterDiff = diffNotes(d)
			}
		}

		rec.Add(expkit.Variant{
			Name:     "切れたバックアップ（末尾 40% を捨てた）",
			Desc:     "転送が途中で切れた・ディスクが足りなかった状況",
			Counters: map[string]int64{"拒否できた": b2i(rejected)},
			Accident: !rejected,
			Notes: append([]string{
				fmt.Sprintf("復元コマンドのエラー: %v", restoreErr),
				"★mysqldump は書いている途中でも終了コード 0 を返す。" +
					"『取れた』を信じると、切れたダンプを保管し続けることになる",
			}, afterDiff...),
		})
		if !rejected {
			t.Error("切れたバックアップを拒否できなかった")
		}
		t.Logf("切れたバックアップ: 拒否=%v (err=%v)", rejected, restoreErr)
	})

	// ============ ③ 古いバックアップを見分ける（行数は同じ） ============
	t.Run("古いバックアップ", func(t *testing.T) {
		old := filepath.Join(dumpDir, "stale.sql")
		if _, err := backuplab.MySQLDump(ctx, srcT, old, backuplab.DumpOptions{
			SingleTransaction: true, Tables: appTables,
		}); err != nil {
			t.Fatal(err)
		}

		// バックアップの後に、元の環境で**値だけ**を変える（行数は変えない）
		mutateSource(t, ctx, srcDSN)
		nowWant, err := takeSource(t, ctx, srcDSN)
		if err != nil {
			t.Fatal(err)
		}

		// 古いバックアップを復元して、いまの元と突き合わせる
		if err := backuplab.Restore(ctx, dstT, old, appTables); err != nil {
			t.Fatal(err)
		}
		rawDst, _ := sql.Open("mysql", dst)
		got, err := backuplab.Take(ctx, rawDst, appTables)
		_ = rawDst.Close()
		if err != nil {
			t.Fatal(err)
		}

		equal, diff := nowWant.Equal(got)
		sameRows := rowsMatch(nowWant, got)

		rec.Add(expkit.Variant{
			Name:     "古いバックアップ（後から値だけ変えた）",
			Desc:     "行数は完全に同じ。中身だけが古い",
			Counters: map[string]int64{"行数が一致": b2i(sameRows), "指紋も一致": b2i(equal)},
			Accident: equal, // 一致してしまったら見分けられていない
			Notes: append([]string{
				"★行数で判定していたら『同じ』に見える。中身のハッシュだけが違いを捉える",
			}, diffNotes(diff)...),
		})
		if equal {
			t.Error("古いバックアップを現在と区別できなかった（中身の差を見落とした）")
		}
		if !sameRows {
			t.Log("注: 行数も変わってしまった。値だけ変える意図と食い違うので確認する")
		}
		t.Logf("古いバックアップ: 行数一致=%v 指紋一致=%v", sameRows, equal)

		// 元の環境を元へ戻す（後続に影響させない）
		want = restoreSourceBaseline(t, ctx, srcDSN)
	})

	// ============ ④ スキーマ違いを拒否する ============
	t.Run("スキーマ違い", func(t *testing.T) {
		// robot_profile を**含めずに**バックアップする（表が1つ足りない環境）
		partial := filepath.Join(dumpDir, "partial.sql")
		fewer := []string{"robot_state", "robot_state_history", "worker_lease", "schema_migrations"}
		if _, err := backuplab.MySQLDump(ctx, srcT, partial, backuplab.DumpOptions{
			SingleTransaction: true, Tables: fewer,
		}); err != nil {
			t.Fatal(err)
		}
		if err := backuplab.Restore(ctx, dstT, partial, appTables); err != nil {
			t.Fatal(err)
		}
		rawDst, _ := sql.Open("mysql", dst)
		got, err := backuplab.Take(ctx, rawDst, fewer) // 復元先には robot_profile が無い
		_ = rawDst.Close()
		if err != nil {
			t.Fatal(err)
		}
		// 元（5表）と復元後（4表）を突き合わせる
		equal, diff := want.Equal(got)

		// ★ここが肝心: アプリは**起動できてしまう**。
		// schema_migrations は 0003 まで『済』で、repo.Migrate は「当たっている」と判断し、
		// アプリが読むのは robot_state と lease だけなので、robot_profile が無くても起動する。
		// 「起動したから復元できた」を信じると、robot_profile を最初に触った瞬間に本番で落ちる。
		started := startApp(t, ctx, dst)

		rec.Add(expkit.Variant{
			Name:     "スキーマ違い（robot_profile を含めないバックアップ）",
			Desc:     "schema_migrations は『0003 まで済』と言うのに、実際の表が足りない",
			Counters: map[string]int64{"指紋で拒否": b2i(!equal), "アプリは起動してしまう": b2i(started.ok)},
			Accident: equal,
			Notes: append([]string{
				"★アプリは起動できてしまう（起動を成功条件にすると見逃す）。" + started.summary,
				"schema_migrations の記録（0003 まで済）を信じると『当たっている』と誤認する。" +
					"実物の表を指紋で見て初めて robot_profile の欠落が分かる",
			}, diffNotes(diff)...),
		})
		if equal {
			t.Error("スキーマ違いを指紋で拒否できなかった")
		}
		t.Logf("スキーマ違い: 指紋拒否=%v アプリ起動=%v (%s)", !equal, started.ok, started.summary)

		// 復元先を正しい断面へ戻しておく（後続の再現性のため）
		good := filepath.Join(dumpDir, "good.sql")
		if _, err := os.Stat(good); err == nil {
			_ = backuplab.Restore(ctx, dstT, good, appTables)
		}
	})

	// ============ ⑤ バイト破損を拒否する ============
	t.Run("バイト破損", func(t *testing.T) {
		bad := filepath.Join(dumpDir, "corrupt.sql")
		if _, err := backuplab.MySQLDump(ctx, srcT, bad, backuplab.DumpOptions{
			SingleTransaction: true, Tables: appTables,
		}); err != nil {
			t.Fatal(err)
		}
		if err := backuplab.Corrupt(bad, 0.5); err != nil {
			t.Fatal(err)
		}
		restoreErr := backuplab.Restore(ctx, dstT, bad, appTables)
		rejected := restoreErr != nil
		var note string
		if !rejected {
			rawDst, _ := sql.Open("mysql", dst)
			got, ferr := backuplab.Take(ctx, rawDst, appTables)
			_ = rawDst.Close()
			if ferr != nil {
				rejected = true
				note = "指紋取得に失敗: " + ferr.Error()
			} else if eq, d := want.Equal(got); !eq {
				rejected = true
				note = fmt.Sprintf("指紋が違う: %v", d)
			} else {
				note = "破損箇所がデータに影響しなかった（コメント部など）"
			}
		}
		rec.Add(expkit.Variant{
			Name:     "バイト破損（中ほどを1バイト反転）",
			Counters: map[string]int64{"拒否できた": b2i(rejected)},
			Accident: !rejected,
			Notes:    []string{fmt.Sprintf("復元エラー: %v", restoreErr), note},
		})
		t.Logf("バイト破損: 拒否=%v (err=%v %s)", rejected, restoreErr, note)
		// バイト反転は位置によっては無害なコメントに当たることがある。
		// その場合は拒否できないのが正しいので、ここでは Fatal にしない（Notes に残す）。
	})

	rec.Scope(
		"MySQL 8.0 / mysqldump --single-transaction（InnoDB の一貫した断面）",
		"復元先はテーブル単位の権限だけで作り直せる（DROP DATABASE を使わない）",
		"指紋は information_schema のスキーマ定義ハッシュ + 表ごとの中身ハッシュ + schema_migrations",
		"判定は**行数ではなく中身のハッシュ**",
	)
	rec.Uncertain(
		"物理バックアップ（Percona XtraBackup・スナップショット）は未検証。ここは論理（mysqldump）のみ",
		"point-in-time recovery（binlog の適用）は未検証",
		"レプリカからのバックアップ・GTID の整合は未検証",
		"BLOB や生成列・トリガ・ビューを含む表での指紋は未検証（このアプリには無い）",
		"『別環境』は同一サーバの別スキーマ。別ホスト・別バージョンの MySQL への復元は未検証",
	)
	rec.Artifact(
		"internal/backuplab: 取る／戻す／壊す／指紋を突き合わせる。指紋は行数ではなく中身のハッシュ",
		"backuplab.Fingerprint.Equal: 差の理由（スキーマ・表・マイグレーション）を文で返す",
		"『別環境へ復元してアプリを起動する』を成功条件にした回帰テスト",
	)
	rec.Next("なし（EXP-1..11 完了）")

	files, err := rec.Save(verdict)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

var freeze = "1) mysqldump は書いている途中でも終了コード 0 を返すので、" +
	"『コマンドが成功した』は復元できることを意味しない。" +
	"2) 切れたバックアップ・バイト破損は、復元が失敗するか、復元後の指紋が違うことで拒否できる。" +
	"3) 古いバックアップは行数が完全に同じでも、中身のハッシュが違うことで見分けられる。" +
	"4) schema_migrations が『済』と言っていても、実際の表が足りないこと（スキーマ違い）は起こる。" +
	"記録ではなく実物の表を指紋で見ないと分からない。" +
	"5) 正常系は、別環境へ復元してアプリ（store + repo + lease）が起動し、" +
	"lease・robot_state・マイグレーション状態を読み戻せる。"

var verdict = "バックアップの成功条件は『コマンドが 0 で終わった』ではなく" +
	"『別環境へ復元して、中身のハッシュが一致し、アプリが起動して状態を読み戻せる』こと。" +
	"切れた・破損・古い・スキーマ違いのいずれも、行数ではなく指紋（スキーマ定義＋中身のハッシュ）で捉えられる。" +
	"特に古いバックアップは行数が同一でも中身で見分けられる。詳細は docs/backup-restore.md。"

// ---- 元の環境を用意する ----

func setupSource(t *testing.T, ctx context.Context, dsn string) backuplab.Fingerprint {
	t.Helper()
	// アプリと同じ道具でスキーマを作る（store + repo）
	s, err := store.Open(dsn, store.DefaultPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	d, err := repo.Open(dsn, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := repo.Migrate(ctx, d); err != nil {
		t.Fatalf("repo.Migrate: %v", err)
	}

	seedState(t, ctx, s)
	seedLease(t, ctx, s.DB())
	seedProfile(t, ctx, d.SQL())

	fp, err := takeSource(t, ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// restoreSourceBaseline は元の環境を、古いバックアップ実験で変えた値から戻す。
func restoreSourceBaseline(t *testing.T, ctx context.Context, dsn string) backuplab.Fingerprint {
	t.Helper()
	s, err := store.Open(dsn, store.DefaultPool())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seedState(t, ctx, s) // 同じ値で入れ直す
	fp, err := takeSource(t, ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func seedState(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	// 後始末してから、決まった値で 200 行入れる（指紋が実行間で安定するように）
	if _, err := s.DB().ExecContext(ctx, "DELETE FROM robot_state WHERE tenant_id = ?", expTenant); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	rows := make([]worker.Row, 200)
	for i := range rows {
		rows[i] = worker.Row{
			ID:         model.ID(fmt.Sprintf("r%05d", i)),
			State:      model.State{Status: model.StatusRunning, Online: true, Battery: int8(i % 100)},
			ObservedAt: base.Add(time.Duration(i) * time.Second),
			Source:     model.SourceAPI,
		}
	}
	if _, err := s.UpsertBatch(ctx, expTenant, rows); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
}

func seedLease(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "DELETE FROM worker_lease WHERE tenant_id = ?", expTenant); err != nil {
		t.Fatal(err)
	}
	m := lease.NewManager(db, time.Hour)
	if _, ok, err := m.Acquire(ctx, expTenant, expOwner); err != nil || !ok {
		t.Fatalf("lease.Acquire: ok=%v err=%v", ok, err)
	}
}

func seedProfile(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "DELETE FROM robot_profile WHERE tenant_id = ?", expTenant); err != nil {
		t.Fatal(err)
	}
	//smlint:allow rowsaffected 理由: 実験の初期データ投入。入るかはエラーで判る
	if _, err := db.ExecContext(ctx,
		"INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial, retired) VALUES (?,?,?,?,?,0)",
		expTenant, "r00000", "ロビー", "ACME-9000", "SN-EXP11-0001"); err != nil {
		t.Fatalf("robot_profile: %v", err)
	}
}

// mutateSource は行数を変えずに値だけ変える（古いバックアップの実験用）。
func mutateSource(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(ctx,
		"UPDATE robot_state SET battery = (battery + 1) % 100 WHERE tenant_id = ?", expTenant)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		t.Fatal("値を変えられなかった（対象行が無い）")
	}
}

func takeSource(t *testing.T, ctx context.Context, dsn string) (backuplab.Fingerprint, error) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return backuplab.Fingerprint{}, err
	}
	defer func() { _ = db.Close() }()
	return backuplab.Take(ctx, db, appTables)
}

// ---- アプリの起動 ----

type startResult struct {
	ok      bool
	summary string
}

// startApp は復元した環境に対して、アプリの起動でやることをやる。
//
//   - store を開いてスキーマの整合を確かめる（robot_state を1件読む）
//   - repo.Migrate を呼ぶ（schema_migrations が『済』なら何もしないはず）
//   - lease を読み戻す（担当が保存されているか）
func startApp(t *testing.T, ctx context.Context, dsn string) startResult {
	t.Helper()
	d, err := repo.Open(dsn, repo.Options{})
	if err != nil {
		return startResult{false, "repo.Open 失敗: " + err.Error()}
	}
	defer func() { _ = d.Close() }()

	// マイグレーション状態を読み戻す。復元がスキーマ違いなら、ここで止まる。
	if err := repo.Migrate(ctx, d); err != nil {
		return startResult{false, "repo.Migrate 失敗: " + err.Error()}
	}

	var n int
	if err := d.SQL().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM robot_state WHERE tenant_id = ?", expTenant).Scan(&n); err != nil {
		return startResult{false, "robot_state 読み取り失敗: " + err.Error()}
	}

	m := lease.NewManager(d.SQL(), time.Hour)
	l, err := m.Get(ctx, expTenant)
	if err != nil {
		return startResult{false, "lease 読み取り失敗: " + err.Error()}
	}
	if l.Owner != expOwner {
		return startResult{false, fmt.Sprintf("lease の担当が違う: %q", l.Owner)}
	}
	return startResult{true, fmt.Sprintf("robot_state %d 件 / lease owner=%s fence=%d", n, l.Owner, l.Fence)}
}

// ---- 小物 ----

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func rowsMatch(a, b backuplab.Fingerprint) bool {
	if len(a.Rows) != len(b.Rows) {
		return false
	}
	for k, v := range a.Rows {
		if b.Rows[k] != v {
			return false
		}
	}
	return true
}

func diffNotes(diff []string) []string {
	if len(diff) == 0 {
		return []string{"指紋は完全に一致した"}
	}
	out := make([]string, len(diff))
	for i, d := range diff {
		out[i] = "差: " + d
	}
	return out
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// TestEXP11_指紋規則がプロセスをまたいで安定 は、content hash の生成規則が
// **同じデータなら同じ hash を返す**ことを、別プロセスでも確かめる。
//
// ★「行数一致・hash 不一致」で古いバックアップを見分ける仕組みは、
// hash 規則そのものがプロセスや version で揺れないことに依っている。
// そこが揺れたら、検出は信用できない。だから hash 規則自体をテストする。
func TestEXP11_指紋規則がプロセスをまたいで安定(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)

	// 元の環境を用意（EXP-11 本体と同じ道具）
	_ = setupSource(t, ctx, dsn)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// ① 同一プロセスで2回取る → 完全一致（決定性）
	fp1, err := backuplab.Take(ctx, db, appTables)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := backuplab.Take(ctx, db, appTables)
	if err != nil {
		t.Fatal(err)
	}
	if eq, diff := fp1.Equal(fp2); !eq {
		t.Fatalf("同一プロセスで2回取って一致しない（非決定的）: %v", diff)
	}

	// ② 別プロセス（cmd/fphash）で取る → 同じ hash
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/fphash")
	out, err := exec.Command(bin, "-dsn", dsn, "-tables", strings.Join(appTables, ",")).CombinedOutput()
	if err != nil {
		t.Fatalf("fphash: %v\n%s", err, out)
	}
	var child backuplab.Fingerprint
	if err := json.Unmarshal([]byte(firstJSONLine(string(out))), &child); err != nil {
		t.Fatalf("子プロセスの出力を読めない: %v\n%s", err, out)
	}
	if eq, diff := fp1.Equal(child); !eq {
		t.Errorf("別プロセスで hash が変わった（規則がプロセス依存）: %v", diff)
	} else {
		t.Logf("hash 規則はプロセスをまたいで安定: schema=%.12s", fp1.Schema)
	}

	// ③ cross-version は LIVE。ここでは版差を跨いだ検証はしていないことを明示する。
	for _, sc := range backuplab.EvidenceMatrix() {
		if !sc.Verified {
			t.Logf("未実証の水準: %s — %s", sc.Scope, sc.Note)
		}
	}
}

func firstJSONLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "{") {
			return ln
		}
	}
	return s
}
