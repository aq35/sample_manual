package pklab_test

// EXP-54: 主キー設計（連番 BIGINT vs ランダム UUID）。
//
//	MYSQL_DSN=... go test ./internal/pklab/ -run TestEXP54 -v
//
// InnoDB の表は主キーの B-tree（clustered）。単調増加なら追記で密、ランダムだと挿入位置が散らばり
// ページ分割で index 肥大＋挿入が重い。二次索引は主キーを内包するので太い主キー（CHAR(36)）は全索引を
// 膨らませる。同じ往復（chunk INSERT）で主キーの順序だけを変え、挿入時間と data/index サイズを比べる。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/pklab"
)

func TestEXP54_主キー設計(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	dsn := mysqltest.DSN(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	if err := pklab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-54", "primary-key-design",
		"連番 BIGINT は追記で密、ランダム UUID は散らばって index 肥大＋挿入が重い")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 連番 BIGINT は木の右端に追記 → ページ分割ほぼ無し・挿入が最速・index 最小。 " +
			"2) ランダム UUID は挿入位置が散らばる → ページ分割で index 肥大・挿入が遅い。 " +
			"3) CHAR(36) は BINARY(16) より太く、二次索引が主キーを内包するぶん更に肥大。 " +
			"4) 時刻順 UUID(BINARY16) は追記に戻り、ランダムより速く小さい。")

	const tenant = "pk"
	const n = 100_000
	const chunk = 500

	type res struct {
		name              string
		dur               int64 // ms
		dataKB, indexKB   int64
	}
	run := func(name string, k pklab.Kind) res {
		d, err := pklab.Insert(ctx, db, k, tenant, n, chunk)
		if err != nil {
			t.Fatalf("%s insert: %v", name, err)
		}
		dl, il, err := pklab.Sizes(ctx, db, k)
		if err != nil {
			t.Fatalf("%s sizes: %v", name, err)
		}
		return res{name, d.Milliseconds(), dl / 1024, il / 1024}
	}

	bigint := run("BIGINT AUTO_INCREMENT（追記）", pklab.BigintAuto)
	uchar := run("UUID v4 CHAR(36)（散らばる・太い）", pklab.UUIDCharV4)
	ubin := run("UUID v4 BINARY(16)（散らばる・細い）", pklab.UUIDBinV4)
	uord := run("時刻順 UUID BINARY(16)（追記に戻す）", pklab.UUIDBinOrdered)

	for _, r := range []res{bigint, uchar, ubin, uord} {
		acc := r.name == uchar.name || r.name == ubin.name // ランダム勢を事故印に
		rec.Add(expkit.Variant{
			Name:     r.name,
			Accident: acc,
			Counters: map[string]int64{"insert_ms": r.dur, "data_KB": r.dataKB, "index_KB": r.indexKB},
			Notes:    []string{"挿入 " + itoa64(r.dur) + "ms / data " + itoa64(r.dataKB) + "KB / index " + itoa64(r.indexKB) + "KB"},
		})
	}
	t.Logf("bigint: %dms data=%dKB idx=%dKB", bigint.dur, bigint.dataKB, bigint.indexKB)
	t.Logf("uchar : %dms data=%dKB idx=%dKB", uchar.dur, uchar.dataKB, uchar.indexKB)
	t.Logf("ubin  : %dms data=%dKB idx=%dKB", ubin.dur, ubin.dataKB, ubin.indexKB)
	t.Logf("uord  : %dms data=%dKB idx=%dKB", uord.dur, uord.dataKB, uord.indexKB)

	// ---- 検証 ----
	// ランダム UUID は連番 BIGINT より挿入が遅い（ページ分割）。
	if uchar.dur <= bigint.dur {
		t.Errorf("ランダム CHAR UUID が BIGINT より遅くない: uuid=%dms bigint=%dms", uchar.dur, bigint.dur)
	}
	if ubin.dur <= bigint.dur {
		t.Errorf("ランダム BIN UUID が BIGINT より遅くない: uuid=%dms bigint=%dms", ubin.dur, bigint.dur)
	}
	// CHAR(36) は BINARY(16) より index が太い（二次索引が主キーを内包）。
	if uchar.indexKB <= ubin.indexKB {
		t.Errorf("CHAR(36) の index が BINARY(16) 以下（太いはず）: char=%dKB bin=%dKB", uchar.indexKB, ubin.indexKB)
	}
	// 時刻順 UUID はランダム BIN より速い（追記に戻る）。
	if uord.dur >= ubin.dur {
		t.Errorf("時刻順 UUID がランダムより速くない: ord=%dms rand=%dms", uord.dur, ubin.dur)
	}

	rec.Scope(
		"MySQL 8.0 InnoDB ROW_FORMAT=DYNAMIC / 各 10万行・chunk500 multi-row（往復は全種同じ）/ payload 80B",
		"主キーの順序だけが違う。挿入時間の差はページ分割・断片化、index の差は主キー幅×二次索引の内包",
		"data/index サイズは ANALYZE TABLE 後の information_schema 値（近似・ページ単位で粗い）",
	)
	rec.Uncertain(
		"絶対時間はこのホスト。差の向き（ランダム>連番）は buffer pool・行数で強弱が変わるが向きは安定",
		"UUID を主キーにしたいなら BINARY(16)＋時刻順（UUIDv7 相当）にすると追記の利点を保てる",
		"ランダム主キーは buffer pool に入り切る間は差が小さく、溢れると I/O で一気に開く（本実験は同一条件で相対比較）",
		"『分散生成したい/推測されたくない』なら UUID の価値はある。その場合も時刻順＋BINARY で肥大を抑える",
	)
	rec.Artifact(
		"internal/pklab: 4種の主キーで挿入時間と data/index サイズを測る",
		"docs/primary-key.md: 主キー設計（連番 vs UUID・肥大の実測）",
	)
	rec.Next("EXP-55 トランザクション分離レベル（RR vs RC）")

	files, err := rec.Save(
		"InnoDB の表は主キーの B-tree。連番 BIGINT は追記で密・最速・index 最小。ランダム UUID は挿入位置が" +
			"散らばりページ分割で挿入が遅く index が肥大する。さらに二次索引は主キーを内包するので、CHAR(36) は" +
			"BINARY(16) より全索引が太る。UUID を使うなら BINARY(16)＋時刻順（v7 相当）にして追記の利点を" +
			"取り戻す。『分散生成・推測されにくさ』が要る時だけ UUID を選び、その場合も肥大を抑える形にする。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func itoa64(n int64) string {
	if n < 0 {
		return "-" + itoa64(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa64(n/10) + string(rune('0'+n%10))
}
