// Package effectlab は EXP-1（外部 effect の途中で落ちる）の実験本体。
//
// 確かめたいのは「DB がきれいか」ではなく **外の世界で effect が何回起きたか**。
// そのため、判定はアプリの DB ではなく外部サービス側の記録と突き合わせて行う。
package effectlab

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
)

//go:embed schema.sql
var schemaSQL string

// 状態。実験指示書の区分をそのまま持つ。
const (
	StateNotStarted            = "NOT_STARTED"            // まだ何もしていない
	StateDispatchReserved      = "DISPATCH_RESERVED"      // 送ると決めて記録した（まだ送っていないかもしれない）
	StateOutcomeUnknown        = "OUTCOME_UNKNOWN"        // 送ったが、結果が分からない ★これを「失敗」にしない
	StateClaimedSuccess        = "CLAIMED_SUCCESS"        // 相手が成功と言った（受領書あり）
	StateIndependentlyObserved = "INDEPENDENTLY_OBSERVED" // 別経路で effect の存在を確認した
	StateCompleted             = "COMPLETED"              // 受領書を持って完了した
)

// 故障注入の地点。
const (
	PointBeforeRequestSaved         = "before_request_saved"
	PointAfterDispatchReserved      = "after_dispatch_reserved"
	PointBeforeServiceReceives      = "before_service_receives"
	PointAfterEffectBeforeResponse  = "after_effect_before_response" // サービス側の hook から殺される
	PointAfterResponseBeforeReceipt = "after_response_before_receipt"
	PointAfterReceiptBeforeState    = "after_receipt_before_state"
	PointAfterStateBeforeReply      = "after_state_before_reply"
)

// KillPoints は「全地点で1回ずつ殺す」を回すための一覧。
var KillPoints = expkit.KillPoints{
	PointBeforeRequestSaved,
	PointAfterDispatchReserved,
	PointBeforeServiceReceives,
	PointAfterEffectBeforeResponse,
	PointAfterResponseBeforeReceipt,
	PointAfterReceiptBeforeState,
	PointAfterStateBeforeReply,
}

// Strategy は比較する方式。
type Strategy string

const (
	// StrategyNaive: 冪等キー無し。落ちたら最初からやり直す。
	StrategyNaive Strategy = "naive_retry"
	// StrategyIdem: 冪等キーを付けて送る。重複を弾くのは **相手** の責任。
	StrategyIdem Strategy = "idempotency_key"
	// StrategyOutbox: 業務状態と「送るつもり」を同じトランザクションで保存してから送る。
	StrategyOutbox Strategy = "transactional_outbox"
	// StrategyObserve: outbox に加えて、結果不明のものは **送り直す前に相手へ問い合わせる**。
	StrategyObserve Strategy = "outbox_then_observe"
)

// Config は1回の実行の設定。
type Config struct {
	DSN         string
	ServiceURL  string
	Tenant      string
	Strategy    Strategy
	Requests    int
	Amount      int
	HTTPTimeout time.Duration

	// KillAt / KillOn: 何番目の要求のどの地点で落ちるか。
	KillOn int
	KS     *expkit.KillSwitch

	// Recover: 再起動後の処理として走らせるか。
	Recover bool

	Out io.Writer
}

func (c *Config) setDefaults() {
	if c.Amount == 0 {
		c.Amount = 100
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = 2 * time.Second
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.KS == nil {
		c.KS = expkit.NewKillSwitch()
	}
}

// Open は DB を開く。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	return db, nil
}

// Migrate は実験用の表を作る。
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		var lines []string
		for _, ln := range strings.Split(stmt, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		s := strings.TrimSpace(strings.Join(lines, "\n"))
		if s == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// Reset は指定テナントの行を消す。
func Reset(ctx context.Context, db *sql.DB, tenant string) error {
	for _, t := range []string{"effect_request", "effect_outbox"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t+" WHERE tenant_id = ?", tenant); err != nil {
			return err
		}
	}
	return nil
}

// Lab は1プロセスぶんの実行。
type Lab struct {
	cfg  Config
	db   *sql.DB
	http *http.Client
}

func New(cfg Config, db *sql.DB) *Lab {
	cfg.setDefaults()
	return &Lab{
		cfg:  cfg,
		db:   db,
		http: &http.Client{Timeout: cfg.HTTPTimeout},
	}
}

func (l *Lab) logf(format string, args ...any) {
	fmt.Fprintf(l.cfg.Out, format+"\n", args...)
}

// RequestID は i 番目の要求の ID。
func RequestID(i int) string { return fmt.Sprintf("req-%04d", i) }

