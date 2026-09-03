package repo

// EXP-8: SQL 検査の fuzz。
//
//	go test ./internal/repo/ -run FuzzCheckStatement -fuzz FuzzCheckStatement -fuzztime 30s
//	go test ./internal/repo/ -run 'FuzzCheckStatement|TestGuardProperties'   # 既知の入力だけ
//
// ★この検査は security boundary ではない（文字列を見ているだけ）。
// fuzz の目的は「どこまでは守れるのか」を機械的に確かめ、
// 守れない形を **既知の穴として明文化する** こと。

import (
	"strings"
	"testing"
)

// 検査が満たすべき性質。fuzz でも通常テストでも同じものを確かめる。
func guardProperties(t *testing.T, sql string) {
	t.Helper()
	opt := statementOptions{}

	// ① どんな入力でも panic しない
	err := checkStatement(sql, opt)

	norm := normalize(sql)
	// ★kindOf などキーワードを見る関数には小文字版を渡す。
	// 検査本体が case-sensitive な :tenant 判定と case-insensitive なキーワード判定を
	// 分けたので、性質テスト側も同じ分け方に合わせないと、
	// 大文字の SQL を素通りさせる（＝測定器のほうが穴になる）。
	low := lower(norm)
	kind := kindOf(low)

	// ② :tenant を（文字列やコメントの外に）持たない UPDATE / DELETE は必ず弾く
	if (kind == kindUpdate || kind == kindDelete) && !strings.Contains(norm, TenantToken) && err == nil {
		t.Errorf("テナント指定の無い %v を通した: %q", kind, sql)
	}

	// ③ 複数文を1回で投げる形は通さない
	//    （driver の設定次第で実行されうる。検査側で落とす）
	if hasMultipleStatements(low) && err == nil {
		t.Errorf("複数の文を通した: %q", sql)
	}

	// ④ 複数表の UPDATE / DELETE（JOIN 付き）は通さない
	//    :tenant が片方の表にしか掛からず、もう一方が無条件に書き換わりうる
	if (kind == kindUpdate || kind == kindDelete) && looksMultiTableWrite(low) && err == nil {
		t.Errorf("複数表の書き換えを通した: %q", sql)
	}

	// ⑤ 通ったなら、束縛したあとに :tenant が残らず、引数の数が一致する
	if err == nil {
		c := compile(sql, opt)
		if c.err != nil {
			return
		}
		args := make([]any, c.argCount)
		bound, boundArgs, berr := c.bind("t-1", args)
		if berr != nil {
			t.Errorf("検査は通ったのに束縛に失敗した: %q: %v", sql, berr)
			return
		}
		// 文字列リテラルの中の :tenant は残ってよい（ただの文字列なので）。
		// 「置換されるべき位置に残っていないこと」を見る。
		if strings.Contains(normalize(bound), TenantToken) {
			t.Errorf("束縛後に %s が残っている: %q", TenantToken, bound)
		}
		if len(boundArgs) != c.argCount+len(c.tenantAt) {
			t.Errorf("引数の数が合わない: %q", sql)
		}
	}
}

// TestGuardProperties は、代表的な入力に対して上の性質を確かめる。
// fuzz を回さなくても（CI でも）常に走る。
func TestGuardProperties(t *testing.T) {
	for _, sql := range guardSeeds() {
		guardProperties(t, sql)
	}
}

// FuzzCheckStatement は SQL らしき文字列を生成して性質を壊しにいく。
func FuzzCheckStatement(f *testing.F) {
	for _, s := range guardSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		if len(sql) > 4096 {
			t.Skip()
		}
		guardProperties(t, sql)
	})
}

// guardSeeds は種。実際に踏んだ形・踏みそうな形を全部入れておく。
func guardSeeds() []string {
	return []string{
		// 正しい形
		"SELECT a FROM t WHERE tenant_id = :tenant LIMIT 10",
		"UPDATE t SET a = ? WHERE tenant_id = :tenant AND id = ?",
		"DELETE FROM t WHERE tenant_id = :tenant AND id = ?",
		"INSERT INTO t (tenant_id, id) VALUES (:tenant, ?)",
		"SELECT COUNT(*) FROM t WHERE tenant_id = :tenant",

		// テナント指定なし
		"UPDATE t SET a = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"UPDATE t SET a = 1",
		"DELETE FROM t",

		// 文字列・コメントで隠す
		"UPDATE t SET note = ':tenant' WHERE id = 1",
		"UPDATE t SET a = 1 -- :tenant\n WHERE id = 1",
		"UPDATE t SET a = 1 /* :tenant */ WHERE id = 1",
		"UPDATE `:tenant` SET a = 1 WHERE id = 1",
		"UPDATE t SET a = \":tenant\" WHERE id = 1",

		// 複数文
		"UPDATE t SET a = 1 WHERE tenant_id = :tenant AND id = ?; DROP TABLE u",
		"SELECT 1 WHERE tenant_id = :tenant LIMIT 1; SELECT 2",
		"UPDATE t SET a=1 WHERE tenant_id=:tenant AND id=?;",

		// 複数表の書き換え
		"UPDATE a JOIN b ON a.id = b.id SET b.v = 1 WHERE a.tenant_id = :tenant",
		"UPDATE a, b SET b.v = 1 WHERE a.tenant_id = :tenant AND a.id = b.id",
		"DELETE a FROM a JOIN b ON a.id = b.id WHERE a.tenant_id = :tenant",
		"DELETE a, b FROM a JOIN b ON a.id = b.id WHERE a.tenant_id = :tenant",

		// INSERT ... SELECT（元表がテナントで絞られていない）
		"INSERT INTO t (tenant_id, id) SELECT :tenant, id FROM u",
		"INSERT INTO t (tenant_id, id) SELECT tenant_id, id FROM u WHERE tenant_id = :tenant",

		// CTE・副問い合わせ
		"WITH x AS (SELECT id FROM u) SELECT id FROM x WHERE tenant_id = :tenant LIMIT 10",
		"SELECT a FROM t WHERE tenant_id = :tenant AND id IN (SELECT id FROM u) LIMIT 10",
		"UPDATE t SET a = (SELECT MAX(v) FROM u) WHERE tenant_id = :tenant AND id = ?",

		// 空白・大小・改行
		"update\tt\nset a = 1\nwhere tenant_id = :tenant and id = ?",
		"UPDATE t SET a = 1 WHERE tenant_id = :tenant AND id = ?",
		"   ",
		"",
		"SELECT",
		":tenant",

		// プレースホルダの数が合わない
		"UPDATE t SET a = ?, b = ? WHERE tenant_id = :tenant",
		"UPDATE t SET a = 1 WHERE tenant_id = :tenant AND id = ? AND x = ?",
	}
}
