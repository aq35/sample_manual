// Package racelab は EXP-66（Web と Worker のレースコンディション）の実験本体。
//
// Web（人が操作）と Worker（裏で処理）は同じ行を同時に触る。守り方は2つ:
//   - 状態遷移は status の CAS: UPDATE ... WHERE status='期待値' して affected_rows=1 で勝者を確定する。
//     claim（pending→in_progress）も complete（in_progress→completed）も、遷移元の状態を WHERE に入れる。
//   - 入力の同時編集は version（楽観ロック）: worker は読んだ version を WHERE に入れて書く。
//     Web が先に編集して version が進んでいたら affected_rows=0 → worker は結果を捨てて読み直す。
//
// 3つのレースを再現する:
//
//	A) 二重 claim: 複数 worker が同じ pending を掴む（CAS なしだと二重処理）
//	B) cancel 中の complete: Web が cancel した仕事を worker が完了で上書き（cancel が消える）
//	C) 入力の途中編集: worker が古い input で計算した結果を、更新後の行に書く（stale result）
package racelab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"
)

//go:embed schema.sql
var schemaSQL string

func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定 DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("race schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// seed は n 件を指定状態・owner で入れ直す。
func seed(ctx context.Context, db *sql.DB, tenant, status, owner string, n int) error {
	//smlint:allow rowsaffected 理由: 実験前の後片付け
	if _, err := db.ExecContext(ctx, "DELETE FROM race_job WHERE tenant_id=?", tenant); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("INSERT INTO race_job (tenant_id, id, status, owner, input, version) VALUES ")
	var args []any
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?,?,?,?,0)")
		args = append(args, tenant, i, status, owner, 100+i) // input は 100+i
	}
	//smlint:allow rowsaffected 理由: 投入
	_, err := db.ExecContext(ctx, b.String(), args...)
	return err
}

func countStatus(ctx context.Context, db *sql.DB, tenant, status string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM race_job WHERE tenant_id=? AND status=?", tenant, status).Scan(&n)
	return n, err
}

// ---- A) 二重 claim ----

// ClaimResult は claim レースの結果。
type ClaimResult struct {
	TotalClaims  int64 // worker が「掴んだ」と思った総数
	DoubleClaims int64 // 2人以上に掴まれた行の数（=二重処理される仕事）
}

