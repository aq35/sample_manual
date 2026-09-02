package store_test

// 実際の MySQL に対して、§2.7 / §4.3 / §5.2 の主張を確かめる。
// MYSQL_DSN が無ければ skip される。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

const tenant = model.TenantID("t-test")

func row(id model.ID, st model.Status, online bool, batt int8, at time.Time, src model.Source) worker.Row {
	return worker.Row{ID: id, State: model.State{Status: st, Online: online, Battery: batt}, ObservedAt: at, Source: src}
}

// §2.7「遅れて届いた古い情報で、新しい状態を上書きしない」を DB 側で確認する。
// A・B・C の到着順が入れ替わっても壊れないこと。
func TestUpsert_古い観測で上書きされない(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	ctx := context.Background()
	mysqltest.Truncate(t, s.DB(), string(tenant))

	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	newer := t0.Add(10 * time.Second)
	older := t0

	// C（WebSocket）の新しい観測が先に着いた
	if _, err := s.UpsertBatch(ctx, tenant, []worker.Row{
		row("r001", model.StatusRunning, true, 80, newer, model.SourceWS),
	}); err != nil {
		t.Fatal(err)
	}
	// A（全件同期）の古い観測が後から着いた
	if _, err := s.UpsertBatch(ctx, tenant, []worker.Row{
		row("r001", model.StatusStopped, false, 10, older, model.SourceAPI),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(ctx, tenant, "r001")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Status != model.StatusRunning || !got.State.Online || got.State.Battery != 80 {
		t.Fatalf("古い情報で上書きされた: %+v", got.State)
	}
	if !got.ObservedAt.Equal(newer) {
		t.Fatalf("observed_at が巻き戻った: %v", got.ObservedAt)
	}
	if got.Source != model.SourceWS {
		t.Fatalf("source が巻き戻っていない側に付いている: %v", got.Source)
	}
}

// 同じ内容を2回書いても結果が変わらない（冪等）。
func TestUpsert_同じ内容を2回書いても同じ(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	ctx := context.Background()
	mysqltest.Truncate(t, s.DB(), string(tenant))

	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r := []worker.Row{row("r001", model.StatusRunning, true, 80, at, model.SourceAPI)}

	n1, err := s.UpsertBatch(ctx, tenant, r)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := s.UpsertBatch(ctx, tenant, r)
	if err != nil {
		t.Fatal(err)
	}
	// MySQL は「結果的に同じ値になる UPDATE」で affected_rows を 0 として返す。
	// （INSERT 成功=1 / 値が変わる UPDATE=2 / 変化なし=0）
	t.Logf("affected_rows: 1回目=%d（新規=1）, 2回目=%d（変化なし=0）", n1, n2)
	if n1 != 1 {
		t.Fatalf("新規挿入の affected_rows が 1 でない: %d", n1)
	}
	if n2 != 0 {
		t.Fatalf("同じ値の再書き込みで affected_rows が 0 でない: %d", n2)
	}
}

// §4.3「バッチ内は必ず主キー順にソートしてから投げる」
//
// すれ違う順序で行を触るとデッドロックになることを、実際に起こして確認する。
// そのうえで、ソートしてあれば起きないことを確認する。
func TestBatch_ソートしないとデッドロックする(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	ctx := context.Background()
	mysqltest.Truncate(t, s.DB(), string(tenant))

	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	seed := []worker.Row{
		row("r002", model.StatusRunning, true, 80, at, model.SourceAPI),
		row("r005", model.StatusRunning, true, 80, at, model.SourceAPI),
	}
	if _, err := s.UpsertBatch(ctx, tenant, seed); err != nil {
		t.Fatal(err)
	}

	// 2つのトランザクションが、それぞれ逆順に行を触る
	deadlocked := runInterleaved(t, s.DB(), []string{"r002", "r005"}, []string{"r005", "r002"})
	if !deadlocked {
		t.Fatal("すれ違い順の更新でデッドロックが起きなかった（想定外）")
	}
	t.Log("すれ違う順序 → デッドロック検出（MySQL が片方を巻き戻す）")

	// 同じ順序（＝主キー順にソート済み）なら、待たされるだけで壊れない
	deadlocked = runInterleaved(t, s.DB(), []string{"r002", "r005"}, []string{"r002", "r005"})
	if deadlocked {
		t.Fatal("主キー順に揃えたのにデッドロックした")
	}
	t.Log("主キー順に揃える → デッドロックしない")
}

// runInterleaved は2つのトランザクションを時間差で走らせ、デッドロックが
// 起きたかを返す。行を触る順序だけを変えて比べるための道具。
func runInterleaved(t *testing.T, db *sql.DB, orderA, orderB []string) bool {
	t.Helper()
	ctx := context.Background()

	const q = `UPDATE robot_state SET battery = battery WHERE tenant_id=? AND robot_id=?`

	run := func(order []string, delay time.Duration, out chan<- error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			out <- err
			return
		}
		defer func() { _ = tx.Rollback() }()

		// 詰まったときに 50 秒待たされないよう、このセッションだけ短くする
		if _, err := tx.ExecContext(ctx, "SET innodb_lock_wait_timeout = 5"); err != nil {
			out <- err
			return
		}
		time.Sleep(delay)
		for i, id := range order {
			if i > 0 {
				time.Sleep(150 * time.Millisecond) // 相手に1行目を掴ませる間
			}
			if _, err := tx.ExecContext(ctx, q, string(tenant), id); err != nil {
				out <- err
				return
			}
		}
		out <- tx.Commit()
	}

	errs := make(chan error, 2)
	go run(orderA, 0, errs)
	go run(orderB, 75*time.Millisecond, errs)

	deadlocked := false
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
		case store.IsDeadlock(err):
			deadlocked = true
		case store.IsLockWaitTimeout(err):
			t.Fatalf("ロック待ちタイムアウト（デッドロックではないが詰まっている）: %v", err)
		default:
			t.Fatal(err)
		}
	}
	return deadlocked
}

