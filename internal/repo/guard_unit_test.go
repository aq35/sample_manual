package repo

// DB を使わない検査のテスト。MYSQL_DSN が無くても走る。
// 他プロジェクトへ持って行くときは、この層の検査だけでも十分に効く。

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckStatement(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		opt   statementOptions
		errIs error
	}{
		{"主キー指定の更新", "UPDATE t SET a=? WHERE tenant_id = :tenant AND id = ?", statementOptions{}, nil},
		{"テナント指定なし", "UPDATE t SET a=? WHERE id = ?", statementOptions{}, ErrMissingTenant},
		{"WHERE なし", "UPDATE t SET a = :tenant", statementOptions{}, ErrUnsafeStatement},
		{"DELETE に WHERE なし", "DELETE FROM t", statementOptions{}, ErrMissingTenant},
		{"SELECT に LIMIT なし", "SELECT a FROM t WHERE tenant_id = :tenant", statementOptions{}, ErrTooManyRows},
		{"SELECT に LIMIT あり", "SELECT a FROM t WHERE tenant_id = :tenant LIMIT 10", statementOptions{}, nil},
		{"COUNT は LIMIT 不要", "SELECT COUNT(*) FROM t WHERE tenant_id = :tenant", statementOptions{}, nil},
		{"1行取得は LIMIT 不要", "SELECT a FROM t WHERE tenant_id = :tenant AND id=?", statementOptions{singleRow: true}, nil},
		{"INSERT", "INSERT INTO t (tenant_id, id) VALUES (:tenant, ?)", statementOptions{}, nil},
		{"跨ぐことを明示すれば通る", "SELECT COUNT(*) FROM t", statementOptions{allowCrossTenant: true}, nil},
		// 文字列リテラルの中に書いてあっても、条件として認めない
		{"文字列の中の :tenant", "UPDATE t SET note = ':tenant' WHERE id = ?", statementOptions{}, ErrMissingTenant},
		// コメントで隠しても同じ
		{"コメントの中の :tenant", "UPDATE t SET a=? -- :tenant\n WHERE id=?", statementOptions{}, ErrMissingTenant},
	}
	for _, c := range cases {
		err := checkStatement(c.sql, c.opt)
		switch {
		case c.errIs == nil && err != nil:
			t.Errorf("%s: 通るはずが %v", c.name, err)
		case c.errIs != nil && !errors.Is(err, c.errIs):
			t.Errorf("%s: %v を期待したが %v", c.name, c.errIs, err)
		}
	}
}

func TestBindTenant(t *testing.T) {
	q := "UPDATE t SET a=?, b=? WHERE tenant_id = :tenant AND id = ?"
	bound, args, err := bindTenant(q, "t-1", []any{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bound, TenantToken) {
		t.Fatalf(":tenant が残っている: %s", bound)
	}
	// 位置引数なので、:tenant の位置にテナントが差し込まれていること
	want := []any{1, 2, "t-1", 3}
	if len(args) != len(want) {
		t.Fatalf("引数の数が違う: %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("引数の並びが違う: %v (期待 %v)", args, want)
		}
	}

	// 引数の数が合わなければ実行しない
	if _, _, err := bindTenant("UPDATE t SET a=? WHERE tenant_id=:tenant", "t-1", nil); err == nil {
		t.Error("引数不足を検出できていない")
	}
	if _, _, err := bindTenant("UPDATE t SET a=1 WHERE tenant_id=:tenant", "t-1", []any{9}); err == nil {
		t.Error("引数過剰を検出できていない")
	}

	// 文字列リテラルの中の ? や :tenant は置換しない
	bound, args, err = bindTenant("INSERT INTO t (a,b) VALUES ('? :tenant', :tenant)", "t-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound, "'? :tenant'") {
		t.Fatalf("文字列リテラルを壊している: %s", bound)
	}
	if len(args) != 1 || args[0] != "t-1" {
		t.Fatalf("引数が違う: %v", args)
	}
}

func TestExpect(t *testing.T) {
	if err := ExpectOne.check(1); err != nil {
		t.Error(err)
	}
	if err := ExpectOne.check(2); !errors.Is(err, ErrUnexpectedRowCount) {
		t.Errorf("2行を通してしまった: %v", err)
	}
	if err := ExpectOne.check(0); !errors.Is(err, ErrUnexpectedRowCount) {
		t.Errorf("0行を通してしまった: %v", err)
	}
	if err := ExpectAtMostOne.check(0); err != nil {
		t.Error(err)
	}
	if err := ExpectAny.check(1000); err != nil {
		t.Error(err)
	}
	if err := ExpectAtMost(10).check(11); !errors.Is(err, ErrUnexpectedRowCount) {
		t.Error("上限を超えたのに通した")
	}
}

func TestHardenDSN(t *testing.T) {
	for _, dsn := range []string{
		"u:p@tcp(127.0.0.1:3306)/db",
		"u:p@tcp(127.0.0.1:3306)/db?parseTime=false",
		"u:p?x@tcp(127.0.0.1:3306)/db?charset=utf8mb4",
	} {
		got, err := hardenDSN(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		for _, want := range []string{"parseTime=true", "sql_safe_updates=1", "loc=UTC"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s → %s に %s が無い", dsn, got, want)
			}
		}
		if !strings.HasPrefix(got, strings.SplitN(dsn, "?", 2)[0]) && !strings.Contains(got, "/db?") {
			t.Errorf("接続先が壊れている: %s", got)
		}
	}
}
