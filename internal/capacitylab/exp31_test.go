package capacitylab_test

// EXP-31: 1 vCPU / 2GB の ECS タスクで、SSE は何本・Worker はどれくらい捌けるか。
//
//	MYSQL_DSN=... go test ./internal/capacitylab/ -run TestEXP31 -v
//
// 実測できる単位コストから見積もる:
//   ・1接続あたりのメモリ → メモリ律速の SSE 本数（2GB からランタイム/GC の余白を引いた予算 ÷ 1本）。
//   ・単一プロセスの DB 往復スループット（並行度別）→ DB 律速の Worker 処理量。
// CPU 律速の rps はホスト依存なのでモデルで扱う（サンドボックスの CPU は本番と違う）。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/capacitylab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

func TestEXP31_1タスクの容量(t *testing.T) {
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

	rec := expkit.NewRecorder("EXP-31", "task-capacity",
		"1 vCPU / 2GB タスクの SSE 本数・Worker 処理量の見積もり")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"1) SSE は idle でも 1接続 = goroutine + バッファのメモリを食う。メモリ予算 ÷ 1接続 が上限の目安。 " +
			"2) SSE は DB 接続を1:1で持たない（持てば接続予算ですぐ枯れる）。push は共有ハブから。 " +
			"3) Worker は短い DB 往復の繰り返し。処理量 ≒ 並行度 × (1/往復遅延)。CPU でなく DB で頭打ち。 " +
			"4) CPU 律速の rps はホスト依存。ここでは測らずモデルで扱う。")

	// ---- ① 1接続あたりのメモリ（バッファ規模を変えて）----
	const n = 20000
	memBare := capacitylab.ConnMem(n, 0)         // goroutine + channel だけ（下限）
	memHTTP := capacitylab.ConnMem(n, 8*1024)    // 小さめの HTTP 書き込みバッファ相当
	memTLS := capacitylab.ConnMem(n, 32*1024)    // TLS 読み書き + HTTP バッファ相当（現実的）
	rec.Workload("sample_conns", n)
	rec.Add(expkit.Variant{
		Name:    "1接続メモリ: goroutine のみ（下限）",
		Metrics: map[string]float64{"bytes_per_conn": memBare, "kib_per_conn": memBare / 1024},
	})
	rec.Add(expkit.Variant{
		Name:    "1接続メモリ: + 8KB バッファ",
		Metrics: map[string]float64{"bytes_per_conn": memHTTP, "kib_per_conn": memHTTP / 1024},
	})
	rec.Add(expkit.Variant{
		Name:    "1接続メモリ: + 32KB バッファ（TLS 現実的）",
		Metrics: map[string]float64{"bytes_per_conn": memTLS, "kib_per_conn": memTLS / 1024},
	})
	t.Logf("1接続メモリ: bare=%.1fKB / +8KB=%.1fKB / +32KB=%.1fKB",
		memBare/1024, memHTTP/1024, memTLS/1024)

	// 2GB のうち、ランタイム/GC/アプリで ~800MB を残し、~1.2GB を接続に使えると仮定した見積もり。
	const connBudget = 1200 * 1024 * 1024
	sseBare := float64(connBudget) / maxf(memBare, 1)
	sseTLS := float64(connBudget) / maxf(memTLS, 1)
	rec.Add(expkit.Variant{
		Name:     "見積もり: SSE 本数（接続予算 1.2GB ÷ 1接続）",
		Counters: map[string]int64{"sse_bare_floor": int64(sseBare), "sse_tls_realistic": int64(sseTLS)},
		Notes: []string{
			"下限バッファなら約 " + itoa(int64(sseBare)) + " 本、TLS 現実的なら約 " + itoa(int64(sseTLS)) + " 本",
			"実運用は fd 上限(ulimit)・GC 余白・安全率で、この 1/2〜1/3 を上限に設定するのが無難",
		},
	})
	t.Logf("SSE 見積もり: bare≒%.0f / TLS≒%.0f 本（予算1.2GB）", sseBare, sseTLS)

	// ---- ② Worker: 単一プロセスの DB 往復スループット（並行度別）----
	const dur = 800 * time.Millisecond
	var thr []int64
	for _, c := range []int{1, 4, 16, 32} {
		ops := capacitylab.OpThroughput(ctx, db, c, dur, "SELECT 1")
		thr = append(thr, int64(ops))
		rec.Add(expkit.Variant{
			Name:     "Worker 往復/秒: 並行度 " + itoa(int64(c)),
			Counters: map[string]int64{"concurrency": int64(c), "ops_per_sec": int64(ops)},
		})
		t.Logf("Worker 往復/秒 @concurrency=%d: %.0f", c, ops)
	}

	// ---- 検証（意味のある値が取れているか）----
	if memTLS <= memBare {
		t.Errorf("バッファ分がメモリに乗っていない: bare=%.0f tls=%.0f", memBare, memTLS)
	}
	if memTLS < 30*1024 { // 32KB バッファが概ね乗るはず
		t.Errorf("1接続メモリが小さすぎる（測定が疑わしい）: %.0f", memTLS)
	}
	if thr[len(thr)-1] <= thr[0] { // 並行度を上げれば往復/秒は増える（IO 待ちが主）
		t.Errorf("並行度を上げても往復/秒が増えていない: %v", thr)
	}

	rec.Scope(
		"Go1.25 / メモリは HeapAlloc+StackInuse の増分を実測 / 往復は同ホスト MySQL への SELECT 1",
		"接続予算は 2GB - 約800MB(ランタイム/GC/アプリ) ≒ 1.2GB と仮定した見積もり",
		"往復/秒はこのホストの DB のもの。本番 DB・ネットワークでは往復遅延が変わる",
	)
	rec.Uncertain(
		"CPU 律速の rps（JSON 整形・圧縮・暗号）は 1 vCPU の実機で測ること。ここでは扱わない",
		"SSE の実メモリは TLS 実装・書き込みバッファ・アプリの per-conn 状態で動く（±数十KB）",
		"往復遅延は本番の RTT 次第。Worker 処理量 = 並行度 × (1/往復遅延) で往復遅延に強く依存",
		"横に増やす（レプリカ）ときの上限は接続予算（EXP-5/poolbudget）と DB 自体（EXP-30）",
	)
	rec.Artifact(
		"internal/capacitylab: 1接続メモリと DB 往復スループットの実測",
		"docs/capacity.md: 1 vCPU/2GB タスクの容量見積もり（SSE 本数・Worker 処理量）",
	)
	rec.Next("なし")

	files, err := rec.Save(
		"1タスクの上限は『CPU』より先に『メモリ（SSE 本数）』と『DB（Worker 往復）』で決まりやすい。" +
			"SSE はメモリ律速: 予算 ÷ 1接続（TLS 込みで数万本の上限、安全率で 1/2〜1/3）。DB 接続は 1:1 で持たない。" +
			"Worker は DB 律速: 並行度 × (1/往復遅延)。足りなければ縦に上げるより横に増やす（EXP-30）。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func itoa(n int64) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
