package repo_test

// リポジトリ層の設計を「実験して決める」ためのコード（その1: テーブル設計）。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run Experiment_ -v -timeout 20m
//
// ここで測った結果が docs/repository-layer.md の根拠になっている。

import (
	"fmt"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/mysqltest"
)

const (
	expTenants  = 20
	expPerOwner = 5000 // 1テナントあたりの行数（合計 10万行）
)

// 実験1: マルチテナントの主キーを (tenant_id, id) にするか、id にして tenant_id へ索引を張るか。
//
// InnoDB は主キー順に行を物理配置する（クラスタ化索引）。
// テナント単位で読むなら、同じテナントの行が隣り合っているかどうかで読むページ数が変わるはず。
func TestExperiment_主キー設計(t *testing.T) {
	db := mysqltest.Raw(t)

	setup := func(name, ddl string) {
		mustExec(t, db, "DROP TABLE IF EXISTS "+name)
		mustExec(t, db, ddl)
	}
	// A: テナント先頭の複合主キー（同じテナントの行が隣接する）
	setup("exp_pk_composite", `CREATE TABLE exp_pk_composite (
		tenant_id VARCHAR(32) NOT NULL,
		id        INT          NOT NULL,
		status    TINYINT      NOT NULL,
		payload   VARCHAR(120) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	) ENGINE=InnoDB`)
	// B: 単独主キー ＋ tenant_id に二次索引（よくある形）
	setup("exp_pk_secondary", `CREATE TABLE exp_pk_secondary (
		id        INT          NOT NULL AUTO_INCREMENT,
		tenant_id VARCHAR(32)  NOT NULL,
		status    TINYINT      NOT NULL,
		payload   VARCHAR(120) NOT NULL,
		PRIMARY KEY (id),
		KEY idx_tenant (tenant_id)
	) ENGINE=InnoDB`)

	// 実運用に近い順序で入れる: テナントが入り混じって書き込まれる
	insert := func(table string, withID bool) time.Duration {
		payload := fmt.Sprintf("%0100d", 1)
		const batch = 200
		start := time.Now()
		var (
			vals []string
			args []any
		)
		flush := func() {
			if len(vals) == 0 {
				return
			}
			cols := "(tenant_id, id, status, payload)"
			if !withID {
				cols = "(tenant_id, status, payload)"
			}
			q := "INSERT INTO " + table + " " + cols + " VALUES " + joinComma(vals)
			mustExec(t, db, q, args...)
			vals, args = vals[:0], args[:0]
		}
		for i := 0; i < expPerOwner; i++ {
			for tn := 0; tn < expTenants; tn++ {
				tenant := fmt.Sprintf("t-%03d", tn)
				if withID {
					vals = append(vals, "(?,?,?,?)")
					args = append(args, tenant, i, i%3, payload)
				} else {
					vals = append(vals, "(?,?,?)")
					args = append(args, tenant, i%3, payload)
				}
				if len(vals) >= batch {
					flush()
				}
			}
		}
		flush()
		return time.Since(start)
	}
	dA := insert("exp_pk_composite", true)
	dB := insert("exp_pk_secondary", false)
	t.Logf("投入: 複合主キー %v / 単独主キー+索引 %v （%d 行）", dA.Round(time.Millisecond), dB.Round(time.Millisecond), expTenants*expPerOwner)

	mustExec(t, db, "ANALYZE TABLE exp_pk_composite")
	mustExec(t, db, "ANALYZE TABLE exp_pk_secondary")

	// テナント1件ぶんを全部読む（画面の一覧、集計、全件同期でよくやる操作）
	scan := func(q string, args ...any) (time.Duration, int) {
		for i := 0; i < 3; i++ { // 暖機
			rows, err := db.Query(q, args...)
			if err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
		}
		start := time.Now()
		const n = 10
		count := 0
		for i := 0; i < n; i++ {
			rows, err := db.Query(q, args...)
			if err != nil {
				t.Fatal(err)
			}
			count = 0
			for rows.Next() {
				var id, status int
				var payload string
				if err := rows.Scan(&id, &status, &payload); err != nil {
					t.Fatal(err)
				}
				count++
			}
			_ = rows.Close()
		}
		return time.Since(start) / n, count
	}

	sA, cA := scan("SELECT id, status, payload FROM exp_pk_composite WHERE tenant_id = ?", "t-010")
	sB, cB := scan("SELECT id, status, payload FROM exp_pk_secondary WHERE tenant_id = ?", "t-010")
	t.Logf("テナント1件ぶん(%d行)の読み出し: 複合主キー %v / 単独主キー+索引 %v （%.1f 倍）",
		cA, sA.Round(time.Microsecond), sB.Round(time.Microsecond), float64(sB)/float64(sA))
	if cA != cB || cA != expPerOwner {
		t.Fatalf("読めた行数が違う: %d / %d", cA, cB)
	}

	// 実行計画を見る
	t.Logf("複合主キー   : %s", explainLine(t, db, "SELECT id, status, payload FROM exp_pk_composite WHERE tenant_id = 't-010'"))
	t.Logf("単独主キー+索引: %s", explainLine(t, db, "SELECT id, status, payload FROM exp_pk_secondary WHERE tenant_id = 't-010'"))
	t.Log("※ 単独主キー側は、二次索引で行を特定してから1行ずつ主キーを引き直す（ランダムアクセス）。")
	t.Log("  索引だけで足りる問い合わせ（covering index）なら差は出ない。差が出るのは列を取りに行くとき。")

	// 物理サイズ（索引を含む）
	sizeA := tableSize(t, db, "exp_pk_composite")
	sizeB := tableSize(t, db, "exp_pk_secondary")
	t.Logf("表のサイズ: 複合主キー %.1f MB / 単独主キー+索引 %.1f MB", sizeA, sizeB)

	mustExec(t, db, "DROP TABLE IF EXISTS exp_pk_composite")
	mustExec(t, db, "DROP TABLE IF EXISTS exp_pk_secondary")
}

