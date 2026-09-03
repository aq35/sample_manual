// コマンド loadsim は §4.1「効く順」の試算を、実際の MySQL で確かめる。
//
//	MYSQL_DSN=... go run ./cmd/loadsim -rate 1000 -duration 10s -robots 1000 -change-rate 0.01
//
// 3つのやり方で同じイベント列を流し、DB に発生したトランザクション数を数える。
//
//	naive : 受けたぶんだけ 1件ずつ UPDATE（autocommit）
//	skip  : メモリで比較し、変化したときだけ書く（§4.2）
//	batch : さらに一定時間まとめて、重複排除して1トランザクションで書く（§4.3）
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

type result struct {
	mode     string
	metrics  worker.Metrics
	txCount  int
	rowCount int
	elapsed  time.Duration
	maxLag   time.Duration
}

func main() {
	var (
		dsn        = flag.String("dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN（既定は環境変数 MYSQL_DSN）")
		tenant     = flag.String("tenant", "t-loadsim", "テナント ID")
		robots     = flag.Int("robots", 1000, "対象数")
		rate       = flag.Int("rate", 1000, "受信レート（件/秒）")
		duration   = flag.Duration("duration", 10*time.Second, "流す時間")
		changeRate = flag.Float64("change-rate", 0.01, "受信のうち実際に状態が変わる割合（§4.1 の前提）")
		batchEvery = flag.Duration("batch", 200*time.Millisecond, "バッチ間隔（batch モード）")
		modes      = flag.String("modes", "naive,skip,batch", "実行するモード")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "MYSQL_DSN が未設定。scripts/mysql-up.sh を参照")
		os.Exit(1)
	}

	cfg := store.DefaultPool()
	s, err := store.Open(*dsn, cfg)
	if err != nil {
		fail(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		fail(err)
	}

	fmt.Printf("条件: 対象 %d 件 / %d 件毎秒 / %v / 変化率 %.1f%% / バッチ間隔 %v\n\n",
		*robots, *rate, *duration, *changeRate*100, *batchEvery)

	var results []result
	for _, m := range splitCSV(*modes) {
		//smlint:allow rowsaffected 理由: §4.1 の比較対象（素直に1件ずつ書く方式）
		if _, err := s.DB().ExecContext(ctx, "DELETE FROM robot_state WHERE tenant_id = ?", *tenant); err != nil {
			fail(err)
		}
		r, err := run(ctx, s, model.TenantID(*tenant), m, *robots, *rate, *duration, *changeRate, *batchEvery)
		if err != nil {
			fail(err)
		}
		results = append(results, r)
		report(r, *duration)
	}

	if len(results) > 1 {
		fmt.Println("まとめ（§4.1 の表に対応）")
		fmt.Printf("  %-8s %14s %14s %10s\n", "モード", "トランザクション/秒", "書いた行/秒", "追いついたか")
		for _, r := range results {
			ok := "はい"
			if r.maxLag > time.Second {
				ok = fmt.Sprintf("いいえ(遅延%v)", r.maxLag.Round(time.Millisecond))
			}
			fmt.Printf("  %-8s %14.1f %14.1f %10s\n",
				r.mode,
				float64(r.txCount)/duration.Seconds(),
				float64(r.rowCount)/duration.Seconds(),
				ok)
		}
	}
}

