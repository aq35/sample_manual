package dlqlab_test

// EXP-45: poison / dead-letter。必ず失敗する命令を無限リトライしない。
//
//	MYSQL_DSN=... go test ./internal/dlqlab/ -run TestEXP45 -v
//
// poison（必ず失敗）を含むキューを、上限なし（無限リトライ）と、上限→dead 隔離（DLQ）で処理し、
// キューが drain するか・poison の試行が有界か・良い命令が流れるかを比べる。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/dlqlab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP45_poison_deadletter(t *testing.T) {
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
	const tenant = "dlq"
	if err := dlqlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	rec := expkit.NewRecorder("EXP-45", "poison-dead-letter",
		"必ず失敗する命令を無限リトライせず、上限で dead に隔離する")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) 上限なし: poison は pending のまま残り、周回のたびに再試行され続ける（キューが drain しない・" +
			"試行が青天井）。良い命令は done になる（先頭で止めない前提）。 " +
			"2) 上限→dead(DLQ): poison は maxAttempts 回で dead に隔離され、キューは drain（pending=0）。" +
			"試行は maxAttempts で有界。良い命令は done。")

	const n = 10
	const poisonID = 3
	isPoison := func(id int64) bool { return id == poisonID }

	// ---- ① 上限なし（無限リトライ）----
	if err := dlqlab.Seed(ctx, db, tenant, n); err != nil {
		t.Fatal(err)
	}
	const passes = 8 // 無限を模して 8 周だけ回す（実際は永遠に drain しない）
	if err := dlqlab.Process(ctx, db, tenant, isPoison, 0 /*maxAttempts*/, false /*useDLQ*/, passes); err != nil {
		t.Fatal(err)
	}
	p1, d1, dead1, att1, err := dlqlab.Stats(ctx, db, tenant, poisonID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "上限なし: poison が pending に残り再試行が青天井・キューが drain しない",
		Accident: true,
		Counters: map[string]int64{"pending": int64(p1), "done": int64(d1), "dead": int64(dead1), "poison_attempts": int64(att1)},
		Notes:    []string{"poison は pending のまま（" + itoa(p1) + " 件）試行 " + itoa(att1) + " 回（周回数ぶん・止まらない）。良い命令は done " + itoa(d1)},
	})
	t.Logf("no-dlq: pending=%d done=%d dead=%d poison_attempts=%d (passes=%d)", p1, d1, dead1, att1, passes)

	// ---- ② 上限→dead（DLQ）----
	if err := dlqlab.Seed(ctx, db, tenant, n); err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 3
	if err := dlqlab.Process(ctx, db, tenant, isPoison, maxAttempts, true /*useDLQ*/, passes); err != nil {
		t.Fatal(err)
	}
	p2, d2, dead2, att2, err := dlqlab.Stats(ctx, db, tenant, poisonID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(expkit.Variant{
		Name:     "上限→dead(DLQ): poison を maxAttempts で隔離・キューは drain・試行は有界",
		Counters: map[string]int64{"pending": int64(p2), "done": int64(d2), "dead": int64(dead2), "poison_attempts": int64(att2), "max_attempts": maxAttempts},
		Notes:    []string{"poison は " + itoa(att2) + " 回で dead(" + itoa(dead2) + ")。pending " + itoa(p2) + "・done " + itoa(d2) + "（キュー drain）"},
	})
	t.Logf("dlq: pending=%d done=%d dead=%d poison_attempts=%d (max=%d)", p2, d2, dead2, att2, maxAttempts)

	// ---- 検証 ----
	// 上限なし: poison が残り、試行が周回数ぶん（有界でない）、良い命令は done
	if p1 != 1 || dead1 != 0 {
		t.Errorf("上限なしで poison が残っていない/隔離された: pending=%d dead=%d", p1, dead1)
	}
	if att1 != passes {
		t.Errorf("上限なしで試行が周回数ぶんでない（青天井のはず）: %d（%d）", att1, passes)
	}
	if d1 != n-1 {
		t.Errorf("上限なしで良い命令が流れていない: done=%d（%d のはず）", d1, n-1)
	}
	// DLQ: poison は maxAttempts で dead、pending=0、done=n-1、試行は有界
	if dead2 != 1 || p2 != 0 {
		t.Errorf("DLQ で隔離/drain できていない: dead=%d pending=%d", dead2, p2)
	}
	if att2 != maxAttempts {
		t.Errorf("DLQ で試行が有界でない: %d（%d のはず）", att2, maxAttempts)
	}
	if d2 != n-1 {
		t.Errorf("DLQ で良い命令が流れていない: done=%d（%d のはず）", d2, n-1)
	}

	rec.Scope(
		"MySQL 8.0 / 10命令中 1つが poison(必ず失敗) / 無限は 8 周で代表・DLQ は maxAttempts=3",
		"失敗しても後続は処理する（先頭で止めない）ので良い命令は poison に依らず流れる",
		"隔離＝state を dead に落とす。実運用は dead をアラートし人が調べる",
	)
	rec.Uncertain(
		"実際の無限リトライは永遠に drain しない（ここは 8 周で代表・試行が周回数に比例することを示す）",
		"先頭で止める実装（strict 順序）なら poison が後続もブロックする＝さらに悪い。ここは skip 継続前提",
		"バックオフ（EXP-17）と併用: 隔離までの間も間隔を伸ばして DB/外部を守る",
		"dead の再投入（原因修正後の replay）は運用手順（本実験外）",
	)
	rec.Artifact(
		"internal/dlqlab: 試行上限つき処理と dead 隔離",
		"docs/dead-letter.md: poison / dead-letter の扱い",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"必ず失敗する命令(poison)は無限リトライしない。試行上限(maxAttempts)を決め、超えたら dead に隔離" +
			"（アラート）してキューを drain させる。良い命令は先頭で止めず処理を続けるので poison に依らず流れる。" +
			"バックオフ(EXP-17)と併用して隔離までの負荷も抑える。dead は原因修正後に replay する運用にする。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
