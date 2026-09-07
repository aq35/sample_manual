package charsetlab_test

// EXP-57: utf8mb4 と index 長・照合(collation)。
//
//	MYSQL_DSN=... go test ./internal/charsetlab/ -run TestEXP57 -v
//
// utf8mb4 は1文字最大4バイト。InnoDB(DYNAMIC) の index キー上限は 3072 バイト。VARCHAR(768)*4=3072
// は張れるが 769 は超えて失敗(1071)。長い列は prefix 索引で回避。照合は等価判定・一意制約の意味を
// 変える（_ci は大小/アクセント無視、_bin は厳密）。全部を実挙動で確かめる。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/charsetlab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP57_utf8mb4とindex長(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-57", "utf8mb4-index-collation",
		"utf8mb4 の index 長上限(3072B)と照合(_ci/_bin)の意味を実挙動で示す")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) utf8mb4 は 4B/字。VARCHAR(768)=3072B は全長索引を張れる。 " +
			"2) VARCHAR(769)=3076B は 3072B 上限を超えて索引作成に失敗(1071)。 " +
			"3) 長い列でも prefix 索引 (col(255)) なら張れる。 " +
			"4) _0900_ai_ci は 'abc'='ABC'（大小無視）で一意制約も重複扱い。_bin は厳密で別物扱い。")

	// --- ① index 長の境界 ---
	ok768, tooLong768, err := charsetlab.TryFullIndex(ctx, db, 768)
	if err != nil {
		t.Fatal(err)
	}
	ok769, tooLong769, err := charsetlab.TryFullIndex(ctx, db, 769)
	if err != nil {
		t.Fatal(err)
	}
	okPfx, err := charsetlab.TryPrefixIndex(ctx, db, 1000, 255)
	if err != nil && !okPfx {
		t.Fatalf("prefix index: %v", err)
	}

	rec.Add(expkit.Variant{
		Name:     "VARCHAR(768) utf8mb4: 全長索引 OK（768×4=3072B・上限ちょうど）",
		Counters: map[string]int64{"full_index_ok": b2i(ok768)},
	})
	rec.Add(expkit.Variant{
		Name:     "VARCHAR(769) utf8mb4: 全長索引 失敗(1071)（3076B>3072B）",
		Accident: true,
		Counters: map[string]int64{"full_index_ok": b2i(ok769), "key_too_long_1071": b2i(tooLong769)},
		Notes:    []string{"1文字でも足すと 4B 増えて上限超過。型名でなくバイト長で決まる"},
	})
	rec.Add(expkit.Variant{
		Name:     "VARCHAR(1000) でも prefix 索引 (s(255)) なら OK",
		Counters: map[string]int64{"prefix_index_ok": b2i(okPfx)},
		Notes:    []string{"長い列の索引は prefix で。等価一意が要るなら生成列やハッシュ列を別途"},
	})

	// --- ② 照合(collation)の意味 ---
	ciMatch, ciDup, err := charsetlab.Collation(ctx, db, "utf8mb4_0900_ai_ci")
	if err != nil {
		t.Fatal(err)
	}
	binMatch, binDup, err := charsetlab.Collation(ctx, db, "utf8mb4_bin")
	if err != nil {
		t.Fatal(err)
	}

	rec.Add(expkit.Variant{
		Name:     "utf8mb4_0900_ai_ci: 'abc'='ABC'（大小無視）・一意制約も重複扱い",
		Counters: map[string]int64{"match_ABC_to_abc": b2i(ciMatch), "unique_dup_rejected": b2i(ciDup)},
	})
	rec.Add(expkit.Variant{
		Name:     "utf8mb4_bin: 'abc'≠'ABC'（厳密）・別物として両方入る",
		Counters: map[string]int64{"match_ABC_to_abc": b2i(binMatch), "unique_dup_rejected": b2i(binDup)},
	})
	t.Logf("index: 768ok=%v 769ok=%v(tooLong=%v) prefix=%v / coll ci(match=%v dup=%v) bin(match=%v dup=%v)",
		ok768, ok769, tooLong769, okPfx, ciMatch, ciDup, binMatch, binDup)
	_ = tooLong768

	// ---- 検証 ----
	if !ok768 {
		t.Errorf("VARCHAR(768) の全長索引が張れない（張れるはず・3072B ちょうど）")
	}
	if ok769 || !tooLong769 {
		t.Errorf("VARCHAR(769) が上限超過で失敗していない: ok=%v tooLong=%v", ok769, tooLong769)
	}
	if !okPfx {
		t.Errorf("prefix 索引が張れない（張れるはず）")
	}
	if !ciMatch || !ciDup {
		t.Errorf("_ci が大小無視になっていない: match=%v dup=%v", ciMatch, ciDup)
	}
	if binMatch || binDup {
		t.Errorf("_bin が厳密になっていない: match=%v dup=%v", binMatch, binDup)
	}

	rec.Scope(
		"MySQL 8.0 InnoDB ROW_FORMAT=DYNAMIC / index キー上限 3072B / utf8mb4=最大4B/字",
		"『型名でなくバイト長』で索引可否が決まる（VARCHAR(768) と (769) の境界）",
		"照合は列の COLLATE で決まり、等価比較・ORDER BY・一意制約すべてに効く",
	)
	rec.Uncertain(
		"3072B は DYNAMIC/COMPRESSED＋innodb_large_prefix(8.0 既定 on)の値。REDUNDANT/COMPACT だと 767B",
		"prefix 索引は前方一致・範囲には効くが、prefix より後ろの一意性は保証しない（完全一意はハッシュ列/生成列）",
		"_ci は人が読む名前の検索・重複防止に自然。ID・トークン・大小を区別したい値は _bin",
		"latin1/ascii は 1B/字で上限に余裕。多言語不要の列をむやみに utf8mb4 にすると索引長で詰まる",
	)
	rec.Artifact(
		"internal/charsetlab: TryFullIndex / TryPrefixIndex / Collation",
		"docs/charset.md: utf8mb4 の index 長と照合",
	)
	rec.Next("（未実験候補セットの最後）")

	files, err := rec.Save(
		"utf8mb4 は最大 4B/字で、InnoDB(DYNAMIC) の index キー上限 3072B に対し VARCHAR(768) は張れるが " +
			"769 は超えて失敗(1071)＝『型名でなくバイト長』で決まる。長い列は prefix 索引 (col(255)) で回避し、" +
			"完全一意が要るならハッシュ列/生成列を別に持つ。照合は _0900_ai_ci が大小/アクセント無視で人名検索や" +
			"重複防止に自然、_bin は厳密で ID・トークン向き。多言語不要の列をむやみに utf8mb4 にすると索引長で" +
			"詰まるので、列ごとに charset/collation を選ぶ。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
