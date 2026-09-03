package repo_test

// マイグレーションが、運用で起きることに耐えるかの確認。

import (
	"context"
	"sync"
	"testing"

	"github.com/aq35/sample_manual/internal/repo"
)

// 複数コンテナが同時に起動しても、当たるのは1回だけ。
func TestMigrate_同時に起動しても壊れない(t *testing.T) {
	db := newDB(t) // ここで1回目の Migrate が終わっている
	ctx := context.Background()

	var before int
	if err := db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("マイグレーションが記録されていない")
	}

	// 8コンテナが同時に起動した状況
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = repo.Migrate(ctx, db)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%d 番目が失敗: %v", i, err)
		}
	}

	var after int
	if err := db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("二重に適用された: %d → %d", before, after)
	}
	t.Logf("8並列で Migrate を呼んでも、適用済みは %d 件のまま（GET_LOCK で直列化）", after)
}

// 適用済みのファイルを書き換えたら、起動時に気づける。
func TestMigrate_適用済みの書き換えを検出する(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// 適用済みのチェックサムを、ファイルを書き換えたことにして壊す
	if _, err := db.SQL().ExecContext(ctx,
		"UPDATE schema_migrations SET checksum = ? WHERE version = 1",
		"0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	err := repo.Migrate(ctx, db)
	if err == nil {
		t.Fatal("書き換えを検出できていない")
	}
	t.Logf("検出した: %v", err)

	// 元に戻す（他のテストに影響させない）
	ms, err2 := repo.LoadMigrations()
	if err2 != nil {
		t.Fatal(err2)
	}
	if _, err := db.SQL().ExecContext(ctx,
		"UPDATE schema_migrations SET checksum = ? WHERE version = 1", ms[0].Checksum); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// マイグレーションの並びが壊れていないこと（番号の重複・命名）。
func TestMigrate_ファイルの並びが正しい(t *testing.T) {
	ms, err := repo.LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("マイグレーションが1つも無い")
	}
	for i, m := range ms {
		if i > 0 && m.Version <= ms[i-1].Version {
			t.Fatalf("番号が昇順でない: %d の次が %d", ms[i-1].Version, m.Version)
		}
		t.Logf("%04d_%s (checksum %s...)", m.Version, m.Name, m.Checksum[:8])
	}
}
