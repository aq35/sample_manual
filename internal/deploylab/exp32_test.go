package deploylab_test

// EXP-32: ゼロダウンタイムのスキーマ変更（expand/contract）。
//
//	MYSQL_DSN=... go test ./internal/deploylab/ -run TestEXP32 -v
//
// ローリングデプロイ中、旧(v1)と新(v2)が同じ DB を同時に触る。列 status を state へ改名する。
// 一気に張り替えると旧が壊れる。expand→両対応→contract なら、どの瞬間も動く。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/deploylab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP32_expand_contract(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-32", "expand-contract",
		"ローリングデプロイ中の無停止スキーマ変更（status→state 改名）")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 列を一気に張り替える（CHANGE status state）と、その瞬間から旧アプリ v1 の SELECT status が壊れる。 " +
			"2) expand（state を足して backfill）すれば、v1 は status のまま無傷、v2 は state を使える。 " +
			"3) 移行期は両方に書く（dual-write）ので、v1 も v2 も読める。 " +
			"4) contract（status を落とす）は v1 が全て退役した後にだけ。先にやると v1 が壊れる。")

	// ---- ① 危険: 一気に張り替える → v1 が壊れる ----
	if err := deploylab.Reset(ctx, db); err != nil {
		t.Fatal(err)
	}
	v1before := deploylab.V1Read(ctx, db) // 変更前は v1 OK
	if err := deploylab.RenameInOneStep(ctx, db); err != nil {
		t.Fatal(err)
	}
	v1afterRename := deploylab.V1Read(ctx, db) // 改名直後、v1 は status を読めず壊れる
	rec.Add(expkit.Variant{
		Name:     "危険: 一気に CHANGE status→state（デプロイ中に v1 が壊れる）",
		Accident: true,
		Counters: map[string]int64{"v1_ok_before": okI(v1before), "v1_ok_after": okI(v1afterRename)},
		Notes:    []string{"改名後の v1 error: " + errStr(v1afterRename)},
	})
	t.Logf("一気張替: v1 before=%v / after=%v", v1before, v1afterRename)

	// ---- ② 安全: expand → 両対応 → contract ----
	if err := deploylab.Reset(ctx, db); err != nil {
		t.Fatal(err)
	}
	// expand: state を足して backfill。v1 はまだ status で無傷。
	if err := deploylab.Expand(ctx, db); err != nil {
		t.Fatal(err)
	}
	v1Exp := deploylab.V1Read(ctx, db)
	v1WExp := deploylab.V1Write(ctx, db, 10)
	rec.Add(expkit.Variant{
		Name:     "安全①expand: state を足す → v1 は無傷（status のまま読み書きできる）",
		Counters: map[string]int64{"v1_read_ok": okI(v1Exp), "v1_write_ok": okI(v1WExp)},
	})

	// 移行期: v2 は dual-write（両方に書く）、state を読む。v1 と v2 が同時に動く。
	v2W := deploylab.V2WriteDual(ctx, db, 20)
	v2R := deploylab.V2ReadState(ctx, db)
	v1Co := deploylab.V1Read(ctx, db)
	rec.Add(expkit.Variant{
		Name:     "安全②移行期: v2 は dual-write＋state 読み / v1 も status 読み → 両方 OK",
		Counters: map[string]int64{"v2_write_ok": okI(v2W), "v2_read_ok": okI(v2R), "v1_read_ok": okI(v1Co)},
	})

	// contract: v1 が退役した後、status を落とす。v2 は state だけで動く。
	if err := deploylab.Contract(ctx, db); err != nil {
		t.Fatal(err)
	}
	v2After := deploylab.V2ReadState(ctx, db)
	v2WAfter := deploylab.V2WriteStateOnly(ctx, db, 30)
	v1After := deploylab.V1Read(ctx, db) // contract 後は v1 は壊れる（が、もういない）
	rec.Add(expkit.Variant{
		Name:     "安全③contract: status を落とす → v2 は無傷。v1 はもう壊れる（退役後だから可）",
		Counters: map[string]int64{"v2_read_ok": okI(v2After), "v2_write_ok": okI(v2WAfter), "v1_ok": okI(v1After)},
		Notes:    []string{"contract 後の v1 error: " + errStr(v1After) + "（v1 は退役済みなので問題ない）"},
	})
	t.Logf("expand/contract: v1(expand)=%v / 移行 v2R=%v v1R=%v / contract v2R=%v v1R=%v",
		v1Exp, v2R, v1Co, v2After, v1After)

	// ---- 検証 ----
	if v1before != nil {
		t.Errorf("変更前に v1 が動いていない: %v", v1before)
	}
	if v1afterRename == nil {
		t.Errorf("一気張替でも v1 が壊れていない（壊れるはず）")
	}
	if v1Exp != nil || v1WExp != nil {
		t.Errorf("expand で v1 が壊れた（無傷のはず）: read=%v write=%v", v1Exp, v1WExp)
	}
	if v2W != nil || v2R != nil || v1Co != nil {
		t.Errorf("移行期に v1/v2 のどれかが壊れた: v2W=%v v2R=%v v1=%v", v2W, v2R, v1Co)
	}
	if v2After != nil || v2WAfter != nil {
		t.Errorf("contract 後に v2 が壊れた: read=%v write=%v", v2After, v2WAfter)
	}
	if v1After == nil {
		t.Errorf("contract 後も v1 が動いてしまう（status は消えたはず）")
	}

	rec.Scope(
		"MySQL 8.0 / 列 status→state の改名を例に / v1=status を読み書き, v2=state を使う",
		"デプロイ中は v1 と v2 が同じ DB を同時に触る前提",
		"MySQL 8.0 の ADD/DROP COLUMN は多くが online DDL（別途 EXP でオンライン挙動は測る）",
	)
	rec.Uncertain(
		"大きな表での ALTER の所要・ロックは表サイズと版で変わる（online DDL / gh-ost は別）",
		"dual-write の一貫性は、書き込み経路が1つ（このアプリ）である前提。複数書き手なら別途",
		"backfill は小表で一括。1回で終わらない規模ではチャンク分割・進捗管理が要る",
	)
	rec.Artifact(
		"internal/deploylab: v1/v2 の読み書きと expand/contract の DDL 手順",
		"docs/zero-downtime-migration.md: 無停止スキーマ変更の手順",
	)
	rec.Next("EXP-33 DB フェイルオーバ/再起動への耐性")

	files, err := rec.Save(
		"スキーマ変更は『足す→両対応→切替→消す』（expand/contract）で無停止にする。" +
			"列の一気張り替え・先に消す、は旧アプリを即壊す。追加は nullable で、移行期は dual-write、" +
			"消すのは旧バージョンが全て退役した後にだけ。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func okI(err error) int64 {
	if err == nil {
		return 1
	}
	return 0
}
func errStr(err error) string {
	if err == nil {
		return "(なし)"
	}
	return err.Error()
}