func run(ctx context.Context, s *store.Store, tenant model.TenantID, mode string,
	robots, rate int, duration time.Duration, changeRate float64, batchEvery time.Duration) (result, error) {

	tr := worker.New(worker.Options{
		TouchBase:   30 * time.Second,
		TouchJitter: 30 * time.Second,
		StaleAfter:  60 * time.Second,
	})

	res := result{mode: mode}
	var mu sync.Mutex

	// batch モード用の書き出しループ
	var wg sync.WaitGroup
	stop := make(chan struct{})
	flush := func() error {
		rows := tr.Drain()
		if len(rows) == 0 {
			return nil
		}
		if _, err := s.UpsertBatch(ctx, tenant, rows); err != nil {
			tr.Requeue(rows) // 失敗したら committed を進めない（§4.2）
			return err
		}
		tr.Commit(rows, time.Now())
		mu.Lock()
		res.txCount++
		res.rowCount += len(rows)
		mu.Unlock()
		return nil
	}
	if mode == "batch" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(batchEvery)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := flush(); err != nil {
						fmt.Fprintln(os.Stderr, "flush:", err)
					}
				case <-stop:
					_ = flush()
					return
				}
			}
		}()
	}

	rnd := rand.New(rand.NewSource(20260902))
	ids := make([]model.ID, robots)
	states := make([]model.State, robots)
	for i := range ids {
		ids[i] = model.ID(fmt.Sprintf("r%05d", i))
		states[i] = model.State{Status: model.StatusRunning, Online: true, Battery: 80}
	}

	// ★計測の前に、起動時の全件取得（§2.3）に相当する初期化を済ませておく。
	// これをやらないと「初回観測ぶんの変化」が測定値に混ざり、変化率が実態より高く出る。
	seed := make([]worker.Row, 0, robots)
	now := time.Now().UTC()
	for i := range ids {
		tr.Observe(ids[i], model.Observation{State: states[i], ObservedAt: now, Source: model.SourceAPI})
	}
	seed = tr.Drain()
	if _, err := s.UpsertBatch(ctx, tenant, seed); err != nil {
		return res, err
	}
	tr.Commit(seed, time.Now())
	tr.ResetMetrics()

	total := int(float64(rate) * duration.Seconds())
	start := time.Now()
	for i := 0; i < total; i++ {
		// 指定レートに合わせて待つ（詰まったら遅延として記録する）
		want := time.Duration(float64(i) / float64(rate) * float64(time.Second))
		if lag := time.Since(start) - want; lag > res.maxLag {
			res.maxLag = lag
		} else if lag < 0 {
			time.Sleep(-lag)
		}

		k := rnd.Intn(robots)
		st := states[k]
		if rnd.Float64() < changeRate { // ここだけが「実際の変化」
			st.Battery = int8(1 + rnd.Intn(99))
			if rnd.Intn(5) == 0 {
				st.Status = model.Status(1 + rnd.Intn(3))
			}
			states[k] = st
		}
		obs := model.Observation{State: st, ObservedAt: time.Now().UTC(), Source: model.SourceWS}

		switch mode {
		case "naive":
			// 受けたぶんだけ、そのまま1件 UPDATE する
			row := worker.Row{ID: ids[k], State: st, ObservedAt: obs.ObservedAt, Source: obs.Source}
			if _, err := s.UpsertBatch(ctx, tenant, []worker.Row{row}); err != nil {
				return res, err
			}
			mu.Lock()
			res.txCount++
			res.rowCount++
			mu.Unlock()

		case "skip":
			// メモリで比較して、変化したときだけ 1件書く
			if d := tr.Observe(ids[k], obs); d == worker.DecisionChange || d == worker.DecisionTouch {
				rows := tr.Drain()
				if _, err := s.UpsertBatch(ctx, tenant, rows); err != nil {
					tr.Requeue(rows)
					return res, err
				}
				tr.Commit(rows, time.Now())
				mu.Lock()
				res.txCount++
				res.rowCount += len(rows)
				mu.Unlock()
			}

		case "batch":
			tr.Observe(ids[k], obs)

		default:
			return res, fmt.Errorf("知らないモード: %s", mode)
		}
	}
	close(stop)
	wg.Wait()
	res.elapsed = time.Since(start)
	if mode != "naive" {
		res.metrics = tr.Metrics()
	} else {
		res.metrics = worker.Metrics{Received: uint64(total), Changed: uint64(total)}
	}
	return res, nil
}

func report(r result, want time.Duration) {
	fmt.Printf("[%s]\n", r.mode)
	fmt.Printf("  受信 %d / 変化 %d / 近況報告 %d / 何もしない %d（変化率 %.2f%%）\n",
		r.metrics.Received, r.metrics.Changed, r.metrics.Touched, r.metrics.Skipped, r.metrics.ChangeRate()*100)
	fmt.Printf("  トランザクション %d 回（%.1f /秒）/ 書いた行 %d（%.1f /秒）\n",
		r.txCount, float64(r.txCount)/want.Seconds(), r.rowCount, float64(r.rowCount)/want.Seconds())
	fmt.Printf("  実時間 %v（目標 %v）/ 最大遅延 %v\n\n",
		r.elapsed.Round(time.Millisecond), want, r.maxLag.Round(time.Millisecond))
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	sort.SliceStable(out, func(i, j int) bool { return order(out[i]) < order(out[j]) })
	return out
}

func order(mode string) int {
	switch mode {
	case "naive":
		return 0
	case "skip":
		return 1
	case "batch":
		return 2
	}
	return 3
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "エラー:", err)
	os.Exit(1)
}
