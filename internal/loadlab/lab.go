// Package loadlab は EXP-4（backpressure と過負荷）の実験本体。
//
// 確かめたいこと:
//   - 入力が処理能力を超えても、メモリが無制限に増えないこと
//   - あるテナントの過負荷が、他のテナントを止めないこと
//   - 捨てた・まとめた・遅らせた事実が、あとから数えられること
//
// 「落ちなかった」ではなく「どう壊れるか・どこで頭打ちになるか」を測る。
package loadlab

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/expkit"
)

//go:embed schema.sql
var schemaSQL string

// Strategy は比較する受け方。
type Strategy string

const (
	// Unbounded: 入ってきたぶんだけ溜める（事故の再現）。
	Unbounded Strategy = "unbounded_queue"
	// BoundedBlock: 有界。いっぱいなら生産側を待たせる（入口へ背圧をかける）。
	BoundedBlock Strategy = "bounded_block"
	// BoundedDrop: 有界。いっぱいなら捨てる（捨てた数を数える）。
	BoundedDrop Strategy = "bounded_drop"
	// Coalesce: キーごとに最新だけ残す（同じ対象の連続更新をまとめる）。
	Coalesce Strategy = "coalesce_latest"
	// CoalesceBatch: まとめたうえで、1トランザクションでまとめて書く。
	CoalesceBatch Strategy = "coalesce_batch"
	// PerTenantQuota: テナントごとに枠を分ける（1テナントの氾濫を隔離する）。
	PerTenantQuota Strategy = "per_tenant_quota"
)

// TenantMix は入力に占めるテナントの割合。
type TenantMix struct {
	ID    string
	Share float64
}

// Config は1回の実行の条件。
type Config struct {
	Strategy   Strategy
	Rate       int // 毎秒の入力件数（全テナント合計）
	Duration   time.Duration
	Tenants    []TenantMix
	Keys       int // テナントあたりの対象数（まとめ効果に効く）
	Capacity   int // 有界キューの大きさ
	BatchSize  int
	FlushEvery time.Duration
	DBDelay    time.Duration // DB 側の遅延（SELECT SLEEP で実際に待たせる）
	Workers    int
	// PayloadBytes は1件が抱えるデータ量（既定 512B）。
	// キューに溜まる量をメモリで見るために持たせている。
	PayloadBytes int
	// DrainAfterStop: 入力停止後、出し切るのを待つ時間。
	// 過負荷では出し切りに何十秒もかかるので、待たずに「残り」を数える。
	DrainAfterStop time.Duration
}

func (c *Config) setDefaults() {
	if c.Rate <= 0 {
		c.Rate = 1000
	}
	if c.Duration <= 0 {
		c.Duration = 3 * time.Second
	}
	if len(c.Tenants) == 0 {
		c.Tenants = []TenantMix{{ID: "t-a", Share: 1}}
	}
	if c.Keys <= 0 {
		c.Keys = 200
	}
	if c.Capacity <= 0 {
		c.Capacity = 1024
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = 200 * time.Millisecond
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.PayloadBytes <= 0 {
		c.PayloadBytes = 512
	}
	if c.DrainAfterStop <= 0 {
		c.DrainAfterStop = 300 * time.Millisecond
	}
}

// Event は1件の入力。
//
// ★payload を持たせてあるのは、キューに溜まる量をメモリで見るため。
// 実際のイベントは JSON なり本文なりを抱えるので、
// 「件数は同じでもメモリは payload の大きさで決まる」ことを再現する。
type Event struct {
	Tenant  string
	Key     string
	Seq     int64
	At      time.Time
	Payload []byte
}

// Result は1回の実行の結果。
type Result struct {
	Produced  int64
	Accepted  int64
	Dropped   int64
	Coalesced int64
	Committed int64
	Txs       int64
	Blocked   time.Duration // 生産側が待たされた合計（背圧の量）
	Leftover  int64         // 入力停止時点で処理できずに残っていた件数
	ByTenant  map[string]int64
	MaxQueue  int64
	Latency   expkit.LatencyStats
	Samples   expkit.SampleSummary
	Elapsed   time.Duration
	Freshness map[string]time.Duration // 最後にコミットできた時点の古さ
}

func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	return db, nil
}

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
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func Reset(ctx context.Context, db *sql.DB, tenants []TenantMix) error {
	for _, t := range tenants {
		if _, err := db.ExecContext(ctx, "DELETE FROM load_item WHERE tenant_id = ?", t.ID); err != nil {
			return err
		}
	}
	return nil
}

