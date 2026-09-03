package repo_test

// WithLock（GET_LOCK の正しい使い方を閉じ込めたもの）の確認。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/repo"
)

func TestWithLock_同時に走らない(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	var running, maxRunning atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithLock(ctx, "test_exclusive", 10*time.Second, func(ctx context.Context) error {
				now := running.Add(1)
				for {
					m := maxRunning.Load()
					if now <= m || maxRunning.CompareAndSwap(m, now) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				running.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("ロックが取れない: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxRunning.Load() != 1 {
		t.Fatalf("同時に %d 個走った（排他できていない）", maxRunning.Load())
	}
	t.Log("8並列で呼んでも、同時に走ったのは 1 つだけ")
}

// ★実験1で見つけた壊れ方（プールごしだと解放できない）の回帰テスト。
func TestWithLock_終わったら必ず解放される(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// 何度も取り直しても、ロックが残らないこと
	for i := 0; i < 20; i++ {
		if err := db.WithLock(ctx, "test_release", time.Second, func(context.Context) error {
			return nil
		}); err != nil {
			t.Fatalf("%d 回目: %v", i, err)
		}
		if id, used, err := db.LockHolder(ctx, "test_release"); err != nil {
			t.Fatal(err)
		} else if used {
			t.Fatalf("%d 回目のあと、ロックが接続 %d に残っている", i, id)
		}
	}
	t.Log("20回繰り返しても、終わるたびに解放されている（接続を固定しているから）")

	// 中でエラーになっても解放される
	wantErr := errors.New("失敗した")
	if err := db.WithLock(ctx, "test_release", time.Second, func(context.Context) error {
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("エラーが伝わっていない: %v", err)
	}
	if _, used, err := db.LockHolder(ctx, "test_release"); err != nil || used {
		t.Fatalf("エラー時に解放されていない: used=%v err=%v", used, err)
	}
	t.Log("中でエラーになっても解放される")
}

func TestWithLock_取れなければ待って諦める(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	held := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = db.WithLock(ctx, "test_busy", 5*time.Second, func(context.Context) error {
			close(held)
			<-done
			return nil
		})
	}()
	<-held

	start := time.Now()
	err := db.WithLock(ctx, "test_busy", time.Second, func(context.Context) error {
		t.Error("取れてはいけない")
		return nil
	})
	elapsed := time.Since(start)
	close(done)

	if !errors.Is(err, repo.ErrLockNotAcquired) {
		t.Fatalf("ErrLockNotAcquired でない: %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("待っていない: %v", elapsed)
	}
	t.Logf("先客がいる間は %v 待って、%v を返した", elapsed.Round(time.Millisecond), repo.ErrLockNotAcquired)
}

func TestWithLock_名前にスキーマ名が付く(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	done := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = db.WithLock(ctx, "test_name", 5*time.Second, func(context.Context) error {
			close(held)
			<-done
			return nil
		})
	}()
	<-held

	// 生の名前（スキーマ名なし）では、まだ空いている
	var free int
	if err := db.SQL().QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", "test_name").Scan(&free); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.SQL().QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	var used int
	if err := db.SQL().QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", schema+".test_name").Scan(&used); err != nil {
		t.Fatal(err)
	}
	close(done)

	if free != 1 || used != 0 {
		t.Fatalf("スキーマ名が付いていない: 生の名前 free=%d / %s.test_name free=%d", free, schema, used)
	}
	t.Logf("ロック名は %q になる（サーバ全体で名前空間が共有されるため）", schema+".test_name")
}

func TestWithLock_長すぎる名前は拒否する(t *testing.T) {
	db := newDB(t)
	long := ""
	for i := 0; i < 70; i++ {
		long += "a"
	}
	err := db.WithLock(context.Background(), long, time.Second, func(context.Context) error {
		t.Error("実行されてはいけない")
		return nil
	})
	if err == nil {
		t.Fatal("長すぎる名前が通った")
	}
	t.Logf("64文字を超える名前は実行前に弾く: %v", err)
}

// 使い終わった接続がプールに戻り、他の処理に影響しないこと。
func TestWithLock_接続を占有し続けない(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := db.WithLock(ctx, fmt.Sprintf("test_conn_%d", i), time.Second, func(ctx context.Context) error {
			var v int
			return db.SQL().QueryRowContext(ctx, "SELECT 1").Scan(&v)
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats := db.SQL().Stats()
	t.Logf("5回ロックを取ったあとの接続: 使用中 %d / アイドル %d", stats.InUse, stats.Idle)
	if stats.InUse != 0 {
		t.Fatalf("接続が %d 本、使われたままになっている", stats.InUse)
	}
	t.Log("ロックのために固定した接続は、終わるとプールに戻る")
}