// idemKey は要求ごとに一意で、**再試行しても変わらない**キー。
// 試行ごとに変えてしまうと、相手が重複を弾けない（よくある間違い）。
func idemKey(tenant, requestID string) string { return tenant + ":" + requestID }

// Run は要求を順番に処理する。
func (l *Lab) Run(ctx context.Context) error {
	for i := 1; i <= l.cfg.Requests; i++ {
		id := RequestID(i)
		injecting := i == l.cfg.KillOn
		if err := l.process(ctx, id, injecting); err != nil {
			l.logf("request %s: %v", id, err)
		}
	}
	return nil
}

// point は「この要求で故障注入する」ときだけ地点を通す。
// 常に通すと1件目で落ちてしまい、途中の状態が作れない。
func (l *Lab) point(injecting bool, name string) {
	if injecting {
		l.cfg.KS.Point(name)
	}
}

// process は1件の要求を、状態遷移をコミットしながら進める。
func (l *Lab) process(ctx context.Context, id string, injecting bool) error {
	key := idemKey(l.cfg.Tenant, id)

	// ★naive は「先に呼んで、あとで記録する」。よくある形で、
	// 呼んだ直後に落ちると **記録がどこにも残らない**（＝次の再試行で二重送信になる）。
	if l.cfg.Strategy == StrategyNaive {
		return l.naive(ctx, id, injecting)
	}

	l.point(injecting, PointBeforeRequestSaved)

	// ① 受け付けを保存する。
	//    outbox 方式は「送るつもり」も同じトランザクションで保存する。
	if err := l.saveRequest(ctx, id, key); err != nil {
		return err
	}

	// ② 送ると決めたことを記録する（DISPATCH_RESERVED）。
	//    ここまで来ていれば、再起動後に「送ったかもしれない」と分かる。
	if err := l.reserve(ctx, id); err != nil {
		return err
	}
	l.point(injecting, PointAfterDispatchReserved)

	return l.dispatch(ctx, id, key, injecting)
}

// naive は「外部を呼んでから記録する」方式。
func (l *Lab) naive(ctx context.Context, id string, injecting bool) error {
	l.point(injecting, PointBeforeRequestSaved)
	l.point(injecting, PointAfterDispatchReserved) // naive には予約が無い（記録が無い）
	l.point(injecting, PointBeforeServiceReceives)

	receipt, err := l.charge(ctx, id, "")
	if err != nil {
		// 記録が無いので、この失敗はどこにも残らない
		return fmt.Errorf("結果不明（記録なし）: %w", err)
	}
	l.point(injecting, PointAfterResponseBeforeReceipt)

	if _, err := l.db.ExecContext(ctx,
		`INSERT INTO effect_request (tenant_id, request_id, amount, idem_key, state, attempts, receipt_id, receipt_source)
		 VALUES (?,?,?,?,?,1,?, 'response')
		 ON DUPLICATE KEY UPDATE attempts = attempts + 1, receipt_id = VALUES(receipt_id), state = VALUES(state)`,
		l.cfg.Tenant, id, l.cfg.Amount, "", StateClaimedSuccess, receipt); err != nil {
		return err
	}
	l.point(injecting, PointAfterReceiptBeforeState)

	if _, err := l.db.ExecContext(ctx,
		`UPDATE effect_request SET state = ? WHERE tenant_id = ? AND request_id = ?`,
		StateCompleted, l.cfg.Tenant, id); err != nil {
		return err
	}
	l.point(injecting, PointAfterStateBeforeReply)
	return nil
}

func (l *Lab) saveRequest(ctx context.Context, id, key string) error {
	if l.cfg.Strategy == StrategyOutbox || l.cfg.Strategy == StrategyObserve {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO effect_request (tenant_id, request_id, amount, idem_key, state)
			 VALUES (?,?,?,?,?) ON DUPLICATE KEY UPDATE amount = VALUES(amount)`,
			l.cfg.Tenant, id, l.cfg.Amount, key, StateNotStarted); err != nil {
			return err
		}
		// ★業務状態と「送るつもり」を同じトランザクションで確定する。
		// これが transactional outbox の本体。片方だけ残ることがなくなる。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO effect_outbox (tenant_id, request_id, state)
			 VALUES (?,?,'PENDING') ON DUPLICATE KEY UPDATE state = state`,
			l.cfg.Tenant, id); err != nil {
			return err
		}
		return tx.Commit()
	}

	_, err := l.db.ExecContext(ctx,
		`INSERT INTO effect_request (tenant_id, request_id, amount, idem_key, state)
		 VALUES (?,?,?,?,?) ON DUPLICATE KEY UPDATE amount = VALUES(amount)`,
		l.cfg.Tenant, id, l.cfg.Amount, key, StateNotStarted)
	return err
}

