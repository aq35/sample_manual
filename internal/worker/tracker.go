// Package worker は「変化がなければ書かない」ための記憶（§4.2）と、
// バッチ書き込みのためのバッファ（§4.3）を実装する。
//
// この Tracker はワーカー1個ぶんの状態で、テナントごとに1つ持つ。
// DB もネットワークも触らないので、単体テストで挙動を全部確認できる。
package worker

import (
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/aq35/sample_manual/internal/model"
)

// Decision は1件の観測をどう扱ったか。§7 の観測項目そのもの。
type Decision uint8

const (
	// DecisionSkip: 変化なし・近況報告の時期でもない。大半はここ（§4.2）。
	DecisionSkip Decision = iota
	// DecisionChange: 状態が変わったので書く。
	DecisionChange
	// DecisionTouch: 変化はないが、鮮度(observed_at)だけ更新する（§4.2）。
	DecisionTouch
	// DecisionStale: 遅れて届いた古い情報。捨てる（§2.7 をメモリ側でも守る）。
	DecisionStale
)

var decisionNames = [...]string{"skip", "change", "touch", "stale"}

func (d Decision) String() string { return decisionNames[d] }

// Metrics は §7「観測」で毎分出すことになっている値。
// これが無いと変化率が分からず、設計判断の前提が立たない。
type Metrics struct {
	Received uint64
	Changed  uint64
	Touched  uint64
	Skipped  uint64
	Stale    uint64
	Pruned   uint64
	Written  uint64 // 実際に DB へ書いた行数
	Flushes  uint64 // トランザクション回数
}

// ChangeRate は changed / received（§7）。全設計判断の前提になる値。
func (m Metrics) ChangeRate() float64 {
	if m.Received == 0 {
		return 0
	}
	return float64(m.Changed) / float64(m.Received)
}

// entry はメモリに持つ1対象ぶんの記憶（§4.2）。
//
// メモリに置いてよい理由（§3.1）: 全部「消えても起動時の全件取得で作り直せるもの」
// だけで構成されている。欠測が許されない履歴はここに載せない。
type entry struct {
	committed  State     // DB に入っていると分かっている値（差分判定の基準）
	latest     State     // 最後に観測した値（画面・判定に使う。committed とは別物）
	observedAt time.Time // 最後に観測した時刻（この値の観測時点）
	seenAt     time.Time // 最後に受信した時刻（メモリだけ。DB には出さない）
	wroteAt    time.Time // 最後に DB へ書いた時刻
}

// State は比較対象。model.State の別名で、観測時刻を含まないことが要点（§4.2）。
type State = model.State

// Options は Tracker の調整値。
type Options struct {
	// TouchBase / TouchJitter: 近況報告の間隔（§4.2）。
	// 対象ごとにずらさないと、同じ瞬間に全件ぶんの書き込みが立つ。
	TouchBase   time.Duration
	TouchJitter time.Duration

	// StaleAfter は鮮度チェック(B)が「取り直す」と判断する閾値（§2.5）。
	// 定期送信があるなら送信間隔×3、変化時のみなら業務要件から決める。
	StaleAfter time.Duration

	// Now は差し替え可能な時計（テスト用）。
	Now func() time.Time
}

