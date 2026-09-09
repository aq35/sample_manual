package assignlab_test

// EXP-63: テナント割り当てを DB の lease で持つ — 均等・失敗時全責任・二重所有0・fence 単調。
//
//	MYSQL_DSN=... go test ./internal/assignlab/ -run TestEXP63 -v
//	EXP_RECORD=1 MYSQL_DSN=... go test ./internal/assignlab/ -run TestEXP63   # receipt を保存
//
// 各 worker は毎ティック「target=CEIL(全テナント/生存worker)」を計算し target まで claim・超えたら shed。
// ① 平常（2台生存）→ 10/10 に収束（動的 claim で均等）
// ② A 死亡 → live が 2→1、target が 10→20 に上がり B が全20を自動で担当（特別コード不要）
// ③ A' 復帰 → target が 10 に戻り B が shed・A' が claim → 10/10 に再収束
// ④ 対照：静的ピン（owner 固定）だと A 死亡で A担当の10が宙に浮く（orphan=10・2台の意味が消える）
// 全体を通して 二重所有=0（tenant_id が PK）・fence は単調非減少（古い担当の上書きを弾く）。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/assignlab"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/mysqltest"
)

const (
	wA      = "worker-A"
	wB      = "worker-B"
	nTenant = 20
	ttlSec  = 2 // lease TTL（秒）
	staleS  = 1 // 心拍の失効判定（秒）。stale ≒ lease と揃える
)

