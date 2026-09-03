package kascontract_test

// 契約テスト: 同じベクタを MySQL と SQLite の両方に流し、
// **同じ domain 結果**になることを確かめる。
//
//	MYSQL_DSN=... go test ./internal/kascontract/ -run TestContract -v
//
// ドライバの生の戻り値は engine で違う（EXP-10）。だが domain へ正規化した結果は一致する。
// 一致しない項目は、KAS が要求する意味を先に定義し、正規化で吸収する（それがこの層の仕事）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/kascontract"
	"github.com/aq35/sample_manual/internal/sqlitefacts"
)

func runVector(t *testing.T, ctx context.Context, eng kascontract.Engine, v kascontract.Vector) []string {
	t.Helper()
	if err := eng.Reset(ctx); err != nil {
		t.Fatalf("%s reset: %v", eng.Name(), err)
	}
	var results []string
	for i, op := range v.Ops {
		switch op.Kind {
		case "put":
			got, err := eng.Put(ctx, kascontract.Record{
				Key: op.Key, Payload: op.Payload, Note: op.Note, Count: op.Count, UpdatedA: op.Updated,
			})
			if err != nil {
				t.Fatalf("%s %s op%d put: %v", eng.Name(), v.ID, i, err)
			}
			results = append(results, string(got))
			if op.WantUpsert != "" && got != op.WantUpsert {
				t.Errorf("[%s] %s op%d: put=%s want=%s", eng.Name(), v.ID, i, got, op.WantUpsert)
			}
		case "cas":
			got, err := eng.CAS(ctx, op.Key, op.Version, op.Payload)
			if err != nil {
				t.Fatalf("%s %s op%d cas: %v", eng.Name(), v.ID, i, err)
			}
			results = append(results, string(got))
			if op.WantCAS != "" && got != op.WantCAS {
				t.Errorf("[%s] %s op%d: cas=%s want=%s", eng.Name(), v.ID, i, got, op.WantCAS)
			}
		case "lease":
			got, fence, err := eng.AcquireLease(ctx, op.Tenant, op.Owner, op.NowMs, op.TTLMs)
			if err != nil {
				t.Fatalf("%s %s op%d lease: %v", eng.Name(), v.ID, i, err)
			}
			results = append(results, fmt.Sprintf("%s:fence=%d", got, fence))
			if op.WantLease != "" && got != op.WantLease {
				t.Errorf("[%s] %s op%d: lease=%s want=%s", eng.Name(), v.ID, i, got, op.WantLease)
			}
			if op.WantFence != 0 && fence != op.WantFence {
				t.Errorf("[%s] %s op%d: fence=%d want=%d", eng.Name(), v.ID, i, fence, op.WantFence)
			}
		case "get":
			rec, ok, err := eng.Get(ctx, op.Key)
			if err != nil {
				t.Fatalf("%s %s op%d get: %v", eng.Name(), v.ID, i, err)
			}
			results = append(results, fmt.Sprintf("exists=%v", ok))
			if op.WantExists != nil && ok != *op.WantExists {
				t.Errorf("[%s] %s op%d: exists=%v want=%v", eng.Name(), v.ID, i, ok, *op.WantExists)
			}
			if op.WantRecord != nil && ok {
				checkRecord(t, eng.Name(), v.ID, i, rec, *op.WantRecord)
			}
		default:
			t.Fatalf("知らない op: %s", op.Kind)
		}
	}
	return results
}

func checkRecord(t *testing.T, eng, id string, i int, got, want kascontract.Record) {
	t.Helper()
	if got.Payload != want.Payload {
		t.Errorf("[%s] %s op%d: payload=%q want=%q", eng, id, i, got.Payload, want.Payload)
	}
	if want.Count != got.Count {
		t.Errorf("[%s] %s op%d: count=%d want=%d（整数境界の往復）", eng, id, i, got.Count, want.Count)
	}
	if want.UpdatedA != "" && got.UpdatedA != want.UpdatedA {
		t.Errorf("[%s] %s op%d: updated=%q want=%q（日時の往復）", eng, id, i, got.UpdatedA, want.UpdatedA)
	}
	// NULL と "" の区別
	if (got.Note == nil) != (want.Note == nil) {
		t.Errorf("[%s] %s op%d: note nil? got=%v want=%v（NULL と空文字の区別）",
			eng, id, i, got.Note == nil, want.Note == nil)
	} else if got.Note != nil && want.Note != nil && *got.Note != *want.Note {
		t.Errorf("[%s] %s op%d: note=%q want=%q", eng, id, i, *got.Note, *want.Note)
	}
}

func TestContract_両engineで同じdomain結果(t *testing.T) {
	ctx := context.Background()

	// SQLite（pure Go）は常に使える
	sdb, _, cleanup, err := sqlitefacts.PureGo.OpenTemp(sqlitefacts.DefaultPragmas())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sqe := kascontract.SQLiteEngine(sdb)
	if err := sqe.Setup(ctx); err != nil {
		t.Fatal(err)
	}

	// MySQL は MYSQL_DSN があるときだけ
	var mye kascontract.Engine
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		mdb, err := sql.Open("mysql", dsn)
		if err == nil && mdb.PingContext(ctx) == nil {
			t.Cleanup(func() { _ = mdb.Close() })
			mye = kascontract.MySQLEngine(mdb)
			if err := mye.Setup(ctx); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Log("MySQL に繋がらないので SQLite だけで契約を確かめる")
		}
	} else {
		t.Log("MYSQL_DSN 未設定。SQLite だけで契約を確かめる（MySQL 側は別途）")
	}

	for _, v := range kascontract.ContractVectors() {
		t.Run(v.ID, func(t *testing.T) {
			sres := runVector(t, ctx, sqe, v)
			t.Logf("sqlite: %v", sres)
			if mye != nil {
				mres := runVector(t, ctx, mye, v)
				t.Logf("mysql : %v", mres)
				// ★同じベクタで domain 結果列が一致すること
				if fmt.Sprint(sres) != fmt.Sprint(mres) {
					t.Errorf("engine 間で domain 結果が違う\n  sqlite=%v\n  mysql =%v", sres, mres)
				}
			}
		})
	}
}

// TestContract_ベクタをJSONに書き出す は、言語非依存のベクタファイルを更新する
// （EXP_RECORD=1 のときだけ repo に書く。他言語の実装がそのまま読める形）。
func TestContract_ベクタをJSONに書き出す(t *testing.T) {
	vectors := kascontract.ContractVectors()
	doc := map[string]any{
		"contract":  "kas-sql-semantics",
		"meter":     "kascontract/1",
		"note":      "ドライバの戻り値ではなく domain の結果型を期待値にしている。MySQL/SQLite の両方が同じ結果を返す。",
		"live_only": kascontract.LiveOnlyItems(),
		"vectors":   vectors,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("EXP_RECORD") == "" {
		t.Logf("ベクタ %d 本（EXP_RECORD=1 で docs/kas/contract-vectors.json に書き出す）", len(vectors))
		return
	}
	root, _ := filepath.Abs("../..")
	out := filepath.Join(root, "docs", "kas", "contract-vectors.json")
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("書き出した: %s（%d 本）", out, len(vectors))
}