func (o *Options) setDefaults() {
	if o.TouchBase <= 0 {
		o.TouchBase = 30 * time.Second
	}
	if o.TouchJitter < 0 {
		o.TouchJitter = 0
	}
	if o.TouchJitter == 0 {
		o.TouchJitter = 30 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 60 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Row は DB へ渡す1行。
type Row struct {
	ID         model.ID
	State      State
	ObservedAt time.Time
	Source     model.Source
	// TouchOnly が true なら observed_at だけの更新（§4.2 の近況報告）。
	TouchOnly bool
}

// Tracker はワーカー1個ぶんの記憶とバッファ。
type Tracker struct {
	mu  sync.Mutex
	opt Options
	m   map[model.ID]entry
	// pending は ★map であることが本質（§4.3）。
	// 200ms の間に同じ対象が5回変化しても DB に要るのは最後の1件だけで、
	// map に入れておけば自動的に重複排除される。スライスだと効かない。
	pending map[model.ID]Row
	met     Metrics
}

func New(opt Options) *Tracker {
	opt.setDefaults()
	return &Tracker{
		opt:     opt,
		m:       make(map[model.ID]entry),
		pending: make(map[model.ID]Row),
	}
}

// TouchInterval は対象ごとにずらした近況報告の間隔（§4.2）。
func (t *Tracker) TouchInterval(id model.ID) time.Duration {
	if t.opt.TouchJitter <= 0 {
		return t.opt.TouchBase
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return t.opt.TouchBase + time.Duration(uint64(h.Sum32())%uint64(t.opt.TouchJitter))
}

// Observe は1件の観測を受け取り、書くかどうかを決める（§4.2 の onEvent）。
// 書くと決めた場合は pending に積むだけで、DB へは行かない。
func (t *Tracker) Observe(id model.ID, obs model.Observation) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.opt.Now()
	t.met.Received++

	e, known := t.m[id]

	// 遅れて届いた古い情報で新しい状態を上書きしない（§2.7）。
	// DB 側は ON DUPLICATE KEY UPDATE + IF で守っているが、メモリの committed も
	// 同じ規則で守らないと、記憶と DB がずれる。
	if known && !obs.ObservedAt.After(e.observedAt) {
		e.seenAt = now
		t.m[id] = e
		t.met.Stale++
		return DecisionStale
	}

	e.seenAt = now
	e.observedAt = obs.ObservedAt
	e.latest = obs.State

	switch {
	case !known || obs.State != e.committed:
		// 変化した → 書く
		t.m[id] = e
		t.pending[id] = Row{ID: id, State: obs.State, ObservedAt: obs.ObservedAt, Source: obs.Source}
		t.met.Changed++
		return DecisionChange

	case now.Sub(e.wroteAt) > t.TouchInterval(id):
		// 変化なし → たまの近況報告（observed_at だけ）
		t.m[id] = e
		// すでに変化ありで積まれている行を、touch で上書きして弱めない。
		if prev, ok := t.pending[id]; !ok || prev.TouchOnly {
			t.pending[id] = Row{ID: id, State: e.committed, ObservedAt: obs.ObservedAt, Source: obs.Source, TouchOnly: true}
		}
		t.met.Touched++
		return DecisionTouch

	default:
		// 何もしない（大半はここ）
		t.m[id] = e
		t.met.Skipped++
		return DecisionSkip
	}
}

// Drain はバッファを取り出す。★主キー順にソートして返す（§4.3）。
// バッチ同士がすれ違う順序で行を触るとデッドロックになる。
func (t *Tracker) Drain() []Row {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.pending) == 0 {
		return nil
	}
	rows := make([]Row, 0, len(t.pending))
	for _, r := range t.pending {
		rows = append(rows, r)
	}
	clear(t.pending)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// Commit は DB への書き込みが成功したあとにだけ呼ぶ（§4.2）。
// 失敗したら呼ばない = committed が据え置かれ、次のイベントでまた送られる。
func (t *Tracker) Commit(rows []Row, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, r := range rows {
		e := t.m[r.ID]
		if !r.TouchOnly {
			e.committed = r.State
		}
		e.wroteAt = at
		t.m[r.ID] = e
	}
	t.met.Written += uint64(len(rows))
	t.met.Flushes++
}

// Requeue は書き込みに失敗した行を戻す（committed は更新しない）。
func (t *Tracker) Requeue(rows []Row) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range rows {
		if cur, ok := t.pending[r.ID]; ok && cur.ObservedAt.After(r.ObservedAt) {
			continue // すでに新しい観測が積まれている
		}
		t.pending[r.ID] = r
	}
}

// Prune は名簿から消えたキーを削除する（§3.3）。A（全件同期）のたびに呼ぶ。
func (t *Tracker) Prune(roster []model.ID) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	keep := make(map[model.ID]struct{}, len(roster))
	for _, id := range roster {
		keep[id] = struct{}{}
	}
	n := 0
	for id := range t.m {
		if _, ok := keep[id]; !ok {
			delete(t.m, id)
			delete(t.pending, id)
			n++
		}
	}
	t.met.Pruned += uint64(n)
	return n
}

// Stale は鮮度が古くなった対象を返す（B の対象リスト、§2.2）。
// 沈黙を「オフライン」と読み替えないこと。ここで返るのは
// 「API で取り直す対象」であって「停止している対象」ではない（§2.5）。
func (t *Tracker) Stale() []model.ID {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.opt.Now()
	var out []model.ID
	for id, e := range t.m {
		if now.Sub(e.observedAt) > t.opt.StaleAfter {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Liveness は「こちらの観測状態」（§2.4 の3つめ）。
type Liveness uint8

const (
	// LivenessUnobserved: 名簿にあるが、まだ一度も観測できていない。
	LivenessUnobserved Liveness = iota
	// LivenessObserved: 鮮度が新しく、保存値を信じてよい。
	LivenessObserved
	// LivenessUnknown: 鮮度が切れた。オフラインではない。「分からない」。
	LivenessUnknown
)

var livenessNames = [...]string{"unobserved", "observed", "unknown"}

func (l Liveness) String() string { return livenessNames[l] }

// Liveness は保存値ではなく計算で出す（§2.4）。
// 「知らない」を「オフライン」と同じ値にしない。
func (t *Tracker) Liveness(id model.ID) (Liveness, State) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.m[id]
	if !ok {
		return LivenessUnobserved, State{}
	}
	if e.observedAt.IsZero() {
		return LivenessUnobserved, e.latest
	}
	if t.opt.Now().Sub(e.observedAt) > t.opt.StaleAfter {
		// ★ここで online を false に書き換えないこと（§2.5）。
		// 返すのは「最後に見た値」と「それが古い」という事実だけ。
		return LivenessUnknown, e.latest
	}
	return LivenessObserved, e.latest
}

// Metrics は §7 の観測値のスナップショット。
func (t *Tracker) Metrics() Metrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.met
}

// ResetMetrics はカウンタを 0 に戻す（毎分の出力後や、計測の起点で使う）。
func (t *Tracker) ResetMetrics() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.met = Metrics{}
}

// Len は記憶している対象数（§3.3 のサイズ確認用）。
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// Seed は起動時の全件取得（§2.3 の 1〜3）で使う。
// committed をいきなり埋めるのではなく「DB へ書く対象」として積む点に注意:
// 再起動直後の DB の中身は信用できない（§3.2 ①）。
func (t *Tracker) Seed(id model.ID, obs model.Observation) Decision {
	return t.Observe(id, obs)
}
