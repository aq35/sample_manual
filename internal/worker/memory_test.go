package worker_test

// §3.3「1対象あたり 約200バイト」を実測する。
// 資料では struct のサイズからの見積もりだったので、ここで実際に測って裏を取る。
//
//	go test ./internal/worker/ -run Memory -v

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
)

func TestMemory_1対象あたりのバイト数(t *testing.T) {
	for _, n := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprintf("%d件", n), func(t *testing.T) {
			c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
			tr := newTracker(c)

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			for i := 0; i < n; i++ {
				id := model.ID(fmt.Sprintf("r%08d", i))
				tr.Observe(id, obs(model.StatusRunning, true, 80, c.now))
			}
			tr.Commit(tr.Drain(), c.now) // pending を空にしてから測る

			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(tr)

			used := after.HeapAlloc - before.HeapAlloc
			perEntry := float64(used) / float64(n)
			t.Logf("%d 件: 合計 %.1f KB / 1件あたり %.0f バイト", n, float64(used)/1024, perEntry)

			if tr.Len() != n {
				t.Fatalf("件数が合わない: %d", tr.Len())
			}
			// 資料の見積もり 200 バイトに対して、桁が変わっていないことだけ確認する。
			if perEntry > 500 {
				t.Fatalf("1件あたり %.0f バイト。見積もり(200B)から桁が外れている", perEntry)
			}
		})
	}
}
