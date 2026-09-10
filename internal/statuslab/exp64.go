package statuslab

// EXP-64（回収の実験本体）: in_progress → pending の「回収」を、
//   ① 時間を見ない（status だけ）で書くと、生きている担当の行まで奪って二重実行になる。
//   ② heartbeat_at の経過時間で絞り、CAS の affected_rows で「自分が奪えたか」を確認すると止まる。
// 基準時刻はすべて DB の NOW(3)（worker 側の time.Now は使わない＝クロックスキュー回避）。
//
// 状態: 0=pending 1=in_progress 2=completed
// docs/worker-state-time.md の §5 の検証（3）に対応。

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

const (
	statusInProgress = 1
	statusCompleted  = 2
)

// SeedJobs は回収実験の初期状態を入れる。
//   - alive 件: status=in_progress・heartbeat が「今」（担当が生きている＝奪ってはいけない）
//   - stale 件: status=in_progress・heartbeat が staleAge だけ過去（担当が落ちた＝回収すべき）
//   - noise 件: status=completed（回収対象ではない山。索引が無いと回収クエリがこれを舐める）
//
// heartbeat は DB の NOW(3) 基準で入れる。id は alive→stale→noise の順に連番。
func SeedJobs(ctx context.Context, db *sql.DB, table, tenant string, alive, stale, noise int, staleAge time.Duration) error {
	//smlint:allow loopquery 理由: 実験前の後片付け
	//smlint:allow rowsaffected 理由: 後片付け
	if _, err := db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id=?", tenant); err != nil {
		return err
	}
	staleSec := int(staleAge.Seconds())
	insert := func(from, to, status int, fresh bool) error {
		const chunk = 500
		for s := from; s < to; s += chunk {
			e := s + chunk
			if e > to {
				e = to
			}
			var b strings.Builder
			b.WriteString("INSERT INTO " + table + " (tenant_id, id, status, owner, heartbeat_at) VALUES ")
			var args []any
			for i := s; i < e; i++ {
				if i > s {
					b.WriteString(",")
				}
				switch {
				case status != statusInProgress:
					b.WriteString("(?,?,?,?,NULL)")
					args = append(args, tenant, i, status, "")
				case fresh:
					b.WriteString("(?,?,?,?,NOW(3))")
					args = append(args, tenant, i, status, "w-alive")
				default:
					b.WriteString("(?,?,?,?,NOW(3) - INTERVAL ? SECOND)")
					args = append(args, tenant, i, status, "w-dead", staleSec)
				}
			}
			//smlint:allow loopquery 理由: 一括投入（500行/文）
			//smlint:allow rowsaffected 理由: 投入
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := insert(0, alive, statusInProgress, true); err != nil {
		return err
	}
	if err := insert(alive, alive+stale, statusInProgress, false); err != nil {
		return err
	}
	return insert(alive+stale, alive+stale+noise, statusCompleted, false)
}

// AliveInProgress は「今まさに処理中で奪ってはいけない行」の数（in_progress かつ heartbeat が新しい）。
// leaseSec より新しい heartbeat を「生きている」とみなす。二重実行の危険はこの数がいくつ奪われたかで測る。
func AliveInProgress(ctx context.Context, db *sql.DB, table, tenant string, leaseSec int) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+
			" WHERE tenant_id=? AND status=? AND heartbeat_at >= NOW(3) - INTERVAL ? SECOND",
		tenant, statusInProgress, leaseSec).Scan(&n)
	return n, err
}

// ReclaimAll は【危険な①】時間を見ない回収。in_progress を全部 pending に戻す。
// 生きている担当の行も奪うので二重実行になる。返り値は戻した件数。
func ReclaimAll(ctx context.Context, db *sql.DB, table, tenant string) (int64, error) {
	res, err := db.ExecContext(ctx,
		"UPDATE "+table+" SET status=?, owner='' WHERE tenant_id=? AND status=?",
		statusPending, tenant, statusInProgress)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReclaimStale は【正しい②】heartbeat が leaseSec より古い in_progress だけを pending に戻す。
// CAS（WHERE に status と heartbeat の両方）で、affected_rows が「実際に奪えた件数」になる。
// 生きている担当（heartbeat が新しい）には一致しないので奪わない。
func ReclaimStale(ctx context.Context, db *sql.DB, table, tenant string, leaseSec int) (int64, error) {
	res, err := db.ExecContext(ctx,
		"UPDATE "+table+" SET status=?, owner='' "+
			"WHERE tenant_id=? AND status=? AND heartbeat_at < NOW(3) - INTERVAL ? SECOND",
		statusPending, tenant, statusInProgress, leaseSec)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FindStale は回収候補（stale な in_progress）を古い順に1ページ引く SELECT。
// 索引 (tenant_id, status, heartbeat_at) の効果を測るために、②の抽出だけを取り出したもの。
func FindStale(ctx context.Context, db *sql.DB, table, tenant string, leaseSec, limit, samples int) expkit.LatencyStats {
	q := "SELECT id FROM " + table +
		" WHERE tenant_id=? AND status=? AND heartbeat_at < NOW(3) - INTERVAL ? SECOND" +
		" ORDER BY heartbeat_at LIMIT ?"
	return measure(ctx, samples, func() error {
		rows, err := db.QueryContext(ctx, q, tenant, statusInProgress, leaseSec, limit)
		if err != nil {
			return err
		}
		return drain(rows)
	})
}