// Run は指定の条件で負荷をかけ、結果を返す。
func Run(ctx context.Context, db *sql.DB, cfg Config) (Result, error) {
	cfg.setDefaults()

	r := &runner{cfg: cfg, db: db}
	r.byTenant = map[string]*atomic.Int64{}
	r.freshness = map[string]*atomic.Int64{}
	for _, t := range cfg.Tenants {
		r.byTenant[t.ID] = &atomic.Int64{}
		r.freshness[t.ID] = &atomic.Int64{}
	}
	return r.run(ctx)
}

type runner struct {
	cfg Config
	db  *sql.DB

	produced, accepted, dropped, coalesced atomic.Int64
	committed, txs                         atomic.Int64
	blockedNS                              atomic.Int64
	queueDepth, maxQueue                   atomic.Int64
	byTenant                               map[string]*atomic.Int64
	freshness                              map[string]*atomic.Int64

	latency *expkit.Latency

	// 方式ごとの受け口
	ch     chan Event // 有界
	unb    *unbounded // 無制限
	coal   *coalescer // キーごとに最新だけ残す
	quotas map[string]chan Event
}

func (r *runner) run(ctx context.Context) (Result, error) {
	cfg := r.cfg
	r.latency = expkit.NewLatency()

	sampler := expkit.NewSampler(r.db, 50*time.Millisecond).Custom(func() map[string]float64 {
		return map[string]float64{"queue_depth": float64(r.queueDepth.Load())}
	}).Start()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var consumers sync.WaitGroup
	switch cfg.Strategy {
	case Unbounded:
		r.unb = newUnbounded()
		consumers.Add(1)
		go func() { defer consumers.Done(); r.consumeUnbounded(ctx) }()
	case Coalesce, CoalesceBatch:
		r.coal = newCoalescer()
		consumers.Add(1)
		go func() { defer consumers.Done(); r.consumeCoalesced(ctx) }()
	case PerTenantQuota:
		r.quotas = map[string]chan Event{}
		per := cfg.Capacity / len(cfg.Tenants)
		if per < 1 {
			per = 1
		}
		for _, t := range cfg.Tenants {
			ch := make(chan Event, per)
			r.quotas[t.ID] = ch
			consumers.Add(1)
			go func(ch chan Event) { defer consumers.Done(); r.consumeChannel(ctx, ch) }(ch)
		}
	default:
		r.ch = make(chan Event, cfg.Capacity)
		for i := 0; i < cfg.Workers; i++ {
			consumers.Add(1)
			go func() { defer consumers.Done(); r.consumeChannel(ctx, r.ch) }()
		}
	}

	start := time.Now()
	r.produce(ctx)
	elapsed := time.Since(start)

	// ★出し切るのを待たない。
	// 過負荷の実験では出し切りに何十秒もかかる（それ自体が結果）。
	// 少しだけ猶予を与えてから止め、**残った件数**を測定値として記録する。
	time.Sleep(cfg.DrainAfterStop)
	leftover := r.queueDepth.Load()
	cancel()
	switch cfg.Strategy {
	case Unbounded:
		r.unb.close()
	case Coalesce, CoalesceBatch:
		r.coal.close()
	case PerTenantQuota:
		for _, ch := range r.quotas {
			close(ch)
		}
	default:
		close(r.ch)
	}
	consumers.Wait()

	samples := sampler.Stop()

	res := Result{
		Produced: r.produced.Load(), Accepted: r.accepted.Load(),
		Dropped: r.dropped.Load(), Coalesced: r.coalesced.Load(),
		Committed: r.committed.Load(), Txs: r.txs.Load(),
		Blocked:  time.Duration(r.blockedNS.Load()),
		MaxQueue: r.maxQueue.Load(), Leftover: leftover,
		Latency:  r.latency.Stats(),
		Samples:  expkit.Summarize(samples),
		Elapsed:  elapsed,
		ByTenant: map[string]int64{}, Freshness: map[string]time.Duration{},
	}
	for id, c := range r.byTenant {
		res.ByTenant[id] = c.Load()
	}
	for id, f := range r.freshness {
		if v := f.Load(); v > 0 {
			res.Freshness[id] = time.Since(time.Unix(0, v))
		}
	}
	return res, nil
}

