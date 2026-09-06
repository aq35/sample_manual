package repo_test

// テナント越えのプロパティテスト。
//
//	MYSQL_DSN=... go test ./internal/repo/ -run TestTenantIsolation -v
//
// 主張: テナント A のハンドル（*repo.Scope）から、どの操作を・どんな順で・どんな引数で
// 呼んでも、テナント B の行には一度も触れない（読めない・書けない）。
//
// これは「型で禁じる（Scope しか受け取らない）＋クエリで強制（:tenant 必須）」が
// 実際に境界として働いているかを、ランダムな操作列で叩いて確かめる。
// ★文字列ガード（EXP-8）は security boundary ではない。ここで見るのは
// 「正しく使ったときにテナントが混ざらないこと」= 分離の健全性。

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/repo"
)

func TestTenantIsolation_操作をランダムに叩いても混ざらない(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// 2テナントに**同じ形・同じ robot_id**のデータを入れる。
	// robot_id が衝突しているからこそ、テナント指定を1つでも取りこぼすと B が漏れる。
	const n = 40
	seedProfiles(t, db, tenantA, n)
	seedProfiles(t, db, tenantB, n)

	// B 側に「A には存在しない値」を仕込んでおく（漏れたら serial で気づける）。
	scB := db.Tenant(tenantB)
	for i := 0; i < n; i++ {
		_ = profiles.Rename(ctx, scB, fmt.Sprintf("r%04d", i),
			fmt.Sprintf("B専用-%d", i), 0)
	}

	scA := db.Tenant(tenantA)
	rng := rand.New(rand.NewSource(1))

	// A のハンドルで、ランダムな操作を大量に叩く。
	// どの戻り値にも B の痕跡（serial が tenantB- で始まる／名前が "B専用"）が出てはいけない。
	const iterations = 400
	leaks := 0
	assertNotB := func(where, name, serial string) {
		if hasPrefix(serial, string(tenantB)+"-") || hasPrefix(name, "B専用") {
			leaks++
			t.Errorf("テナント越え! [%s] name=%q serial=%q が A のハンドルから見えた", where, name, serial)
		}
	}

	for i := 0; i < iterations; i++ {
		id := fmt.Sprintf("r%04d", rng.Intn(n+5)) // たまに存在しない ID も混ぜる
		switch rng.Intn(6) {
		case 0: // Get
			if p, err := profiles.Get(ctx, scA, id); err == nil {
				assertNotB("Get", p.Name, p.Serial)
			}
		case 1: // List（キーセットで全ページ舐める）
			ks := repo.Keyset{Limit: 7}
			for {
				page, err := profiles.List(ctx, scA, ks)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				for _, p := range page.Items {
					assertNotB("List", p.Name, p.Serial)
				}
				if page.Next == "" {
					break
				}
				ks.After = page.Next
			}
		case 2: // GetMany
			ids := []string{id, fmt.Sprintf("r%04d", rng.Intn(n))}
			m, err := profiles.GetMany(ctx, scA, ids)
			if err != nil {
				t.Fatalf("GetMany: %v", err)
			}
			for _, p := range m {
				assertNotB("GetMany", p.Name, p.Serial)
			}
		case 3: // Count（A のぶんだけのはず）
			c, err := profiles.Count(ctx, scA)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if c != n {
				t.Errorf("Count=%d want %d（A のぶんだけのはず。B が混ざると増える）", c, n)
			}
		case 4: // Rename（A の行だけが変わるはず）
			_ = profiles.Rename(ctx, scA, id, fmt.Sprintf("A更新-%d", i), 0)
		case 5: // Delete して、B の同じ robot_id が消えていないことを確認
			if err := profiles.Delete(ctx, scA, id); err == nil {
				// B 側の同じ id はまだ居るはず
				if _, err := profiles.Get(ctx, db.Tenant(tenantB), id); err != nil {
					t.Errorf("A の Delete が B の %s まで消した（テナント越え削除）", id)
				}
				// 消したぶん A に入れ直して件数を保つ
				_ = profiles.Create(ctx, scA, repo.Profile{
					RobotID: id, Name: "A再作成", ModelName: "AGV-3000",
					Serial: fmt.Sprintf("%s-SN%s", tenantA, id),
				})
			}
		}
	}

	// 最後に、B のデータが丸ごと無傷であることを確かめる（A の操作が B を壊していない）。
	cB, err := profiles.Count(ctx, db.Tenant(tenantB))
	if err != nil {
		t.Fatal(err)
	}
	if cB != n {
		t.Errorf("B の件数が %d（%d のはず）。A の操作が B に波及した", cB, n)
	}
	t.Logf("A から %d 操作を叩き、テナント越え %d 件（0 が正）。B は %d 件で無傷", iterations, leaks, cB)
}

// TestTenantIsolation_Unscopedは明示的にしか越えられない は、
// テナント越えが「うっかり」ではなく「明示的な Unscoped(reason)」でしか起きないことを示す。
func TestTenantIsolation_Unscopedは明示的にしか越えられない(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedProfiles(t, db, tenantA, 5)
	seedProfiles(t, db, tenantB, 5)

	// :tenant を書き忘れた SQL は、そもそも実行前に弾かれる（型ではなくガードの層）。
	scA := db.Tenant(tenantA)
	_, err := scA.Query(ctx, "bad", "SELECT robot_id FROM robot_profile") // :tenant 無し
	if err == nil {
		t.Error(":tenant の無い SELECT が通ってしまった（分離の穴）")
	} else {
		t.Logf(":tenant 無しは拒否された: %v", err)
	}

	// 全テナント横断は Unscoped(reason) を明示したときだけ。理由が監査に残る。
	across := db.Unscoped("運用: 全テナントの棚卸し")
	rows, err := across.AllowUnbounded().Query(ctx, "audit",
		"SELECT DISTINCT tenant_id FROM robot_profile")
	if err != nil {
		t.Fatalf("Unscoped 横断に失敗: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var tn string
		if err := rows.Scan(&tn); err != nil {
			t.Fatal(err)
		}
		seen[tn] = true
	}
	if !seen[string(tenantA)] || !seen[string(tenantB)] {
		t.Errorf("Unscoped でも両テナントが見えない: %v", seen)
	}
	t.Logf("Unscoped（理由つき）でだけ横断できる: %v", keysOf(seen))
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = model.TenantID("")
