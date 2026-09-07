// Package tenantworker は、これまでの実験の結論を1つのワーカーに結線する。
//
//   - EXP-2 lease/fence  … 担当テナントを lease で決め、fence で古い担当を弾く
//   - EXP-14/16 fan-out  … 担当テナント集合を1クエリに畳んでポーリング（テナント別に切り分けて処理）
//   - EXP-17 backoff     … 空振りで間隔を伸ばし、仕事で詰める
//   - EXP-1 outbox/観測  … action の戻り値を receipt にせず、cmd_result に独立観測を残す
//   - EXP-12 頻度        … dispatch 遅延は間隔で決まる。batch/interval ≥ 到着レート
//
// ★テナント越えを起こさない設計:
//
//	畳むのは「担当（lease 保有）テナントに限定した読み取り選択」だけ。
//	取得後は tenant_id で切り分け、処理は必ずテナント単位（fence 付き）。
package tenantworker

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aq35/sample_manual/internal/lease"
	"github.com/aq35/sample_manual/internal/model"
)

// Command は1つの命令（ワーカーが処理する単位）。
type Command struct {
	Tenant  model.TenantID
	ID      string
	RobotID string
	Type    string
	Payload string
	Fence   uint64 // 処理時の lease fence（古い担当の実績を弾く）
}

// Handler はテナント単位の処理。★別テナントの Command は絶対に渡らない。
// 戻り値の error が nil なら done、非 nil なら failed として記録する。
// timeout など「出したが結果不明」は ErrOutcomeUnknown を返す（自動再実行しない）。
type Handler func(ctx context.Context, cmd Command) error

// ErrOutcomeUnknown は「命令は出したが結果が不明」（EXP-1）。
var ErrOutcomeUnknown = fmt.Errorf("outcome unknown")

// Options は調整値。
type Options struct {
	Owner       string        // この worker の識別子（lease の持ち主名）
	Batch       int           // 1ラウンドで捌く最大件数（担当ぶん）
	MinInterval time.Duration // 適応的バックオフの下限
	MaxInterval time.Duration // 上限
}

func (o *Options) setDefaults() {
	if o.Batch <= 0 {
		o.Batch = 200
	}
	if o.MinInterval <= 0 {
		o.MinInterval = 100 * time.Millisecond
	}
	if o.MaxInterval <= 0 {
		o.MaxInterval = 2 * time.Second
	}
}

// Stats は運用で見る数字。
type Stats struct {
	Owned      atomic.Int64 // 現在担当しているテナント数
	Polls      atomic.Int64
	EmptyPolls atomic.Int64
	Dispatched atomic.Int64
	Failed     atomic.Int64
	Unknown    atomic.Int64
	Misrouted  atomic.Int64 // ★テナント越えの処理（0 でなければ分離の穴）
}

// Dispatcher は担当テナントの命令を捌く。
type Dispatcher struct {
	db      *sql.DB
	leases  *lease.Manager
	handler Handler
	opt     Options

	candidates []model.TenantID // 担当候補（この中から lease を取れたものを担当する）
	Stats      Stats
}

// New は Dispatcher を作る。candidates はこの worker が担当を試みるテナント。
func New(db *sql.DB, leases *lease.Manager, candidates []model.TenantID, h Handler, opt Options) *Dispatcher {
	opt.setDefaults()
	return &Dispatcher{db: db, leases: leases, handler: h, opt: opt, candidates: candidates}
}

// Run は ctx が切れるまでポーリングし続ける。終了時に担当リースを返す。
func (d *Dispatcher) Run(ctx context.Context) error {
	interval := d.opt.MinInterval
	defer d.releaseAll(context.Background()) // ctx が切れても返す

	for ctx.Err() == nil {
		owned, fences, err := d.acquireOwned(ctx)
		if err != nil {
			return err
		}
		d.Stats.Owned.Store(int64(len(owned)))

		n := 0
		if len(owned) > 0 {
			n, err = d.pollAndDispatch(ctx, owned, fences)
			if err != nil {
				return err
			}
		}

		// 適応的バックオフ（EXP-17）
		if n > 0 {
			interval = d.opt.MinInterval
		} else {
			interval *= 2
			if interval > d.opt.MaxInterval {
				interval = d.opt.MaxInterval
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	return nil
}

// acquireOwned は候補テナントのうち lease を取れた（or 更新できた）ものを返す。
func (d *Dispatcher) acquireOwned(ctx context.Context) ([]model.TenantID, map[model.TenantID]uint64, error) {
	var owned []model.TenantID
	fences := map[model.TenantID]uint64{}
	for _, tn := range d.candidates {
		// まず更新を試み、だめなら取得を試みる。
		if ok, err := d.leases.Renew(ctx, tn, d.opt.Owner); err != nil {
			return nil, nil, err
		} else if ok {
			l, err := d.leases.Get(ctx, tn)
			if err != nil {
				return nil, nil, err
			}
			owned = append(owned, tn)
			fences[tn] = l.Fence
			continue
		}
		//smlint:allow loopquery 理由: 候補テナントごとに lease を取りに行くのは担当決めそのもの
		l, ok, err := d.leases.Acquire(ctx, tn, d.opt.Owner)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			owned = append(owned, tn)
			fences[tn] = l.Fence
		}
		// 取れない = 他の worker が担当。何もしない（二重起動しない）。
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i] < owned[j] })
	return owned, fences, nil
}