// §4.2「SQL 側で弾く方法もあるが二番手」
// 書き込みは避けられても、往復は発生する（affected_rows=0 でも1往復ぶんの時間がかかる）。
func TestWhereGuard_書き込みは避けられるが往復は残る(t *testing.T) {
	s := mysqltest.Store(t, store.DefaultPool())
	ctx := context.Background()
	mysqltest.Truncate(t, s.DB(), string(tenant))

	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r := row("r001", model.StatusRunning, true, 80, at, model.SourceAPI)
	if _, err := s.UpsertBatch(ctx, tenant, []worker.Row{r}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	n, err := s.UpdateWithWhereGuard(ctx, tenant, r)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("同じ値なので 0 行のはず: %d", n)
	}
	t.Logf("値は変わらなかったが、往復に %v かかった（メモリで弾けばこれが 0 になる）", elapsed)
}

// §5.2 の設定チェックを、コードとして持っておく。
func TestPoolConfig_既定値のままだと警告する(t *testing.T) {
	bad := store.PoolConfig{MaxOpenConns: 20} // MaxIdleConns 未設定＝既定値 2
	err := bad.Validate(28800 * time.Second)
	if err == nil {
		t.Fatal("MaxIdleConns 未設定を検出できていない")
	}
	t.Logf("検出した問題:\n%v", err)

	good := store.DefaultPool()
	if err := good.Validate(28800 * time.Second); err != nil {
		t.Fatalf("推奨設定が弾かれた: %v", err)
	}
	// wait_timeout が短い環境では ConnMaxIdleTime も短くする必要がある
	if err := good.Validate(60 * time.Second); err == nil {
		t.Fatal("wait_timeout(60s) < ConnMaxIdleTime(5m) を検出できていない")
	}
}
