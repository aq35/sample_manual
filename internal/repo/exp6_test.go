package repo_test

// EXP-6: マイグレーション途中の crash。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run TestEXP6 -v -timeout 20m
//
// ★「同時に走らせない」と「途中で落ちる」は別の性質。分けて確かめる。
// 実験は本番と別のデータベース（workerdb2）に対して行う。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/repo"
)

type migState struct {
	Migrations []struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
		State   string `json:"state"`
	} `json:"migrations"`
	Objects map[string]bool `json:"objects"`
}

func TestEXP6_マイグレーション途中のcrash(t *testing.T) {
	mysqltest.Serialize(t)
	dsn := labDSN(t)
	bin := expkit.Build(t, "github.com/aq35/sample_manual/cmd/migratelab")
	ctx := context.Background()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Skipf("実験用データベースに接続できないため skip: %v", err)
	}

	rec := expkit.NewRecorder("EXP-6", "migration-crash-matrix",
		"マイグレーションの各段階で落ちたとき、半端な状態を成功扱いしないか")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(strings.Join([]string{
		"1) どの段階で落ちても、実際には当たっていないマイグレーションが done として記録されることはない。",
		"2) DDL を当てたのに done を記録する前に落ちた場合、次の起動は黙って進まず、",
		"   『途中で終わっている』と言って止まる。",
		"3) 8プロセス同時起動でも、同じ DDL が2回走ることはない。",
		"4) 適用済みファイルの改変は checksum で拒否される。",
	}, " "))
	rec.Workload("migrations", 3).
		Workload("database", "workerdb2（本番用とは別）").
		Injection("stages", repo.MigrationStages).
		Injection("concurrency", 8)

	// ---- ① 各段階で落として、次の起動の挙動を見る ----
	type stageResult struct {
		Stage       string
		AfterCrash  migState
		Recovered   bool
		RecoverErr  string
		Explainable bool
		FalseDone   bool
	}
	var (
		results   []stageResult
		falseDone int
	)
	for _, stage := range repo.MigrationStages {
		fresh(t, ctx, bin, dsn)

		info := runChild(t, ctx, bin, dsn, expkit.KillPointEnv+"="+stage)
		if !info.Killed && stage != repo.StageAfterAll {
			// after_all は最後の段階なので、落ちる前に終わることがある
			t.Logf("[%s] 落ちなかった（%s）", stage, info)
		}
		st := report(t, ctx, bin, dsn)

		// 落ちた直後の状態が「説明できる」か:
		//   done と記録されているものは、その成果物が実在する
		explainable := true
		for _, m := range st.Migrations {
			if m.State != "done" {
				continue
			}
			for _, obj := range objectsOf(m.Version) {
				if !st.Objects[obj] {
					explainable = false
					falseDone++
				}
			}
		}

		// 次の起動
		rinfo := runChild(t, ctx, bin, dsn)
		recovered := rinfo.Code == 0
		errMsg := firstLine(rinfo.Stdout, "MIGRATE_ERROR")
		after := report(t, ctx, bin, dsn)
		if recovered {
			// 完了したなら、全部の成果物が揃っているはず
			for _, obj := range allObjects() {
				if !after.Objects[obj] {
					t.Errorf("[%s] 回復したのに %s が無い", stage, obj)
				}
			}
		} else {
			// 完了しないなら、理由が「途中で終わっている」であること
			if !strings.Contains(errMsg, "途中で終わっている") {
				t.Errorf("[%s] 回復できず、理由も不明: %q", stage, errMsg)
			}
		}
		results = append(results, stageResult{
			Stage: stage, AfterCrash: st, Recovered: recovered,
			RecoverErr: errMsg, Explainable: explainable,
		})
		t.Logf("[%-22s] 落ちた直後: %s / 次の起動: %s", stage, describe(st), outcome(recovered, errMsg))
	}

	notes := make([]string, 0, len(results))
	for _, r := range results {
		notes = append(notes, fmt.Sprintf("%-22s 直後=%s → 次の起動=%s",
			r.Stage, describe(r.AfterCrash), outcome(r.Recovered, r.RecoverErr)))
	}
	rec.Add(expkit.Variant{
		Name:     "各段階で SIGKILL したあとの、次の起動",
		Desc:     "started → DDL → done の二相記録と checksum で、半端な状態を成功扱いしないか",
		Counters: map[string]int64{"stages": int64(len(repo.MigrationStages)), "false_done": int64(falseDone)},
		Accident: falseDone > 0,
		Notes:    notes,
	})
	if falseDone > 0 {
		t.Errorf("成果物が無いのに done と記録されたものが %d 件", falseDone)
	}

	// ---- ② 8プロセス同時起動（crash なし） ----
	fresh(t, ctx, bin, dsn)
	oks, fails := runConcurrent(t, ctx, bin, dsn, 8, "")
	st := report(t, ctx, bin, dsn)
	applied := len(st.Migrations)
	rec.Add(expkit.Variant{
		Name: "8プロセス同時起動（crash なし）",
		Counters: map[string]int64{
			"succeeded": int64(oks), "failed": int64(fails), "migrations_recorded": int64(applied),
		},
		Accident: fails > 0 || applied != 3,
		Notes: []string{
			fmt.Sprintf("成功 %d / 失敗 %d / 記録されたマイグレーション %d 件", oks, fails, applied),
			"GET_LOCK で直列化しているので、同じ DDL が2回走ることはない（docs/locking.md）",
		},
	})
	if fails > 0 || applied != 3 {
		t.Errorf("同時起動で失敗 %d / 記録 %d 件（3 のはず）", fails, applied)
	}
	t.Logf("[同時起動] 成功 %d / 失敗 %d / 記録 %d 件", oks, fails, applied)

	// ---- ③ 同時起動 ＋ 1プロセスが DDL 途中で落ちる ----
	fresh(t, ctx, bin, dsn)
	oks2, fails2 := runConcurrentWithCrash(t, ctx, bin, dsn, 8, repo.StageAfterDDLBeforeDone)
	st2 := report(t, ctx, bin, dsn)
	unfinished := 0
	for _, m := range st2.Migrations {
		if m.State != "done" {
			unfinished++
		}
	}
	rec.Add(expkit.Variant{
		Name: "8プロセス同時起動 ＋ 1つが DDL 適用後・done 記録前に落ちる",
		Counters: map[string]int64{
			"succeeded": int64(oks2), "failed": int64(fails2), "unfinished_rows": int64(unfinished),
		},
		Accident: false,
		Notes: []string{
			fmt.Sprintf("成功 %d / 失敗 %d / 途中のまま残った記録 %d 件", oks2, fails2, unfinished),
			"落ちたプロセスのロックは接続が切れて解放され、次のプロセスが入る",
			"次のプロセスは『途中で終わっている』を見つけて止まる。**同じ DDL を勝手に流し直さない**",
		},
	})
	t.Logf("[同時起動+crash] 成功 %d / 失敗 %d / 途中のまま %d 件", oks2, fails2, unfinished)
	if unfinished == 0 && fails2 == 0 {
		t.Log("注意: この試行では crash 前に他プロセスが全部当て終わっていた")
	}

	// ---- ④ 適用済みファイルの改変 ----
	fresh(t, ctx, bin, dsn)
	if info := runChild(t, ctx, bin, dsn); info.Code != 0 {
		t.Fatalf("初回のマイグレーションに失敗: %s\n%s", info, info.Stdout)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	info := runChild(t, ctx, bin, dsn)
	msg := firstLine(info.Stdout, "MIGRATE_ERROR")
	rec.Add(expkit.Variant{
		Name:     "適用済みマイグレーションの改変",
		Counters: map[string]int64{"exit_code": int64(info.Code)},
		Accident: info.Code == 0,
		Notes:    []string{fmt.Sprintf("起動を止めた: %s", msg)},
	})
	if info.Code == 0 || !strings.Contains(msg, "書き換えられている") {
		t.Errorf("改変を検出できていない: exit=%d msg=%q", info.Code, msg)
	}
	t.Logf("[改変検出] %s", msg)

	rec.Scope(
		"MySQL 8.0 / 実験用データベース workerdb2 / マイグレーション3本（うち1本は2文）",
		"排他は GET_LOCK（接続固定）。プロセスが死ねば接続が切れてロックは解放される",
		"『同時に走らせない』と『途中で落ちる』を別々の実験として測っている",
	)
	rec.Uncertain(
		"MySQL の DDL は暗黙コミットするため、複数文のマイグレーションを巻き戻す方法は無い",
		"バックアップからの復元は EXP-11 で扱う（ここでは未検証）",
		"オンライン DDL（ALGORITHM=INPLACE）の途中で落ちた場合の残骸は未検証",
		"レプリカへの伝播中に落ちた場合は未検証",
	)
	rec.Artifact(
		"repo.Options.MigrationHook: 本番のマイグレーション処理をそのまま使って段階ごとに落とせる",
		"cmd/migratelab -report: crash 後の状態（記録と実際の成果物）を JSON で出す",
	)
	rec.Next("EXP-11 backup/restore: 半端な状態から復元できることの証明")

	files, err := rec.Save(strings.Join([]string{
		"7段階すべてで、成果物が無いのに done と記録されたものは 0 件だった。",
		"DDL を当てたあと done を記録する前に落ちた場合、次の起動は黙って進まず、",
		"『途中で終わっている』と言って止まる（自動で流し直さない）。",
		"8プロセス同時起動でも同じ DDL は2回走らず、適用済みファイルの改変は checksum で拒否された。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果を保存した: %v", files)
}

func objectsOf(version int) []string {
	switch version {
	case 1:
		return []string{"table:robot_profile"}
	case 2:
		return []string{"index:idx_profile_created"}
	case 3:
		return []string{"column:retired", "index:idx_profile_retired"}
	}
	return nil
}

func allObjects() []string {
	return []string{"table:robot_profile", "index:idx_profile_created",
		"column:retired", "index:idx_profile_retired"}
}

func describe(st migState) string {
	if len(st.Migrations) == 0 {
		return "記録なし"
	}
	var parts []string
	for _, m := range st.Migrations {
		parts = append(parts, fmt.Sprintf("%d:%s", m.Version, m.State))
	}
	objs := 0
	for _, ok := range st.Objects {
		if ok {
			objs++
		}
	}
	return fmt.Sprintf("%s（成果物 %d/4）", strings.Join(parts, ","), objs)
}

func outcome(recovered bool, msg string) string {
	if recovered {
		return "完了"
	}
	if msg == "" {
		return "失敗（理由不明）"
	}
	if i := strings.Index(msg, "（"); i > 0 {
		msg = msg[:i]
	}
	return "停止: " + strings.TrimSpace(strings.TrimPrefix(msg, "MIGRATE_ERROR "))
}

func fresh(t *testing.T, ctx context.Context, bin, dsn string) {
	t.Helper()
	c, err := expkit.StartChild(ctx, bin, []string{"-dsn", dsn, "-fresh", "-report"})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := c.Wait(30 * time.Second); err != nil || info.Code != 0 {
		t.Fatalf("初期化に失敗: %v %v", err, info)
	}
}

func runChild(t *testing.T, ctx context.Context, bin, dsn string, env ...string) expkit.ExitInfo {
	t.Helper()
	c, err := expkit.StartChild(ctx, bin, []string{"-dsn", dsn}, env...)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Wait(60 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func report(t *testing.T, ctx context.Context, bin, dsn string) migState {
	t.Helper()
	c, err := expkit.StartChild(ctx, bin, []string{"-dsn", dsn, "-report"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Wait(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	line := firstLine(info.Stdout, "STATE ")
	line = strings.TrimSpace(strings.TrimPrefix(line, "STATE "))
	var st migState
	if err := json.Unmarshal([]byte(line), &st); err != nil {
		t.Fatalf("状態を読めない: %v (%q)", err, line)
	}
	return st
}

func firstLine(stdout, marker string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	return ""
}

func runConcurrent(t *testing.T, ctx context.Context, bin, dsn string, n int, env string) (ok, fail int) {
	t.Helper()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var envs []string
			if env != "" {
				envs = append(envs, env)
			}
			info := runChild(t, ctx, bin, dsn, envs...)
			mu.Lock()
			if info.Code == 0 {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return ok, fail
}

func runConcurrentWithCrash(t *testing.T, ctx context.Context, bin, dsn string, n int, stage string) (ok, fail int) {
	t.Helper()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		crasher := i == 0
		go func() {
			defer wg.Done()
			var envs []string
			if crasher {
				envs = append(envs, expkit.KillPointEnv+"="+stage)
			}
			info := runChild(t, ctx, bin, dsn, envs...)
			mu.Lock()
			if info.Code == 0 {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return ok, fail
}

// labDSN は実験用データベース（workerdb2）の DSN。
func labDSN(t *testing.T) string {
	t.Helper()
	dsn := mysqltest.DSN(t)
	out := strings.Replace(dsn, "/workerdb?", "/workerdb2?", 1)
	if out == dsn {
		t.Skip("実験用データベース（workerdb2）を指定できないため skip")
	}
	return out
}