func (l *Lab) reserve(ctx context.Context, id string) error {
	if _, err := l.db.ExecContext(ctx,
		`UPDATE effect_request SET state = ?, attempts = attempts + 1
		  WHERE tenant_id = ? AND request_id = ?`,
		StateDispatchReserved, l.cfg.Tenant, id); err != nil {
		return err
	}
	if l.cfg.Strategy == StrategyOutbox || l.cfg.Strategy == StrategyObserve {
		if _, err := l.db.ExecContext(ctx,
			`UPDATE effect_outbox SET state='RESERVED', reserved_at = CURRENT_TIMESTAMP(3)
			  WHERE tenant_id = ? AND request_id = ?`, l.cfg.Tenant, id); err != nil {
			return err
		}
	}
	return nil
}

// dispatch は実際に外部サービスを呼ぶ。
func (l *Lab) dispatch(ctx context.Context, id, key string, injecting bool) error {
	l.point(injecting, PointBeforeServiceReceives)

	receipt, err := l.charge(ctx, id, key)
	if err != nil {
		// ★ここが要点。timeout もエラーも「effect が起きていない」ことを意味しない。
		// 失敗として片付けず、OUTCOME_UNKNOWN にする。
		// エラー文字列は last_error に残すだけで、受領書としては保存しない。
		if uerr := l.markUnknown(ctx, id, err); uerr != nil {
			return uerr
		}
		return fmt.Errorf("結果不明: %w", err)
	}

	l.point(injecting, PointAfterResponseBeforeReceipt)

	// ② 受領書を保存する（相手が発行した ID のみ）
	if _, err := l.db.ExecContext(ctx,
		`UPDATE effect_request SET state = ?, receipt_id = ?, receipt_source = 'response', last_error = NULL
		  WHERE tenant_id = ? AND request_id = ?`,
		StateClaimedSuccess, receipt, l.cfg.Tenant, id); err != nil {
		return err
	}
	l.point(injecting, PointAfterReceiptBeforeState)

	// ③ 完了にする（受領書を持っていることが条件）
	if err := l.complete(ctx, id); err != nil {
		return err
	}
	l.point(injecting, PointAfterStateBeforeReply)
	return nil
}

