package readrouter_test

// EXP-20: read-your-writes とレプリカラグ。
//
//	MYSQL_DSN=... MYSQL_DSN2=... go test ./internal/readrouter/ -run TestEXP20 -v
//
// レプリカ（workerdb2）へのレプリケーションを、遅延つきで手で模擬する。
// 「不変な過去はレプリカ／書いた直後は primary」という振り分けが正しいことを確かめる。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/readrouter"
)

const ddl = `CREATE TABLE IF NOT EXISTS exp20_kv (
  tenant_id VARCHAR(32) NOT NULL, k VARCHAR(64) NOT NULL, v VARCHAR(64) NOT NULL,
  PRIMARY KEY (tenant_id, k)) ENGINE=InnoDB`

func TestEXP20_readYourWrites_とレプリカラグ(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	primary := open(t, mysqltest.DSN(t))
	dsn2 := os.Getenv("MYSQL_DSN2")
	if dsn2 == "" {
		t.Skip("MYSQL_DSN2（レプリカ相当）が未設定のため skip")
	}
	replica := open(t, dsn2)

	for _, db := range []*sql.DB{primary, replica} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM exp20_kv WHERE tenant_id='rr'"); err != nil {
			t.Fatal(err)
		}
	}

	// レプリケーションの模擬: primary の行を、lag 後に replica へ適用する。
	lag := 300 * time.Millisecond
	replicate := func(k, v string) {
		go func() {
			time.Sleep(lag)
			//nolint
			_, _ = replica.ExecContext(context.Background(),
				"INSERT INTO exp20_kv (tenant_id,k,v) VALUES ('rr',?,?) ON DUPLICATE KEY UPDATE v=VALUES(v)", k, v)
		}()
	}

	router := readrouter.New(primary, replica)

	rec := expkit.NewRecorder("EXP-20", "read-your-writes-replica-lag",
		"予定/実績を primary とレプリカで読み分ける: 何が安全で何が壊れるか")
	rec.Env(expkit.CaptureEnv(ctx, primary))
	rec.Freeze(strings.Join([]string{
		"1) 書いた直後にレプリカ（ラグあり）から読むと、自分の書き込みが見えない（read-your-writes 違反）。",
		"2) 同じ読みを primary（Strong）から読めば、必ず見える。",
		"3) ラグが解消した後の『過去の値』は、レプリカから読んでも一致する（不変な過去はレプリカで安全）。",
		"4) worker の dispatch ポーリングは Strong（primary）で行う。レプリカだと未反映で取りこぼす。",
	}, " "))
	rec.Workload("replica_lag", lag.String())

	get := func(db *sql.DB, k string) (string, bool) {
		var v string
		err := db.QueryRowContext(ctx, "SELECT v FROM exp20_kv WHERE tenant_id='rr' AND k=?", k).Scan(&v)
		if err == sql.ErrNoRows {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return v, true
	}

	// ---- ① 書いた直後: レプリカは見えない / primary は見える ----
	if _, err := primary.ExecContext(ctx, "INSERT INTO exp20_kv (tenant_id,k,v) VALUES ('rr','sched-1','v1')"); err != nil {
		t.Fatal(err)
	}
	replicate("sched-1", "v1")

	_, okEventual := get(router.DB(readrouter.Eventual), "sched-1")   // レプリカ（ラグ中）
	vStrong, okStrong := get(router.DB(readrouter.Strong), "sched-1") // primary
	rec.Add(expkit.Variant{
		Name:     "書いた直後: Eventual（レプリカ・ラグ中）で読む",
		Accident: true,
		Counters: map[string]int64{"見えた": b2i(okEventual)},
		Notes:    []string{"自分の書き込みが見えない（read-your-writes 違反）。だから直後の読みは Strong にする"},
	})
	rec.Add(expkit.Variant{
		Name:     "書いた直後: Strong（primary）で読む",
		Counters: map[string]int64{"見えた": b2i(okStrong)},
		Notes:    []string{"必ず見える（v=" + vStrong + "）"},
	})
	t.Logf("直後: Eventual見えた=%v / Strong見えた=%v(v=%s)", okEventual, okStrong, vStrong)

	// ---- ② ラグ解消後: 過去の値はレプリカでも一致 ----
	time.Sleep(lag + 200*time.Millisecond)
	vReplica, okR := get(router.DB(readrouter.Eventual), "sched-1")
	rec.Add(expkit.Variant{
		Name:     "ラグ解消後: Eventual（レプリカ）で過去の値を読む",
		Counters: map[string]int64{"見えた": b2i(okR)},
		Notes:    []string{"不変な過去はレプリカで一致（v=" + vReplica + "）。履歴・レポートはここへ回す"},
	})
	t.Logf("ラグ解消後: Eventual見えた=%v(v=%s)", okR, vReplica)

	// ---- 検証 ----
	if okEventual {
		t.Error("ラグ中のレプリカで書いた直後の値が見えてしまった（模擬が甘い）")
	}
	if !okStrong || vStrong != "v1" {
		t.Errorf("primary で自分の書き込みが見えない: ok=%v v=%q", okStrong, vStrong)
	}
	if !okR || vReplica != "v1" {
		t.Errorf("ラグ解消後もレプリカで過去の値が見えない: ok=%v v=%q", okR, vReplica)
	}
	// dispatch は Strong=primary であること
	if router.DB(readrouter.Strong) != primary {
		t.Error("Strong が primary を返していない（dispatch は primary で行う）")
	}

	rec.Scope(
		"MySQL 8.0 / primary=workerdb, レプリカ相当=workerdb2 / ラグを 300ms で手動模擬",
		"実レプリケーションではなく、遅延つきで replica へ適用して振る舞いを再現",
	)
	rec.Uncertain(
		"実運用のラグは負荷で変動する。ラグ量の測定・監視は別途（Seconds_Behind_Master 等）",
		"read-your-writes を厳密にやるなら、書き込み後しばらく Strong に固定する（セッションピン留め）",
		"レプリカのフェイルオーバ・整合（GTID）は本実験外",
	)
	rec.Artifact("internal/readrouter: Freshness(Strong/Eventual) で primary/レプリカを振り分けるルータ")
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"予定/実績の読みをレプリカへ回すと primary の負担は減るが、レプリカにはラグがある。",
		"書いた直後・read-your-writes・worker の dispatch ポーリングは primary（Strong）で行う。",
		"不変な過去（過去の実績・履歴・レポート・admin の横断）だけをレプリカ（Eventual）へ回す。",
		"優先度は低め: 30テナントでは primary に余裕があり、まず covering索引/有界化/キャッシュが効く。",
		"読み QPS が実測で primary を圧迫し始めたら、ラグ許容の読みだけをレプリカへ。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
