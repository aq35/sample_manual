package repo

// EXP-8: SQL 検査の fuzz — 結果の記録。
//
//	go test ./internal/repo/ -run TestEXP8 -v
//
// fuzz 本体は guard_fuzz_test.go。ここは
// 「見つかった抜け道」と「回帰入力」を記録に残すための実験単位。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
)

func TestEXP8_SQL検査のfuzz(t *testing.T) {
	rec := expkit.NewRecorder("EXP-8", "sql-guard-fuzz",
		"SQL 検査（文字列ベース）の抜け道を fuzz で探す")
	rec.Env(expkit.CaptureEnv(t.Context(), nil))
	rec.Freeze(strings.Join([]string{
		"1) 現在の検査は文字列を見ているだけなので、抜け道がある。",
		"2) 種（手で書いた形）だけでも、複数文・複数表の書き換えが通る。",
		"3) fuzz を回すと、手では思いつかない形（引用符が閉じていない、",
		"   :tenant の大文字小文字が混ざる）でさらに通る。",
		"4) 見つけたものは検査に足せるが、**この検査が security boundary になることはない**。",
	}, " "))
	rec.Workload("seeds", len(guardSeeds())).
		Workload("properties", []string{
			"panic しない", "テナント指定の無い UPDATE/DELETE を通さない",
			"複数文を通さない", "複数表の書き換えを通さない",
			"通した文は束縛後に :tenant が残らない",
		}).
		Injection("fuzzer", "go test -fuzz、30〜90秒 × 3回")

	// ---- 種と回帰入力に対して、いま性質が保たれているか ----
	corpus := loadFuzzCorpus(t)
	all := append(append([]string{}, guardSeeds()...), corpus...)
	for _, sql := range all {
		guardProperties(t, sql)
	}

	rec.Add(expkit.Variant{
		Name:     "修正前（種だけで見つかった抜け道）",
		Desc:     "手で書いた種（34 個）を性質に掛けただけで見つかったもの",
		Accident: true,
		Counters: map[string]int64{"findings": 6},
		Notes: []string{
			"複数文を通した: `UPDATE ... WHERE tenant_id = :tenant AND id = ?; DROP TABLE u`",
			"複数文を通した: `SELECT 1 WHERE tenant_id = :tenant LIMIT 1; SELECT 2`",
			"複数表の書き換えを通した: `UPDATE a JOIN b ON a.id = b.id SET b.v = 1 WHERE a.tenant_id = :tenant`",
			"複数表の書き換えを通した: `UPDATE a, b SET b.v = 1 WHERE a.tenant_id = :tenant AND a.id = b.id`",
			"複数表の書き換えを通した: `DELETE a FROM a JOIN b ON ... WHERE a.tenant_id = :tenant`",
			"複数表の書き換えを通した: `DELETE a, b FROM a JOIN b ON ... WHERE a.tenant_id = :tenant`",
		},
	})
	rec.Add(expkit.Variant{
		Name:     "fuzz が追加で見つけたもの",
		Desc:     "手では思いつかなかった形",
		Accident: true,
		Counters: map[string]int64{"findings": 2},
		Notes: []string{
			"`:tenant\":tenant0` — 引用符が閉じていないため、2つめの :tenant が束縛されずに driver へ渡っていた（10秒で発見）",
			"`:tenAnt':tenant'0` — 検査は小文字化して「トークンあり」と判定するが、" +
				"置換は大文字小文字を区別するため置換されない。1分で発見",
			"どちらも『検査は通ったのに、束縛後に :tenant が残る』という性質違反として出た",
		},
	})
	rec.Add(expkit.Variant{
		Name: "修正後",
		Desc: "引用符・コメントの閉じ忘れを拒否／複数文を拒否／複数表の書き換えを拒否／" +
			"トークン判定を大文字小文字の区別ありに統一",
		Counters: map[string]int64{
			"findings":          0,
			"regression_inputs": int64(len(corpus)),
			"seed_inputs":       int64(len(guardSeeds())),
			"fuzz_execs_90s":    1226829,
		},
		Notes: []string{
			"90秒 / 1,226,829 実行で新たな違反なし（10,000 exec/秒 前後）",
			"見つかった入力は testdata/fuzz/FuzzCheckStatement/ に残り、以後は毎回のテストで回る",
			"★修正の途中で、性質テスト自身が穴になっていたことも分かった。" +
				"検査本体を『:tenant は大文字小文字を区別／キーワードは区別しない』に分けたとき、" +
				"テスト側が小文字化を合わせていないと、大文字の SQL を素通りさせていた",
		},
	})

	rec.Scope(
		"検査対象は internal/repo の checkStatement / bindTenant（文字列ベース）",
		"fuzz は go test -fuzz。1入力 4096 バイトまで",
		"見つかった入力はすべて回帰テストへ昇格させている",
	)
	rec.Uncertain(
		"**この検査は security boundary ではない。** 悪意のある入力を止める仕組みとしては使えない",
		"パーサを使っていないので、方言（MySQL 特有の構文）の網羅は保証できない",
		"INSERT ... SELECT の元表がテナントで絞られているかは、この検査では判定できない（既知の穴）",
		"動的に組み立てた SQL（文字列連結）の中身は、組み立て後の形しか見ていない",
		"fuzz は 90 秒 × 数回。もっと長く回せば別の形が出る可能性がある",
	)
	rec.Artifact(
		"internal/repo/guard_fuzz_test.go: 性質（panic しない・通してはいけない形）を書けば "+
			"fuzz と通常テストの両方で回る形",
		"testdata/fuzz/FuzzCheckStatement/: 見つかった入力の回帰コーパス",
	)
	rec.Next("EXP-9 go/analysis による保守性の検査")

	files, err := rec.Save(strings.Join([]string{
		"種だけで 6 件、fuzz でさらに 2 件の抜け道が見つかり、いずれも検査に反映した。",
		"90秒・122万実行で新たな違反は出なくなったが、",
		"パーサを使っていない以上これは『うっかり』を止める仕組みであって、",
		"security boundary ではない（INSERT ... SELECT の元表など、既知の穴も残っている）。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("種 %d 個 + 回帰入力 %d 個で性質を確認した。結果: %v", len(guardSeeds()), len(corpus), files)
}

// loadFuzzCorpus は go test -fuzz が残した入力を読む。
// 形式は `go test fuzz v1` + 型付きリテラルの並び。
func loadFuzzCorpus(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("testdata", "fuzz", "FuzzCheckStatement")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // まだ1件も見つかっていない場合
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "string(") {
				continue
			}
			lit := strings.TrimSuffix(strings.TrimPrefix(line, "string("), ")")
			s, err := strconv.Unquote(lit)
			if err != nil {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}
