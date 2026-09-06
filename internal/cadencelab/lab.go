// Package cadencelab は EXP-12（ワーカーのポーリング頻度）の実験本体。
//
// 問い: スケジュール／複数ロボットの命令／実績を DB で表したとき、
// **ワーカーはどれくらいの頻度で DB を引き、命令を出すのが良いか。**
//
// トレードオフ:
//   - 速く引く（短い interval）→ 命令の遅延は下がるが、空振りの問い合わせが増え DB を食う
//   - 遅く引く（長い interval）→ DB は軽いが、due になった命令が interval だけ待たされる
//
// 測るもの: dispatch 遅延（scheduled_for → 実際に出した時刻）の p50/p95/p99、
// 問い合わせ回数、空振り率、捌いた件数。
package cadencelab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/aq35/sample_manual/internal/expkit"
)

//go:embed schema.sql
var schemaSQL string

// Setup はスキーマを作る。
func Setup(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		//smlint:allow loopquery 理由: スキーマ作成。固定の DDL を順に流すだけ
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("cadence schema: %w\n%s", err, stmt)
		}
	}
	return nil
}

// Config は1回の測定条件。
type Config struct {
	Tenant   string
	Commands int           // 命令の総数
	Window   time.Duration // これらが due になる時間幅（到着レート = Commands/Window）
	Interval time.Duration // ポーリング間隔
	Batch    int           // 1回のポーリングで最大何件掴むか
	Jitter   time.Duration // interval にかけるゆらぎ（複数テナントの同時ポーリングをずらす）
	Wake     bool          // producer が「due が来た」と in-process で起こすか
	HoldFor  time.Duration // 捌き終わってもこの時間までポーリングを続ける（空振りの無駄を測る）
	Fence    int64
}

// Result は測定結果。
type Result struct {
	Dispatched    int64
	Polls         int64 // DB を引いた回数
	EmptyPolls    int64 // 0 件だった回数（空振り）
	Latency       expkit.LatencyStats
	Elapsed       time.Duration
	QueriesPerSec float64
	EmptyRatio    float64
}