// ClaimRace は n 件の pending を concurrency 人の worker が奪い合う。
//   - useCAS=false: SELECT で見つけて、無条件 UPDATE で in_progress にする（読取り→書込みの隙間で二重取得）
//   - useCAS=true : UPDATE ... WHERE status='pending' し、affected_rows=1 のときだけ「掴んだ」と数える
//
// CAS は1行を1人にしか渡さない（DoubleClaims=0）。無条件版は同じ行を複数人が掴みうる。
func ClaimRace(ctx context.Context, db *sql.DB, tenant string, n, concurrency int, useCAS bool) (ClaimResult, error) {
	if err := seed(ctx, db, tenant, "pending", "", n); err != nil {
		return ClaimResult{}, err
	}
	var mu sync.Mutex
	claims := make(map[int64]int) // id -> 掴まれた回数
	var firstErr error
	setErr := func(e error) {
		mu.Lock()
		if firstErr == nil && e != nil {
			firstErr = e
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("w%d", w)
			for ctx.Err() == nil {
				var id int64
				//smlint:allow loopquery 理由: claim 競合の生成。worker が次の pending を探す動作そのもの（実験対象）
				err := db.QueryRowContext(ctx,
					"SELECT id FROM race_job WHERE tenant_id=? AND status='pending' ORDER BY id LIMIT 1",
					tenant).Scan(&id)
				if err == sql.ErrNoRows {
					return
				}
				if err != nil {
					setErr(err)
					return
				}
				if useCAS {
					//smlint:allow loopquery 理由: claim 競合の生成（実験対象）
					res, err := db.ExecContext(ctx,
						"UPDATE race_job SET status='in_progress', owner=? WHERE tenant_id=? AND id=? AND status='pending'",
						owner, tenant, id)
					if err != nil {
						setErr(err)
						return
					}
					aff, _ := res.RowsAffected()
					if aff == 1 { // 自分が勝ったときだけ「掴んだ」
						mu.Lock()
						claims[id]++
						mu.Unlock()
					}
				} else {
					// 読取り→書込みの隙間を作る（二重取得を再現しやすくする）
					time.Sleep(time.Millisecond)
					//smlint:allow loopquery 理由: claim 競合の生成（実験対象）
					//smlint:allow rowsaffected 理由: 無条件版は勝敗を見ない（それが事故）
					if _, err := db.ExecContext(ctx,
						"UPDATE race_job SET status='in_progress', owner=? WHERE tenant_id=? AND id=?",
						owner, tenant, id); err != nil {
						setErr(err)
						return
					}
					mu.Lock()
					claims[id]++ // 見つけた＝掴んだ、と思い込む（状態を確認していない）
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	var res ClaimResult
	for _, c := range claims {
		res.TotalClaims += int64(c)
		if c > 1 {
			res.DoubleClaims++
		}
	}
	return res, firstErr
}

// ---- B) cancel 中の complete ----

// CancelThenComplete は「Web が cancel した直後に worker が complete する」を n 件で決定的に起こす。
//   - guarded=false: worker は UPDATE ... WHERE id（状態を見ない）→ cancel を completed で上書き（cancel が消える）
//   - guarded=true : worker は UPDATE ... WHERE id AND status='in_progress' AND owner=?（CAS）→ affected_rows=0 で破棄
//
// 返り値は「cancel したのに completed になった件数（=消えた cancel）」。guarded なら 0。
func CancelThenComplete(ctx context.Context, db *sql.DB, tenant string, n int, guarded bool) (int64, error) {
	if err := seed(ctx, db, tenant, "in_progress", "w1", n); err != nil {
		return 0, err
	}
	for i := 0; i < n; i++ {
		// Web: in_progress のものを cancel（Web 側も状態 CAS で守る）
		//smlint:allow loopquery 理由: レースの決定的再現（1件ずつ Web→Worker の順で起こす）
		//smlint:allow rowsaffected 理由: Web の cancel は前提として成功する
		if _, err := db.ExecContext(ctx,
			"UPDATE race_job SET status='cancelled' WHERE tenant_id=? AND id=? AND status='in_progress'",
			tenant, i); err != nil {
			return 0, err
		}
		// Worker: 完了を書きに来る（cancel 済みと知らずに）
		var q string
		var args []any
		if guarded {
			q = "UPDATE race_job SET status='completed', result=input WHERE tenant_id=? AND id=? AND status='in_progress' AND owner='w1'"
			args = []any{tenant, i}
		} else {
			q = "UPDATE race_job SET status='completed', result=input WHERE tenant_id=? AND id=?"
			args = []any{tenant, i}
		}
		//smlint:allow loopquery 理由: レースの決定的再現
		//smlint:allow rowsaffected 理由: 上書きが起きたか否かは最後に status を数えて判定
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return 0, err
		}
	}
	// cancel したのに completed になった件数 = 消えた cancel
	completed, err := countStatus(ctx, db, tenant, "completed")
	return completed, err
}

// ---- C) 入力の途中編集（stale result） ----

// StaleInputRace は「worker が読んだ後に Web が input を編集し、worker が古い入力の結果を書く」を n 件で決定的に起こす。
//   - guarded=false: worker は UPDATE ... WHERE id（version を見ない）→ 更新後の行に stale な result を書く
//   - guarded=true : worker は UPDATE ... WHERE id AND version=?（読んだ version）→ affected_rows=0 で破棄・読み直し
//
// 返り値は「stale な結果を書いた件数」。guarded なら 0。
func StaleInputRace(ctx context.Context, db *sql.DB, tenant string, n int, guarded bool) (int64, error) {
	if err := seed(ctx, db, tenant, "in_progress", "w1", n); err != nil {
		return 0, err
	}
	var stale int64
	for i := 0; i < n; i++ {
		// Worker: 読む（input と version をスナップショット）
		var input, ver int64
		//smlint:allow loopquery 理由: レースの決定的再現。worker の read（この後 Web が編集する）そのもの
		if err := db.QueryRowContext(ctx,
			"SELECT input, version FROM race_job WHERE tenant_id=? AND id=?", tenant, i).Scan(&input, &ver); err != nil {
			return 0, err
		}
		result := input * 2 // 読んだ input から計算した結果

		// Web: worker の計算中に input を編集（version を進める）
		//smlint:allow loopquery 理由: レースの決定的再現（worker の read 後に Web が編集）
		//smlint:allow rowsaffected 理由: Web の編集は前提として成功する
		if _, err := db.ExecContext(ctx,
			"UPDATE race_job SET input=input+1000, version=version+1 WHERE tenant_id=? AND id=?",
			tenant, i); err != nil {
			return 0, err
		}

		// Worker: 結果を書きに来る（古い input で計算した result）
		var q string
		var args []any
		if guarded {
			q = "UPDATE race_job SET result=?, status='completed' WHERE tenant_id=? AND id=? AND version=?"
			args = []any{result, tenant, i, ver}
		} else {
			q = "UPDATE race_job SET result=?, status='completed' WHERE tenant_id=? AND id=?"
			args = []any{result, tenant, i}
		}
		//smlint:allow loopquery 理由: レースの決定的再現
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		aff, _ := res.RowsAffected()
		if aff == 1 {
			stale++ // 書けてしまった＝古い入力の結果が残った（guarded では version 不一致で 0 になる）
		}
	}
	return stale, nil
}
