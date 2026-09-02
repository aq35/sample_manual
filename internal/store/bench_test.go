package store_test

// §4.3「まとめて書く」の効き目（fsync の削減）を、実際の MySQL で測る。
//
//	MYSQL_DSN=... go test ./internal/store/ -bench Write -benchtime 20x -run XXX
//
// 100行を書くのに、コミットが何回入るかだけが違う3つのやり方を比べる。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/mysqltest"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

const benchRows = 100

func benchSetup(b *testing.B) (*store.Store, []worker.Row) {
	s := mysqltest.Store(b, store.DefaultPool())
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, "DELETE FROM robot_state WHERE tenant_id = ?", "t-bench"); err != nil {
		b.Fatal(err)
	}
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	rows := make([]worker.Row, benchRows)
	for i := range rows {
		rows[i] = worker.Row{
			ID:         model.ID(fmt.Sprintf("r%05d", i)),
			State:      model.State{Status: model.StatusRunning, Online: true, Battery: 80},
			ObservedAt: at,
			Source:     model.SourceWS,
		}
	}
	if _, err := s.UpsertBatch(ctx, "t-bench", rows); err != nil {
		b.Fatal(err)
	}
	return s, rows
}

// 毎回 observed_at と battery を進めて、必ず実書き込みが起きるようにする。
func bump(rows []worker.Row, i int) []worker.Row {
	out := make([]worker.Row, len(rows))
	copy(out, rows)
	for j := range out {
		out[j].ObservedAt = out[j].ObservedAt.Add(time.Duration(i+1) * time.Second)
		out[j].State.Battery = int8(1 + (i+j)%99)
	}
	return out
}

// ① 素直に1件ずつ UPDATE（autocommit）: コミット100回
func BenchmarkWrite_EachAutocommit(b *testing.B) {
	s, rows := benchSetup(b)
	ctx := context.Background()
	i := 0
	b.ResetTimer()
	for b.Loop() {
		if err := s.UpdateEachAutocommit(ctx, "t-bench", bump(rows, i)); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchRows)/1e6, "ms/row")
}

// ② 1件ずつだが1トランザクション: コミット1回（往復は100回のまま）
func BenchmarkWrite_EachInOneTx(b *testing.B) {
	s, rows := benchSetup(b)
	ctx := context.Background()
	i := 0
	b.ResetTimer()
	for b.Loop() {
		if err := s.UpdateEachInTx(ctx, "t-bench", bump(rows, i)); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchRows)/1e6, "ms/row")
}

// ③ 1文にまとめて1トランザクション: 往復1回・コミット1回（推奨、§4.3）
func BenchmarkWrite_BatchUpsert(b *testing.B) {
	s, rows := benchSetup(b)
	ctx := context.Background()
	i := 0
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.UpsertBatch(ctx, "t-bench", bump(rows, i)); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchRows)/1e6, "ms/row")
}