// produce は指定レートで入力を作る。処理が追いつかない場合の振る舞いが方式の違い。
func (r *runner) produce(ctx context.Context) {
	cfg := r.cfg
	rnd := rand.New(rand.NewSource(20260903))

	// テナントの選択表（割合に応じて）
	var picker []string
	for _, t := range cfg.Tenants {
		n := int(t.Share * 100)
		for i := 0; i < n; i++ {
			picker = append(picker, t.ID)
		}
	}
	if len(picker) == 0 {
		picker = []string{cfg.Tenants[0].ID}
	}

	const slot = 2 * time.Millisecond
	perSlot := int(float64(cfg.Rate) * slot.Seconds())
	if perSlot < 1 {
		perSlot = 1
	}
	deadline := time.Now().Add(cfg.Duration)
	ticker := time.NewTicker(slot)
	defer ticker.Stop()

	var seq int64
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for i := 0; i < perSlot; i++ {
			seq++
			tenant := picker[rnd.Intn(len(picker))]
			ev := Event{
				Tenant:  tenant,
				Key:     fmt.Sprintf("k%04d", rnd.Intn(cfg.Keys)),
				Seq:     seq,
				At:      time.Now(),
				Payload: make([]byte, cfg.PayloadBytes),
			}
			r.produced.Add(1)
			r.offer(ctx, ev)
		}
	}
}

// offer は方式ごとの受け入れ。ここが backpressure の本体。
func (r *runner) offer(ctx context.Context, ev Event) {
	switch r.cfg.Strategy {
	case Unbounded:
		r.unb.push(ev)
		r.accepted.Add(1)
		r.setDepth(int64(r.unb.len()))

	case BoundedDrop:
		select {
		case r.ch <- ev:
			r.accepted.Add(1)
			r.setDepth(int64(len(r.ch)))
		default:
			// ★捨てる。捨てた事実を数える（黙って落とさない）
			r.dropped.Add(1)
		}

	case BoundedBlock:
		start := time.Now()
		select {
		case r.ch <- ev:
		case <-ctx.Done():
			return
		}
		// 待たされた時間＝入口へかかった背圧の量
		r.blockedNS.Add(int64(time.Since(start)))
		r.accepted.Add(1)
		r.setDepth(int64(len(r.ch)))

	case Coalesce, CoalesceBatch:
		if replaced := r.coal.put(ev); replaced {
			// 同じキーの古い値を上書きした＝まとめた
			r.coalesced.Add(1)
		}
		r.accepted.Add(1)
		r.setDepth(int64(r.coal.len()))

	case PerTenantQuota:
		ch := r.quotas[ev.Tenant]
		select {
		case ch <- ev:
			r.accepted.Add(1)
			var total int64
			for _, c := range r.quotas {
				total += int64(len(c))
			}
			r.setDepth(total)
		default:
			r.dropped.Add(1) // そのテナントの枠が満杯。他のテナントには影響しない
		}
	}
}

func (r *runner) setDepth(n int64) {
	r.queueDepth.Store(n)
	for {
		max := r.maxQueue.Load()
		if n <= max || r.maxQueue.CompareAndSwap(max, n) {
			return
		}
	}
}

func (r *runner) consumeChannel(ctx context.Context, ch chan Event) {
	batch := make([]Event, 0, r.cfg.BatchSize)
	for {
		select {
		case <-ctx.Done():
			return // 出し切らずに止める（残りは Leftover として数える）
		case ev, ok := <-ch:
			if !ok {
				if len(batch) > 0 {
					r.write(ctx, batch)
				}
				return
			}
			batch = append(batch, ev)
			if len(batch) >= r.cfg.BatchSize {
				r.write(ctx, batch)
				batch = batch[:0]
			}
			r.setDepth(int64(len(ch)))
		}
	}
}

func (r *runner) consumeUnbounded(ctx context.Context) {
	batch := make([]Event, 0, r.cfg.BatchSize)
	for {
		if ctx.Err() != nil {
			break
		}
		ev, ok := r.unb.pop()
		if !ok {
			break
		}
		batch = append(batch, ev)
		if len(batch) >= r.cfg.BatchSize {
			r.write(ctx, batch)
			batch = batch[:0]
		}
		r.setDepth(int64(r.unb.len()))
	}
	if len(batch) > 0 {
		r.write(ctx, batch)
	}
}

