// Package repotest は、リポジトリ層の使い方をテストで縛るための道具。
//
// 他プロジェクトでもそのまま使えるように、テスト側だけが依存する形にしてある。
//
//	repotest.RequireIndexed(t, sc, "profile.List", query, args...)  // 実行計画を固定する
//	repotest.RequireQueries(t, db, 1, func() { ... })               // N+1 を検出する
package repotest

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/repo"
)

// RequireIndexed は、その問い合わせが索引を使っていることを確かめる。
//
// 「遅くなったら直す」ではなく「遅くなる書き方をコミットさせない」ための検査。
//
// ★注意: 実行計画は行数で変わる。実験では、同じ問い合わせが
// 50行のときは uq_tenant_serial + filesort、5,000行のときは PRIMARY の range になった。
// **本番に近い件数を入れてから検査すること。** 数十件のテストデータで固定した計画は、
// 本番の計画とは別物になる。
func RequireIndexed(t *testing.T, s *repo.Scope, op, query string, args ...any) []repo.PlanRow {
	t.Helper()
	plan, err := s.Explain(context.Background(), op, query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN に失敗: %v", err)
	}
	if len(plan) == 0 {
		t.Fatalf("実行計画が空: %s", query)
	}
	for _, p := range plan {
		t.Logf("計画: %s", p)
		if p.FullScan() {
			t.Errorf("全表走査になっている（索引が効いていない）: %s\nSQL: %s", p, query)
		}
		if p.UsesFilesort() {
			t.Errorf("Using filesort（並べ替えが索引で済んでいない）: %s\nSQL: %s", p, query)
		}
		if p.UsesTemporary() {
			t.Errorf("Using temporary（一時表を作っている）: %s\nSQL: %s", p, query)
		}
	}
	return plan
}

// RequireRowsScanned は、走査見込み行数が上限以下であることを確かめる。
// 「索引は使っているが、テナント全体を舐めている」を見つけるのに使う。
func RequireRowsScanned(t *testing.T, s *repo.Scope, limit int64, op, query string, args ...any) {
	t.Helper()
	plan, err := s.Explain(context.Background(), op, query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN に失敗: %v", err)
	}
	for _, p := range plan {
		if p.Rows > limit {
			t.Errorf("走査見込みが %d 行（上限 %d 行）: %s\nSQL: %s", p.Rows, limit, p, query)
		}
	}
}

// RequireQueries は、fn の中で発行された問い合わせ回数がちょうど want 件であることを確かめる。
//
// N+1 は「遅いクエリ」ではなく「速いクエリを N 回」なので、
// スロークエリログには出ない。回数を数えるのが確実（実験: 500件で 64倍の差）。
func RequireQueries(t *testing.T, db *repo.DB, want int64, fn func()) {
	t.Helper()
	before := db.Stats()
	fn()
	after := db.Stats()
	got := (after.Queries - before.Queries) + (after.Execs - before.Execs)
	if got != want {
		t.Errorf("問い合わせ回数が %d 回（想定 %d 回）。N+1 になっていないか", got, want)
	}
}

// RequireNoLongTx は、fn の中で長いトランザクションが無かったことを確かめる。
func RequireNoLongTx(t *testing.T, db *repo.DB, fn func()) {
	t.Helper()
	before := db.Stats()
	fn()
	after := db.Stats()
	if n := after.LongTxs - before.LongTxs; n > 0 {
		t.Errorf("長いトランザクションが %d 回あった（外部 API を呼んでいないか）", n)
	}
}