func TestEXP63_テナント割り当てをleaseで(t *testing.T) {
	mysqltest.Serialize(t)
	ctx := context.Background()
	db, err := sql.Open("mysql", mysqltest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL に繋がらない: %v", err)
	}
	if err := assignlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}

	// 1ティック = 心拍 → renew → target 計算 → 過少なら claim / 過多なら shed
	dynamicTick := func(worker string) (held, target, live int) {
		if err := assignlab.Heartbeat(ctx, db, worker); err != nil {
			t.Fatal(err)
		}
		if err := assignlab.RenewLeases(ctx, db, worker, ttlSec); err != nil {
			t.Fatal(err)
		}
		live, _ = assignlab.LiveWorkers(ctx, db, staleS)
		total, _ := assignlab.TotalTenants(ctx, db)
		target = assignlab.Target(total, live)
		held, _ = assignlab.HeldCount(ctx, db, worker)
		switch {
		case held < target:
			if _, err := assignlab.ClaimUpTo(ctx, db, worker, target-held, ttlSec, false); err != nil {
				t.Fatal(err)
			}
		case held > target:
			if _, err := assignlab.ShedDown(ctx, db, worker, held-target); err != nil {
				t.Fatal(err)
			}
		}
		held, _ = assignlab.HeldCount(ctx, db, worker)
		return
	}

	rec := expkit.NewRecorder("EXP-63", "tenant-assignment-lease",
		"テナント割り当てを DB lease で持つ：均等・失敗時全責任・二重所有0・fence 単調")
	rec.Env(expkit.CaptureEnv(ctx, db))
	rec.Freeze(
		"割り当てを DB の1テーブル（1テナント1行の lease）に持ち、各 worker が target=CEIL(全/生存) まで " +
			"claim・超えたら shed するだけで：① 平常は 10/10 に収束（均等）、② A 死亡で live↓→target↑ により " +
			"B が全20を自動担当（失敗時全責任・特別コード不要）、③ A' 復帰で 10/10 に再収束。全体で二重所有=0" +
			"（tenant_id が PK）・fence は単調非減少。④ 静的ピンは A 死亡で担当10が宙に浮く（orphan=10）。")

	if err := assignlab.SeedTenants(ctx, db, nTenant, false, wA, wB); err != nil {
		t.Fatal(err)
	}

	// ---- ① 平常：2台生存 → 均等（10/10）----
	// 先に両者が心拍を打って live=2 を成立させてから claim（でないと先発が target=20 を取ってしまう）
	if err := assignlab.Heartbeat(ctx, db, wA); err != nil {
		t.Fatal(err)
	}
	if err := assignlab.Heartbeat(ctx, db, wB); err != nil {
		t.Fatal(err)
	}
	hA, tgt, live := dynamicTick(wA)
	hB, _, _ := dynamicTick(wB)
	orphan1, _ := assignlab.OrphanCount(ctx, db)
	fence1, _ := assignlab.FenceMap(ctx, db)
	t.Logf("① 平常: live=%d target=%d A=%d B=%d orphan=%d", live, tgt, hA, hB, orphan1)

	rec.Add(expkit.Variant{
		Name: "① 平常（2台生存）: 動的 claim で均等 10/10",
		Counters: map[string]int64{
			"live": int64(live), "target": int64(tgt), "held_A": int64(hA), "held_B": int64(hB),
			"orphan": int64(orphan1),
		},
		Notes: []string{"target=CEIL(20/2)=10。A=" + itoa(hA) + " B=" + itoa(hB) + "＝均等・宙に浮くテナント0"},
	})

	// ---- ② A 死亡 → B が全責任（20）----
	// A はティックを止める（心拍も renew も止まる）。B だけがティックし続ける。
	// 実時間で A の心拍が stale（1s）になり、A の lease が失効（2s）すると、B の live=1→target=20 で全部拾う。
	deadline := time.Now().Add(6 * time.Second)
	var hB2, tgtB2, liveB2 int
	for time.Now().Before(deadline) {
		time.Sleep(400 * time.Millisecond)
		hB2, tgtB2, liveB2 = dynamicTick(wB) // B は生き続ける（心拍＋renew＋claim）
		if hB2 == nTenant {
			break
		}
	}
	own2, _ := assignlab.Ownership(ctx, db)
	orphan2, _ := assignlab.OrphanCount(ctx, db)
	fence2, _ := assignlab.FenceMap(ctx, db)
	t.Logf("② A死亡: live=%d target=%d B=%d A(own2)=%d orphan=%d", liveB2, tgtB2, hB2, own2[wA], orphan2)

	rec.Add(expkit.Variant{
		Name: "② A 死亡 → survivor B が全責任（自動・特別コード不要）",
		Counters: map[string]int64{
			"live": int64(liveB2), "target": int64(tgtB2), "held_B": int64(hB2),
			"held_A_stale": int64(own2[wA]), "orphan": int64(orphan2),
		},
		Notes: []string{"live 2→" + itoa(liveB2) + " で target 10→" + itoa(tgtB2) + "。B が失効した A担当を claim し held=" + itoa(hB2) + "、orphan=0"},
	})

	// ---- ③ A' 復帰 → 10/10 に再収束 ----
	var hA3, hB3, liveR int
	for i := 0; i < 12; i++ {
		hB3, _, liveR = dynamicTick(wB) // B: target 10 に戻り shed
		hA3, _, _ = dynamicTick(wA)     // A': 空いた分を claim
		time.Sleep(120 * time.Millisecond)
		if abs(hA3-hB3) <= 1 && hA3+hB3 == nTenant {
			break
		}
	}
	own3, _ := assignlab.Ownership(ctx, db)
	orphan3, _ := assignlab.OrphanCount(ctx, db)
	fence3, _ := assignlab.FenceMap(ctx, db)
	t.Logf("③ A'復帰: live=%d A=%d B=%d orphan=%d", liveR, hA3, hB3, orphan3)

	rec.Add(expkit.Variant{
		Name: "③ A' 復帰 → 10/10 に自動リバランス（shed＋claim）",
		Counters: map[string]int64{
			"live": int64(liveR), "held_A": int64(hA3), "held_B": int64(hB3), "orphan": int64(orphan3),
		},
		Notes: []string{"target 20→10 で B が shed、A' が claim。A=" + itoa(hA3) + " B=" + itoa(hB3)},
	})

	// fence 単調性（①→②→③ で各テナント非減少）＋二重所有0
	monotone, maxFence := checkMonotone(fence1, fence2, fence3)
	sum3 := own3[wA] + own3[wB]
	rec.Add(expkit.Variant{
		Name: "全体不変条件: 二重所有0（PK）・fence 単調非減少",
		Counters: map[string]int64{
			"double_owned": 0, "valid_owned_sum": int64(sum3), "tenants": int64(nTenant),
			"fence_monotone": b2i(monotone), "max_fence": maxFence,
		},
		Notes: []string{"tenant_id が PK＝1テナント1owner（二重所有は構造的に不可能）。fence は claim ごと+1 で単調（最大" + itoa(int(maxFence)) + "）"},
	})

	// ---- ④ 対照：静的ピンは失敗時に担当が宙に浮く ----
	if err := assignlab.Setup(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := assignlab.SeedTenants(ctx, db, nTenant, true, wA, wB); err != nil { // 前半A・後半Bにピン
		t.Fatal(err)
	}
	// 各自 自分のピン分だけを claim（静的ポリシー）
	if err := assignlab.Heartbeat(ctx, db, wA); err != nil {
		t.Fatal(err)
	}
	if err := assignlab.Heartbeat(ctx, db, wB); err != nil {
		t.Fatal(err)
	}
	if _, err := assignlab.ClaimUpTo(ctx, db, wA, nTenant, ttlSec, true); err != nil {
		t.Fatal(err)
	}
	if _, err := assignlab.ClaimUpTo(ctx, db, wB, nTenant, ttlSec, true); err != nil {
		t.Fatal(err)
	}
	pinnedA, _ := assignlab.HeldCount(ctx, db, wA)
	// A 死亡：ピン分の lease が失効するまで待つ。B は静的ポリシーなので「自分のピン分」しか claim しない。
	time.Sleep(time.Duration(ttlSec+1) * time.Second)
	if err := assignlab.Heartbeat(ctx, db, wB); err != nil {
		t.Fatal(err)
	}
	if err := assignlab.RenewLeases(ctx, db, wB, ttlSec); err != nil {
		t.Fatal(err)
	}
	// B は自分のピン分だけ claim（A のピン分は対象外＝拾わない）
	if _, err := assignlab.ClaimUpTo(ctx, db, wB, nTenant, ttlSec, true); err != nil {
		t.Fatal(err)
	}
	orphanStatic, _ := assignlab.OrphanCount(ctx, db)
	t.Logf("④ 静的ピン A死亡: A担当だった=%d orphan=%d（B は自分のピン分しか拾わない）", pinnedA, orphanStatic)

	rec.Add(expkit.Variant{
		Name:     "④ 対照：静的ピン → A 死亡で A担当が宙に浮く（2台の意味が消える）",
		Accident: true,
		Counters: map[string]int64{"pinned_A": int64(pinnedA), "orphan_after_A_dead": int64(orphanStatic)},
		Notes:    []string{"owner を固定すると B は A のピン分を拾えず orphan=" + itoa(orphanStatic) + "（動的 claim なら0＝②で実証）"},
	})

	// ---- 検証 ----
	if !(hA == 10 && hB == 10 && orphan1 == 0) {
		t.Errorf("① 均等でない: A=%d B=%d orphan=%d", hA, hB, orphan1)
	}
	if !(hB2 == nTenant && liveB2 == 1 && tgtB2 == nTenant && orphan2 == 0) {
		t.Errorf("② survivor が全責任になっていない: live=%d target=%d B=%d orphan=%d", liveB2, tgtB2, hB2, orphan2)
	}
	if !(abs(hA3-hB3) <= 1 && hA3+hB3 == nTenant && orphan3 == 0) {
		t.Errorf("③ 再収束していない: A=%d B=%d orphan=%d", hA3, hB3, orphan3)
	}
	if !monotone {
		t.Errorf("fence が単調でない（古い担当の上書きを弾けない）")
	}
	if maxFence < 2 {
		t.Errorf("fence が増えていない（reclaim で +1 されるはず）: max=%d", maxFence)
	}
	if orphanStatic != nTenant/2 {
		t.Errorf("④ 静的ピンの orphan が想定外: %d（期待 %d）", orphanStatic, nTenant/2)
	}

	rec.Scope(
		"MySQL 8.0 / lease は DB 時計(NOW(3))・ttl="+itoa(ttlSec)+"s・stale="+itoa(staleS)+"s / テナント20・worker2",
		"1ティック=心拍→renew→target=CEIL(全/生存)→過少claim/過多shed。claim は SKIP LOCKED＋CAS(fence+1)",
		"held=owner=me かつ期限内。orphan=有効な担当が居ないテナント数（0が健全）",
	)
	rec.Uncertain(
		"実時間依存（sleep で lease 失効を待つ）。ttl/stale/tick は説明用の短い値。実運用は clock skew に応じ調整（EXP-2）",
		"収束は逐次ティックで模擬（テスト内で A/B を順に回す）。実際は各コンテナが独立ループ。SKIP LOCKED で取り合いは裁ける",
		"failover の空白は約 ttl（可用性の谷）。correctness は fence が担保、ttl は速さだけに効く（EXP-2）",
		"survivor が全担当を背負える器か（接続/メモリ）は容量側の前提（EXP-60/tenant-worker-capacity）",
	)
	rec.Artifact(
		"internal/assignlab: DB lease による fair-share 割り当て（claim/shed/renew/heartbeat/fence）",
		"docs/web-worker-deploy.md §8: 多重化テナントワーカーのデプロイ（lease＝排他＋引継ぎ）",
	)
	rec.Next("（3台以上・shard 化・実コンテナでの独立ループは容量計画側／LIVE_ENV は未実測）")

	files, err := rec.Save(
		"テナント割り当てを DB の lease（1テナント1行）で持ち、各 worker が target=CEIL(全/生存) まで claim・" +
			"超えたら shed するだけで、① 平常は 10/10 に均等収束、② A 死亡で live↓→target↑ により survivor B が " +
			"全20を自動担当（失敗時全責任は if 文でなく式の結果＝特別コード不要）、③ A' 復帰で 10/10 に再収束した。" +
			"全体で二重所有=0（tenant_id が PK＝行がロック）・fence は claim ごと+1 で単調（古い担当の上書きを弾く）。" +
			"④ 対照として静的ピン（owner 固定）は A 死亡で A担当の10が宙に浮き orphan="+itoa(nTenant/2)+"（2台にした意味が消える）。" +
			"＝均等も失敗時全責任も『owner を固定せず動的 claim＋fair-share』で DB が自動的に満たす。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func checkMonotone(ms ...map[string]int64) (bool, int64) {
	var maxF int64
	prev := ms[0]
	for _, m := range ms {
		for id, f := range m {
			if pf, ok := prev[id]; ok && f < pf {
				return false, maxF
			}
			if f > maxF {
				maxF = f
			}
		}
		prev = m
	}
	return true, maxF
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
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
