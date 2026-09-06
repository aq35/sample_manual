package credlab_test

// EXP-13: DB 資格情報のローテーション。
//
//	MYSQL_DSN=... go test ./internal/credlab/ -run TestEXP13 -v
//
// DB のユーザー/パスワードが期限切れ（ローテーション）するとき、接続エラーを出さずに
// 切り替えられるか。abrupt（古いプールのまま）と graceful（新プールへ差し替え）を比べる。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/credlab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP13_資格情報のローテーション(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	base := mysqltest.DSN(t)

	// root でユーザー管理できることが前提（socket 経由）。できなければ skip。
	if err := credlab.SetupUser("pw_v1"); err != nil {
		t.Skipf("テスト用ユーザーを作れない（root socket 不可）: %v", err)
	}
	t.Cleanup(func() { _ = credlab.DropUser() })

	rec := expkit.NewRecorder("EXP-13", "credential-rotation",
		"DB 資格情報のローテーション: abrupt と graceful で切り替え中の接続エラーを比べる")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(strings.Join([]string{
		"1) 既に張られた接続はパスワードを変えても切れない。期限が効くのは新しい接続を張るとき。",
		"2) abrupt（古いプールのまま新パスワードへ変える）と ConnMaxLifetime が短いと、",
		"   接続が張り替わるたびに旧パスワードで認証され、接続エラーが出る。",
		"3) graceful（二重パスワードで両方有効にし、新プールを作り原子的に差し替え、古いをドレイン）なら、",
		"   切り替え中も接続エラー 0 で移行できる。移行後に古いパスワードを無効化する。",
	}, " "))
	rec.Workload("concurrency", 8).Workload("conn_lifetime", "150ms").
		Injection("rotate", "負荷の途中で ALTER USER でパスワードを変える")

	const life = 150 * time.Millisecond // 資格情報の期限より短い想定

	// ---- ① abrupt: 古いプールのまま、パスワードだけ変える（事故）----
	abrupt := func() credlab.LoadResult {
		dsn, _ := credlab.DSN(base, "pw_v1")
		p, err := credlab.OpenPool(dsn, life)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		h := credlab.NewHolder(p)
		done := make(chan credlab.LoadResult, 1)
		go func() { done <- credlab.RunLoad(ctx, h, 8, 2*time.Second) }()
		time.Sleep(600 * time.Millisecond)
		// パスワードをローテーション。古いプールは何もしない（＝間違ったやり方）。
		if err := credlab.Rotate("pw_v2"); err != nil {
			t.Fatal(err)
		}
		res := <-done
		return res
	}()
	rec.Add(expkit.Variant{
		Name:     "abrupt（古いプールのまま ALTER USER）",
		Desc:     "ConnMaxLifetime で接続が張り替わると、旧パスワードで認証され弾かれる",
		Accident: abrupt.AuthErr == 0, // エラーが出ないと counter-proof にならない
		Counters: map[string]int64{"ops": abrupt.Ops, "auth_err": abrupt.AuthErr, "other_err": abrupt.OtherErr},
		Notes: []string{
			"接続が張り替わるたびに Error 1045（Access denied）。プールを作り直さないとこうなる",
		},
	})
	t.Logf("abrupt   ops=%d auth_err=%d other=%d", abrupt.Ops, abrupt.AuthErr, abrupt.OtherErr)

	// exp13 を pw_v1 に戻す（次の実験のため）
	if err := credlab.Rotate("pw_v1"); err != nil {
		t.Fatal(err)
	}

	// ---- ② graceful: 新パスワードで新プールを作り、原子的に差し替え、古いプールをドレイン ----
	graceful := func() credlab.LoadResult {
		dsn, _ := credlab.DSN(base, "pw_v1")
		p, err := credlab.OpenPool(dsn, life)
		if err != nil {
			t.Fatal(err)
		}
		h := credlab.NewHolder(p)
		done := make(chan credlab.LoadResult, 1)
		go func() { done <- credlab.RunLoad(ctx, h, 8, 2*time.Second) }()
		time.Sleep(600 * time.Millisecond)
		// ★二重パスワードでローテーション（古い pw_v1 も新しい pw_v2 も両方有効な期間を作る）。
		if err := credlab.RotateDual("pw_v2"); err != nil {
			t.Fatal(err)
		}
		// その間に graceful に入れ替える（新プール ping → swap → 古いをドレイン）。
		if err := credlab.GracefulSwap(h, base, "pw_v2", life, 500*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		res := <-done
		// 移行が終わったので古いパスワードを無効化する。
		if err := credlab.DiscardOldPassword(); err != nil {
			t.Fatal(err)
		}
		return res
	}()
	rec.Add(expkit.Variant{
		Name:     "graceful（新プールへ原子的に差し替え＋ドレイン）",
		Desc:     "二重パスワードで両方有効な間に swap。新プールを ping で確かめ、古いプールは使い終わってから閉じる",
		Counters: map[string]int64{"ops": graceful.Ops, "auth_err": graceful.AuthErr, "other_err": graceful.OtherErr},
		Notes: []string{
			"切り替え中も接続エラー 0。config.ManagedSecret の onRotate からこれを呼べば自動化できる",
		},
	})
	t.Logf("graceful ops=%d auth_err=%d other=%d", graceful.Ops, graceful.AuthErr, graceful.OtherErr)

	// 後片付け
	_ = credlab.Rotate("pw_v1")

	// ---- 検証 ----
	if abrupt.AuthErr == 0 {
		t.Errorf("abrupt で認証エラーが1件も出ていない（counter-proof が成立しない）。ConnMaxLifetime を短くする")
	}
	if graceful.AuthErr != 0 {
		t.Errorf("graceful なのに認証エラーが %d 件出た（0 のはず）", graceful.AuthErr)
	}

	rec.Scope(
		"MySQL 8.0 / 単一ホスト / exp13 ユーザーを root(socket) で作成・ALTER",
		"接続エラー = Error 1045 (Access denied)。新接続の認証失敗を数える",
		"ConnMaxLifetime=150ms で接続の張り替えを短時間に起こしている（本番の期限はもっと長い）",
	)
	rec.Uncertain(
		"実際の SecretManager 連携（config.ManagedSecret.onRotate → GracefulSwap）の結線は本実験外",
		"レプリカ・複数ホストの資格情報同時ローテーションは未検証",
		"接続数を一斉に張り替えたときの DB 側負荷（EXP-5 の飽和）は本実験の対象外",
	)
	rec.Artifact(
		"internal/credlab: プールの原子的差し替え（Holder.Swap）と GracefulSwap",
		"config.ManagedSecret: 期限前に先回り更新し onRotate を呼ぶ → GracefulSwap に繋げる",
	)
	rec.Next("なし")

	files, err := rec.Save(strings.Join([]string{
		"資格情報のローテーションは「古いプールのままパスワードを変える」と接続エラーになる。",
		"新パスワードで新プールを作り、ping で確かめてから原子的に差し替え、古いプールをドレインすれば、",
		"切り替え中も接続エラー 0 で移行できる。ConnMaxLifetime は資格情報の期限より短くする。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

var _ = sql.ErrNoRows