func (l *Lab) complete(ctx context.Context, id string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// ★受領書が無ければ完了にしない。
	// 「呼び出しが例外を投げなかった」だけでは完了の根拠にならない。
	res, err := tx.ExecContext(ctx,
		`UPDATE effect_request SET state = ?
		  WHERE tenant_id = ? AND request_id = ? AND receipt_id IS NOT NULL`,
		StateCompleted, l.cfg.Tenant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("受領書が無いので完了にできない: %s", id)
	}
	if l.cfg.Strategy == StrategyOutbox || l.cfg.Strategy == StrategyObserve {
		if _, err := tx.ExecContext(ctx,
			`UPDATE effect_outbox SET state='DONE' WHERE tenant_id = ? AND request_id = ?`,
			l.cfg.Tenant, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (l *Lab) markUnknown(ctx context.Context, id string, cause error) error {
	msg := cause.Error()
	if len(msg) > 250 {
		msg = msg[:250]
	}
	_, err := l.db.ExecContext(ctx,
		`UPDATE effect_request SET state = ?, last_error = ?
		  WHERE tenant_id = ? AND request_id = ? AND state <> ?`,
		StateOutcomeUnknown, msg, l.cfg.Tenant, id, StateCompleted)
	return err
}

// charge は外部サービスを呼ぶ。冪等キーを付けるかは方式による。
func (l *Lab) charge(ctx context.Context, id, key string) (string, error) {
	body, _ := json.Marshal(map[string]any{"request_id": id, "amount": l.cfg.Amount})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.ServiceURL+"/charge", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.cfg.Strategy != StrategyNaive {
		// ★キーは要求ごとに固定。再試行で変えない
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ReceiptID string `json:"receipt_id"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ReceiptID == "" {
		return "", fmt.Errorf("受領書が無い応答")
	}
	return out.ReceiptID, nil
}

// observe は「独立した観測」。相手に effect の有無を問い合わせる。
func (l *Lab) observe(ctx context.Context, id, key string) (string, bool, error) {
	u := fmt.Sprintf("%s/effects?key=%s&request_id=%s", l.cfg.ServiceURL, key, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Effects []struct {
			ReceiptID string `json:"receipt_id"`
		} `json:"effects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	if len(out.Effects) == 0 {
		return "", false, nil
	}
	return out.Effects[0].ReceiptID, true, nil
}

// Recover は再起動後の処理。**ここが方式ごとにいちばん違う。**
func (l *Lab) Recover(ctx context.Context) error {
	known := map[string]row{}
	rows, err := l.db.QueryContext(ctx,
		`SELECT request_id, idem_key, state, COALESCE(receipt_id,'') FROM effect_request WHERE tenant_id = ?`,
		l.cfg.Tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key, &r.state, &r.receipt); err != nil {
			_ = rows.Close()
			return err
		}
		known[r.id] = r
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for i := 1; i <= l.cfg.Requests; i++ {
		id := RequestID(i)
		r, ok := known[id]
		if ok && r.state == StateCompleted {
			continue
		}
		key := idemKey(l.cfg.Tenant, id)

		if !ok {
			// 記録がまったく無い。呼び出し側から見れば「返事が無かった要求」。
			// naive はここで何も知らないまま送り直す（＝effect が二重になりうる）。
			l.logf("recover %s: 記録なし → 最初から処理する", id)
			if err := l.process(ctx, id, false); err != nil {
				l.logf("recover %s: %v", id, err)
			}
			continue
		}

		switch l.cfg.Strategy {
		case StrategyObserve:
			// ★結果不明を自動で送り直さない。まず相手に聞く。
			receipt, found, err := l.observe(ctx, id, key)
			if err != nil {
				l.logf("recover %s: 観測に失敗（状態は不明のまま据え置く）: %v", id, err)
				continue
			}
			if found {
				if _, err := l.db.ExecContext(ctx,
					`UPDATE effect_request SET state = ?, receipt_id = ?, receipt_source='observation'
					  WHERE tenant_id = ? AND request_id = ?`,
					StateIndependentlyObserved, receipt, l.cfg.Tenant, id); err != nil {
					return err
				}
				if err := l.complete(ctx, id); err != nil {
					l.logf("recover %s: %v", id, err)
				}
				l.logf("recover %s: 観測で effect の存在を確認 → 完了（送り直さない）", id)
				continue
			}
			l.logf("recover %s: 観測しても effect が無い → 送る", id)
			if err := l.dispatch(ctx, id, key, false); err != nil {
				l.logf("recover %s: %v", id, err)
			}

		default:
			// naive / idem / outbox: 未完了なら送り直す。
			// 二重 effect を防げるかどうかは **相手が冪等キーを守るか** に懸かっている。
			if err := l.dispatch(ctx, id, key, false); err != nil {
				l.logf("recover %s: %v", id, err)
			}
		}
	}
	return nil
}

type row struct {
	id, key, state, receipt string
}

// Report は DB 側から見た最終状態。
type Report struct {
	States   map[string]int    `json:"states"`
	Row      map[string]string `json:"row_state"` // request_id -> state
	Receipts map[string]string `json:"receipts"`  // request_id -> receipt_id（空なら受領書なし）
	Sources  map[string]string `json:"receipt_sources"`
	Attempts map[string]int    `json:"attempts"`
	Errors   map[string]string `json:"last_errors"`
}

func Load(ctx context.Context, db *sql.DB, tenant string) (Report, error) {
	rep := Report{
		States:   map[string]int{},
		Row:      map[string]string{},
		Receipts: map[string]string{},
		Sources:  map[string]string{},
		Attempts: map[string]int{},
		Errors:   map[string]string{},
	}
	rows, err := db.QueryContext(ctx,
		`SELECT request_id, state, COALESCE(receipt_id,''), COALESCE(receipt_source,''), attempts, COALESCE(last_error,'')
		   FROM effect_request WHERE tenant_id = ?`, tenant)
	if err != nil {
		return rep, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state, receipt, source, lastErr string
		var attempts int
		if err := rows.Scan(&id, &state, &receipt, &source, &attempts, &lastErr); err != nil {
			return rep, err
		}
		rep.States[state]++
		rep.Row[id] = state
		rep.Receipts[id] = receipt
		rep.Sources[id] = source
		rep.Attempts[id] = attempts
		if lastErr != "" {
			rep.Errors[id] = lastErr
		}
	}
	return rep, rows.Err()
}

// StateOf は1件の状態（無ければ空文字）。
func (r Report) StateOf(id string) string { return r.Row[id] }