// Seed は命令を Window にわたって散らして入れる（到着レートの模擬）。
func Seed(ctx context.Context, db *sql.DB, cfg Config, start time.Time) error {
	//smlint:allow rowsaffected 理由: 実験前の後片付け。消える行が 0 でも正しい
	if _, err := db.ExecContext(ctx, "DELETE FROM cmd_command WHERE tenant_id = ?", cfg.Tenant); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO cmd_command (tenant_id, command_id, robot_id, type, payload, state, idem_key, fence, scheduled_for)
		 VALUES (?,?,?,?,?, 'pending', ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for i := 0; i < cfg.Commands; i++ {
		off := time.Duration(int64(cfg.Window) * int64(i) / int64(max(cfg.Commands, 1)))
		due := start.Add(off)
		id := fmt.Sprintf("c%08d", i)
		//smlint:allow loopquery 理由: 実験データ投入。準備済み文で Commands 件入れる
		//smlint:allow rowsaffected 理由: 投入。入るかはエラーで判る
		if _, err := stmt.ExecContext(ctx, cfg.Tenant, id, fmt.Sprintf("r%04d", i%200),
			"move", "", "idem-"+id, cfg.Fence, due); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Run はポーリングワーカーを1つ回して、全命令を捌くまで（or ctx 終了まで）測る。
func Run(ctx context.Context, db *sql.DB, cfg Config) (Result, error) {
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	lat := expkit.NewLatency()
	var res Result
	start := time.Now()

	poll := func() (int, error) {
		now := time.Now()
		rows, err := db.QueryContext(ctx,
			`SELECT command_id, scheduled_for FROM cmd_command
			  WHERE tenant_id = ? AND state = 'pending' AND scheduled_for <= ?
			  ORDER BY scheduled_for LIMIT ?`, cfg.Tenant, now, cfg.Batch)
		res.Polls++
		if err != nil {
			return 0, err
		}
		type due struct {
			id  string
			sch time.Time
		}
		var batch []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.sch); err != nil {
				_ = rows.Close()
				return 0, err
			}
			batch = append(batch, d)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			res.EmptyPolls++
			return 0, nil
		}
		// 掴んで dispatched にする（claim）。ここで dispatch 遅延を記録。
		for _, d := range batch {
			//smlint:allow rowsaffected 理由: claim。掴めたかは状態遷移で担保、遅延の記録が目的
			//smlint:allow loopquery 理由: 掴んだ命令を1件ずつ発行する実験そのもの
			if _, err := db.ExecContext(ctx,
				`UPDATE cmd_command SET state='dispatched', dispatched_at=?, attempts=attempts+1
				  WHERE tenant_id=? AND command_id=? AND state='pending'`,
				time.Now(), cfg.Tenant, d.id); err != nil {
				return 0, err
			}
			lat.Record(time.Since(d.sch)) // scheduled_for からの遅れ
			res.Dispatched++
		}
		return len(batch), nil
	}

	// 全部 pending が捌けるまで回す（安全のため ctx でも止まる）。
	hold := cfg.Window + 10*time.Second
	if cfg.HoldFor > 0 {
		hold = cfg.HoldFor
	}
	deadline := start.Add(hold)
	interval := cfg.Interval
	for time.Now().Before(deadline) && ctx.Err() == nil {
		n, err := poll()
		if err != nil {
			return res, err
		}
		// 残りを確認（全部 dispatched になったら終わり）
		var remaining int
		//smlint:allow loopquery 理由: ポーリングループの残数確認。実験の制御そのもの
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM cmd_command WHERE tenant_id=? AND state='pending'",
			cfg.Tenant).Scan(&remaining); err != nil {
			return res, err
		}
		// 捌き終わったら普通は終わる。HoldFor 指定時は、その時刻まで空振りを承知で続ける
		// （速く引くほど空振りが増える＝無駄を測るため）。
		if remaining == 0 {
			if cfg.HoldFor == 0 || time.Now().After(deadline) {
				break
			}
		}
		// 次のポーリングまで待つ。Wake なら「掴めた直後は間を置かず続ける」。
		wait := interval
		if cfg.Wake && n > 0 {
			wait = 0 // まだ捌けるものがあるなら間を置かない（push 相当）
		}
		if cfg.Jitter > 0 {
			wait += time.Duration(int64(cfg.Jitter) * int64(res.Polls%7) / 7)
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}

	res.Elapsed = time.Since(start)
	res.Latency = lat.Stats()
	if res.Elapsed > 0 {
		res.QueriesPerSec = float64(res.Polls) / res.Elapsed.Seconds()
	}
	if res.Polls > 0 {
		res.EmptyRatio = float64(res.EmptyPolls) / float64(res.Polls)
	}
	return res, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RunWake は「producer が due 到来を in-process で知らせる」押し込み型。
//
// ポーリングのように定期的に空振りせず、due になった瞬間にだけ起こされて捌く。
// 疎な到着（低レート）で「低遅延・空振りゼロ」を両立できるかを見る。
// ★これが本来の wake。「掴めた直後に間を置かず続ける」busy-continue は wake ではない
// （連続到着では結局ずっと引くことになる。EXP-12 で反証済み）。
func RunWake(ctx context.Context, db *sql.DB, cfg Config) (Result, error) {
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	lat := expkit.NewLatency()
	var res Result
	start := time.Now()

	// producer: seed した各命令の scheduled_for を読み、その時刻に signal を送る。
	dues, err := dueTimes(ctx, db, cfg.Tenant)
	if err != nil {
		return res, err
	}
	signal := make(chan struct{}, 1)
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		for _, d := range dues {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(d)):
			}
			select { // coalesce: 溜まっていれば1つで足りる
			case signal <- struct{}{}:
			default:
			}
		}
	}()

	drain := func() error {
		now := time.Now()
		rows, err := db.QueryContext(ctx,
			`SELECT command_id, scheduled_for FROM cmd_command
			  WHERE tenant_id = ? AND state = 'pending' AND scheduled_for <= ?
			  ORDER BY scheduled_for LIMIT ?`, cfg.Tenant, now, cfg.Batch)
		res.Polls++
		if err != nil {
			return err
		}
		type due struct {
			id  string
			sch time.Time
		}
		var batch []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.sch); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, d)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			res.EmptyPolls++
			return nil
		}
		for _, d := range batch {
			//smlint:allow rowsaffected 理由: claim。状態遷移で担保、遅延の記録が目的
			//smlint:allow loopquery 理由: 掴んだ命令を1件ずつ発行する実験そのもの
			if _, err := db.ExecContext(ctx,
				`UPDATE cmd_command SET state='dispatched', dispatched_at=?, attempts=attempts+1
				  WHERE tenant_id=? AND command_id=? AND state='pending'`,
				time.Now(), cfg.Tenant, d.id); err != nil {
				return err
			}
			lat.Record(time.Since(d.sch))
			res.Dispatched++
		}
		return nil
	}

	deadline := start.Add(cfg.Window + 10*time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-signal:
			if err := drain(); err != nil {
				return res, err
			}
		case <-prodDone:
			_ = drain() // 最後の取りこぼしを1回さらう
			goto done
		}
		var remaining int
		//smlint:allow loopquery 理由: wake ループの残数確認。実験の制御そのもの
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM cmd_command WHERE tenant_id=? AND state='pending'",
			cfg.Tenant).Scan(&remaining); err == nil && remaining == 0 {
			select {
			case <-prodDone:
				goto done
			default:
			}
		}
	}
done:
	res.Elapsed = time.Since(start)
	res.Latency = lat.Stats()
	if res.Elapsed > 0 {
		res.QueriesPerSec = float64(res.Polls) / res.Elapsed.Seconds()
	}
	if res.Polls > 0 {
		res.EmptyRatio = float64(res.EmptyPolls) / float64(res.Polls)
	}
	return res, nil
}

func dueTimes(ctx context.Context, db *sql.DB, tenant string) ([]time.Time, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT scheduled_for FROM cmd_command WHERE tenant_id=? AND state='pending' ORDER BY scheduled_for", tenant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