// pollAndDispatch は担当テナントを1クエリで畳んで引き、テナント別に切り分けて処理する。
func (d *Dispatcher) pollAndDispatch(ctx context.Context, owned []model.TenantID, fences map[model.TenantID]uint64) (int, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(owned)), ",")
	query := `SELECT tenant_id, command_id, robot_id, type, payload FROM cmd_command
	           WHERE tenant_id IN (` + ph + `) AND state = 'pending' AND scheduled_for <= ?
	        ORDER BY scheduled_for LIMIT ?`
	args := make([]any, 0, len(owned)+2)
	ownedSet := map[model.TenantID]bool{}
	for _, tn := range owned {
		args = append(args, string(tn))
		ownedSet[tn] = true
	}
	args = append(args, time.Now(), d.opt.Batch)

	rows, err := d.db.QueryContext(ctx, query, args...)
	d.Stats.Polls.Add(1)
	if err != nil {
		return 0, err
	}
	var cmds []Command
	for rows.Next() {
		var c Command
		var tn string
		if err := rows.Scan(&tn, &c.ID, &c.RobotID, &c.Type, &c.Payload); err != nil {
			_ = rows.Close()
			return 0, err
		}
		c.Tenant = model.TenantID(tn)
		cmds = append(cmds, c)
	}
	_ = rows.Close()
	if len(cmds) == 0 {
		d.Stats.EmptyPolls.Add(1)
		return 0, nil
	}

	for _, c := range cmds {
		// ★担当外テナントが返ってきたら分離の穴。処理しない。
		if !ownedSet[c.Tenant] {
			d.Stats.Misrouted.Add(1)
			continue
		}
		c.Fence = fences[c.Tenant]
		d.processOne(ctx, c)
	}
	return len(cmds), nil
}

// processOne は1つの命令を、テナント単位のハンドラで処理し、結果を独立に記録する。
func (d *Dispatcher) processOne(ctx context.Context, c Command) {
	// claim: pending → dispatched（fence 付き。古い担当は fence が合わず claim できない）
	res, err := d.db.ExecContext(ctx,
		`UPDATE cmd_command SET state='dispatched', dispatched_at=?, attempts=attempts+1, fence=?
		  WHERE tenant_id=? AND command_id=? AND state='pending'`,
		time.Now(), c.Fence, string(c.Tenant), c.ID)
	if err != nil {
		d.Stats.Failed.Add(1)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return // 既に他が掴んだ
	}

	// ハンドラ実行（テナント単位）。戻り値を receipt にせず、独立に観測して記録する（EXP-1）。
	herr := d.handler(ctx, c)
	status, state := "success", "done"
	switch {
	case herr == nil:
		d.Stats.Dispatched.Add(1)
	case herr == ErrOutcomeUnknown:
		status, state = "unknown", "unknown" // 自動再実行しない。人／別プロセスが確定する
		d.Stats.Unknown.Add(1)
	default:
		status, state = "failure", "failed"
		d.Stats.Failed.Add(1)
	}
	d.recordResult(ctx, c, status)
	//smlint:allow rowsaffected 理由: 状態遷移は上の claim で担保済み。ここは結末の反映
	_, _ = d.db.ExecContext(ctx,
		"UPDATE cmd_command SET state=? WHERE tenant_id=? AND command_id=?", state, string(c.Tenant), c.ID)
}

// recordResult は実績を追記する（独立観測。action の戻り値ではない）。
func (d *Dispatcher) recordResult(ctx context.Context, c Command, status string) {
	//smlint:allow rowsaffected 理由: 実績の追記。1 行入る前提
	_, _ = d.db.ExecContext(ctx,
		`INSERT INTO cmd_result (tenant_id, command_id, observed_at, status, fence)
		 VALUES (?,?,?,?,?)`,
		string(c.Tenant), c.ID, time.Now(), status, c.Fence)
}

func (d *Dispatcher) releaseAll(ctx context.Context) {
	for _, tn := range d.candidates {
		//smlint:allow loopquery 理由: 終了時に担当リースを返す。テナントごとに1回
		_ = d.leases.Release(ctx, tn, d.opt.Owner)
	}
}
