package repo

import "testing"

// 検査と :tenant 束縛だけのコスト（DB へ行かない）。
func BenchmarkCheckAndBind(b *testing.B) {
	const q = `SELECT robot_id, name FROM robot_profile
	            WHERE tenant_id = :tenant AND robot_id > ? ORDER BY robot_id LIMIT ?`
	opt := statementOptions{}
	args := []any{"r0001", 100}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := compile(q, opt).bind("t-bench", args); err != nil {
			b.Fatal(err)
		}
	}
}

// キャッシュを使わない場合（＝毎回 SQL を舐める場合）の比較用。
func BenchmarkCheckAndBind_NoCache(b *testing.B) {
	const q = `SELECT robot_id, name FROM robot_profile
	            WHERE tenant_id = :tenant AND robot_id > ? ORDER BY robot_id LIMIT ?`
	opt := statementOptions{}
	args := []any{"r0001", 100}
	b.ReportAllocs()
	for b.Loop() {
		if err := checkStatement(q, opt); err != nil {
			b.Fatal(err)
		}
		if _, _, err := bindTenant(q, "t-bench", args); err != nil {
			b.Fatal(err)
		}
	}
}