func (r *runner) consumeCoalesced(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-r.coal.closed:
			r.flushCoalesced(context.WithoutCancel(ctx))
			return
		case <-ctx.Done():
			r.flushCoalesced(context.WithoutCancel(ctx))
			return
		}
		r.flushCoalesced(ctx)
	}
}

func (r *runner) flushCoalesced(ctx context.Context) {
	batch := r.coal.drain()
	if len(batch) == 0 {
		return
	}
	// 主キー順に並べてから書く（デッドロック回避、調査 §4.3）
	sort.Slice(batch, func(i, j int) bool {
		if batch[i].Tenant != batch[j].Tenant {
			return batch[i].Tenant < batch[j].Tenant
		}
		return batch[i].Key < batch[j].Key
	})
	if r.cfg.Strategy == CoalesceBatch {
		r.write(ctx, batch)
		return
	}
	for _, ev := range batch {
		r.write(ctx, []Event{ev})
	}
}

// write は1トランザクションで書く。DBDelay があれば DB 側で実際に待たせる。
func (r *runner) write(ctx context.Context, batch []Event) {
	if len(batch) == 0 {
		return
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	if r.cfg.DBDelay > 0 {
		// ★遅い DB を模擬する。アプリ側の sleep ではなく DB 側で待たせるので、
		// 接続の占有・プールの詰まりまで含めて再現される
		if _, err := tx.ExecContext(ctx, "SELECT SLEEP(?)", r.cfg.DBDelay.Seconds()); err != nil {
			return
		}
	}

	var sb strings.Builder
	sb.WriteString("INSERT INTO load_item (tenant_id, k, v, seq) VALUES ")
	args := make([]any, 0, len(batch)*4)
	for i, ev := range batch {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?)")
		args = append(args, ev.Tenant, ev.Key, ev.Seq, ev.Seq)
	}
	sb.WriteString(" AS new ON DUPLICATE KEY UPDATE v = new.v, seq = GREATEST(load_item.seq, new.seq)")

	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return
	}
	if err := tx.Commit(); err != nil {
		return
	}
	now := time.Now()
	r.txs.Add(1)
	r.committed.Add(int64(len(batch)))
	for _, ev := range batch {
		r.latency.Record(now.Sub(ev.At))
		if c, ok := r.byTenant[ev.Tenant]; ok {
			c.Add(1)
		}
		if f, ok := r.freshness[ev.Tenant]; ok {
			f.Store(ev.At.UnixNano())
		}
	}
}

// ---- 受け口の実装 ----

type unbounded struct {
	mu     sync.Mutex
	items  []Event
	closed bool
	notify chan struct{}
}

func newUnbounded() *unbounded {
	return &unbounded{notify: make(chan struct{}, 1)}
}

func (u *unbounded) push(ev Event) {
	u.mu.Lock()
	u.items = append(u.items, ev) // ★上限が無い。入ってきたぶんだけ増える
	u.mu.Unlock()
	select {
	case u.notify <- struct{}{}:
	default:
	}
}

func (u *unbounded) pop() (Event, bool) {
	for {
		u.mu.Lock()
		if len(u.items) > 0 {
			ev := u.items[0]
			u.items = u.items[1:]
			u.mu.Unlock()
			return ev, true
		}
		closed := u.closed
		u.mu.Unlock()
		if closed {
			return Event{}, false
		}
		select {
		case <-u.notify:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (u *unbounded) len() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.items)
}

func (u *unbounded) close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	select {
	case u.notify <- struct{}{}:
	default:
	}
}

// coalescer はキーごとに最新だけ残す（調査 §4.3 の重複排除と同じ考え方）。
type coalescer struct {
	mu     sync.Mutex
	items  map[string]Event
	closed chan struct{}
	once   sync.Once
}

func newCoalescer() *coalescer {
	return &coalescer{items: map[string]Event{}, closed: make(chan struct{})}
}

func (c *coalescer) put(ev Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := ev.Tenant + "/" + ev.Key
	_, existed := c.items[k]
	c.items[k] = ev // 新しいほうで置き換える
	return existed
}

func (c *coalescer) drain() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) == 0 {
		return nil
	}
	out := make([]Event, 0, len(c.items))
	for _, ev := range c.items {
		out = append(out, ev)
	}
	clear(c.items)
	return out
}

func (c *coalescer) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *coalescer) close() { c.once.Do(func() { close(c.closed) }) }