// 実験2: 主キーをランダム（UUID 相当）にすると、挿入と表のサイズがどうなるか。
func TestExperiment_ランダムな主キー(t *testing.T) {
	db := mysqltest.Raw(t)
	const rows = 100000

	mustExec(t, db, "DROP TABLE IF EXISTS exp_seq")
	mustExec(t, db, `CREATE TABLE exp_seq (
		id BIGINT NOT NULL, payload VARCHAR(120) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB`)
	mustExec(t, db, "DROP TABLE IF EXISTS exp_uuid")
	mustExec(t, db, `CREATE TABLE exp_uuid (
		id CHAR(36) NOT NULL, payload VARCHAR(120) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB`)

	payload := fmt.Sprintf("%0100d", 1)
	fill := func(table string, key func(i int) any) time.Duration {
		const batch = 200
		start := time.Now()
		var (
			vals []string
			args []any
		)
		flush := func() {
			if len(vals) == 0 {
				return
			}
			mustExec(t, db, "INSERT INTO "+table+" (id, payload) VALUES "+joinComma(vals), args...)
			vals, args = vals[:0], args[:0]
		}
		for i := 0; i < rows; i++ {
			vals = append(vals, "(?,?)")
			args = append(args, key(i), payload)
			if len(vals) >= batch {
				flush()
			}
		}
		flush()
		return time.Since(start)
	}
	dSeq := fill("exp_seq", func(i int) any { return int64(i) })
	uuids := make([]string, rows)
	for i := range uuids {
		// version4 相当のランダム文字列（挿入位置がばらける）
		uuids[i] = fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x",
			rnd.Uint32(), rnd.Intn(1<<16), rnd.Intn(1<<12), rnd.Intn(1<<16), rnd.Int63n(1<<48))
	}
	dUUID := fill("exp_uuid", func(i int) any { return uuids[i] })

	t.Logf("%d 行の投入: 連番 %v / ランダム(UUIDv4相当) %v （%.1f 倍）",
		rows, dSeq.Round(time.Millisecond), dUUID.Round(time.Millisecond), float64(dUUID)/float64(dSeq))
	mustExec(t, db, "ANALYZE TABLE exp_seq")
	mustExec(t, db, "ANALYZE TABLE exp_uuid")
	t.Logf("表のサイズ: 連番 %.1f MB / ランダム %.1f MB", tableSize(t, db, "exp_seq"), tableSize(t, db, "exp_uuid"))

	mustExec(t, db, "DROP TABLE IF EXISTS exp_seq")
	mustExec(t, db, "DROP TABLE IF EXISTS exp_uuid")
}
