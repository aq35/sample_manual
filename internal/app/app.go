// Package app は §2 の「3本のループ」を実際に動かすワーカー本体。
//
//	A. 全件同期  起動時 ＋ 5〜15分ごと   名簿の変化と値のズレを直す（正しさ）
//	B. 鮮度チェック 30〜60秒ごと          古いものだけ API で取り直す
//	C. 受信      常時                    WebSocket を受けて更新する（速さ）
//
// 起動順序（§2.3）と、接続の生死判定（§2.6）もここに入っている。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aq35/sample_manual/internal/jsonx"
	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/store"
	"github.com/aq35/sample_manual/internal/worker"
)

// Config はワーカー1台ぶんの設定。既定値は §2.2 の表に合わせてある。
type Config struct {
	Tenant model.TenantID
	APIURL string // 例: http://svc/robots
	WSURL  string // 例: ws://svc/ws

	FullSyncEvery  time.Duration // A: 5〜15分
	FreshnessEvery time.Duration // B: 30〜60秒
	StaleAfter     time.Duration // 鮮度切れとみなす時間
	BatchEvery     time.Duration // 書き出し間隔（§4.3）
	TouchBase      time.Duration // 近況報告（§4.2）
	TouchJitter    time.Duration

	PingEvery   time.Duration // §2.6 アプリ層の Ping
	PongWait    time.Duration // Pong が来なければ切って繋ぎ直す
	MaxNoPong   int
	BatteryStep int // 比較前に丸める刻み（§4.2）

	// StartupJitter: 起動時の全件取得をテナントごとにずらす（§3.4）。
	// デプロイで全コンテナが同時に起動すると、API に山が立つ。
	StartupJitter time.Duration

	Logger *slog.Logger
}

func (c *Config) setDefaults() {
	if c.FullSyncEvery == 0 {
		c.FullSyncEvery = 10 * time.Minute
	}
	if c.FreshnessEvery == 0 {
		c.FreshnessEvery = 45 * time.Second
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = 60 * time.Second
	}
	if c.BatchEvery == 0 {
		c.BatchEvery = 200 * time.Millisecond
	}
	if c.TouchBase == 0 {
		c.TouchBase = 30 * time.Second
	}
	if c.TouchJitter == 0 {
		c.TouchJitter = 30 * time.Second
	}
	if c.PingEvery == 0 {
		c.PingEvery = 15 * time.Second
	}
	if c.PongWait == 0 {
		c.PongWait = 10 * time.Second
	}
	if c.MaxNoPong == 0 {
		c.MaxNoPong = 2
	}
	if c.BatteryStep == 0 {
		c.BatteryStep = 5
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// App はワーカー1台。テナントごとに1つ作るが、★Store（接続プール）は共有する（§5.1）。
type App struct {
	cfg   Config
	tr    *worker.Tracker
	st    *store.Store
	httpc *http.Client

	ready       atomic.Bool
	lastRecvNs  atomic.Int64 // 最後に受信した時刻（§7 の監視項目）
	reconnects  atomic.Int64
	fullSyncs   atomic.Int64
	freshChecks atomic.Int64
	apiFailures atomic.Int64

	unknownFields jsonx.UnknownFieldRecorder
}

func New(cfg Config, st *store.Store) *App {
	cfg.setDefaults()
	return &App{
		cfg: cfg,
		st:  st,
		tr: worker.New(worker.Options{
			TouchBase:   cfg.TouchBase,
			TouchJitter: cfg.TouchJitter,
			StaleAfter:  cfg.StaleAfter,
		}),
		httpc: &http.Client{Timeout: 30 * time.Second},
	}
}

// Tracker は検査用（テストと画面配信）。
func (a *App) Tracker() *worker.Tracker { return a.tr }

// Ready は §2.3 の「準備完了の宣言」。★全件取得が終わるまで true にしない。
// 先に true にすると、全件「不明」の画面が一瞬出る。
func (a *App) Ready() bool { return a.ready.Load() }

// LastReceived は最後に WebSocket からデータが届いた時刻（§7）。
// 監視は「接続の有無」ではなくこれを見る（§2.6）。
func (a *App) LastReceived() time.Time {
	ns := a.lastRecvNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (a *App) Reconnects() int64  { return a.reconnects.Load() }
func (a *App) FullSyncs() int64   { return a.fullSyncs.Load() }
func (a *App) FreshChecks() int64 { return a.freshChecks.Load() }
func (a *App) APIFailures() int64 { return a.apiFailures.Load() }

// Run は §2.3 の起動順序どおりに立ち上げ、3本のループを回す。
func (a *App) Run(ctx context.Context) error {
	// テナントごとに起動をずらす（§3.4）。デプロイ時に API へ山が立つのを防ぐ。
	if d := a.startupDelay(); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// 1〜3. 名簿を取り、全状態を埋める
	if err := a.FullSync(ctx); err != nil {
		return fmt.Errorf("起動時の全件同期に失敗: %w", err)
	}
	if err := a.Flush(ctx); err != nil {
		return fmt.Errorf("起動時の書き込みに失敗: %w", err)
	}
	// 4. ★ここで準備完了（3 より前に置かない）
	a.ready.Store(true)
	a.cfg.Logger.Info("準備完了", "tenant", a.cfg.Tenant, "対象数", a.tr.Len())

	// 5. 受信開始
	errc := make(chan error, 4)
	go func() { errc <- a.loopFullSync(ctx) }()  // A
	go func() { errc <- a.loopFreshness(ctx) }() // B
	go func() { errc <- a.loopReceive(ctx) }()   // C
	go func() { errc <- a.loopFlush(ctx) }()     // 書き出し

	err := <-errc
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (a *App) startupDelay() time.Duration {
	if a.cfg.StartupJitter <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(a.cfg.Tenant))
	return time.Duration(uint64(h.Sum32()) % uint64(a.cfg.StartupJitter))
}

// ---- A. 全件同期（§2.2）----

// FullSync は API から名簿＋全状態を取得する。
//
// ★これは省略できない。B（鮮度チェック）だけでは
//   - 名簿の変化（新規追加は B の対象リストにそもそも載らない）
//   - 間違った値（誤っていても「新しい」ので B の対象にならない）
//
// を拾えない。
func (a *App) FullSync(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.APIURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		a.apiFailures.Add(1)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		a.apiFailures.Add(1)
		return fmt.Errorf("全件同期: HTTP %d", resp.StatusCode)
	}

	// ★大きい配列は1件ずつ読む（§6.2）。全件のスライスを作らない。
	roster := make([]model.ID, 0, a.tr.Len()+16)
	err = jsonx.StreamRobots(resp.Body, func(r jsonx.SlimRobot) error {
		id, obs, err := r.ToObservation(model.SourceAPI, a.cfg.BatteryStep)
		if err != nil {
			// 1件の書式ずれで全件を落とさない（§6.3）
			a.cfg.Logger.Warn("読めない1件を飛ばした", "id", r.ID, "err", err)
			return nil
		}
		roster = append(roster, id)
		a.tr.Observe(id, obs)
		return nil
	})
	if err != nil {
		return err
	}

	// 名簿から消えたキーを掃除する（§3.3）
	if n := a.tr.Prune(roster); n > 0 {
		a.cfg.Logger.Info("名簿から消えた対象を削除", "件数", n)
	}
	a.fullSyncs.Add(1)
	return nil
}

func (a *App) loopFullSync(ctx context.Context) error {
	t := time.NewTicker(a.cfg.FullSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := a.FullSync(ctx); err != nil {
				// 失敗しても止めない。次の周期で取り直す。
				a.cfg.Logger.Warn("全件同期に失敗", "err", err)
			}
		}
	}
}

// ---- B. 鮮度チェック（§2.2）----

// FreshnessCheck は鮮度が切れた対象だけを API に問い合わせる。
//
// ★沈黙を根拠にオフラインと判定しない（§2.5）。判定できるのは API だけ。
// API も答えないなら「不明」のままにする（ここで嘘をつかない、§2.4）。
func (a *App) FreshnessCheck(ctx context.Context) error {
	stale := a.tr.Stale()
	if len(stale) == 0 {
		return nil
	}
	u, err := url.Parse(a.cfg.APIURL)
	if err != nil {
		return err
	}
	q := u.Query()
	for _, id := range stale {
		q.Add("ids", string(id))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		a.apiFailures.Add(1)
		return err // ★状態は書き換えない。「不明」のまま
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		a.apiFailures.Add(1)
		return fmt.Errorf("鮮度チェック: HTTP %d", resp.StatusCode)
	}

	// 数件〜数十件なので、まとめて Decode で十分（§6.2 の使い分け）
	var out jsonx.SlimResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	for _, r := range out.Robots {
		id, obs, err := r.ToObservation(model.SourceAPI, a.cfg.BatteryStep)
		if err != nil {
			continue
		}
		a.tr.Observe(id, obs)
	}
	a.freshChecks.Add(1)
	return nil
}

func (a *App) loopFreshness(ctx context.Context) error {
	t := time.NewTicker(a.cfg.FreshnessEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := a.FreshnessCheck(ctx); err != nil {
				a.cfg.Logger.Warn("鮮度チェックに失敗（状態は不明のまま据え置く）", "err", err)
			}
		}
	}
}

// ---- C. 受信（§2.6）----

func (a *App) loopReceive(ctx context.Context) error {
	backoff := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.receiveOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.reconnects.Add(1)
		a.cfg.Logger.Warn("WebSocket を張り直す", "err", err, "待ち", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// receiveOnce は1本の接続を、切れるまで読み続ける。
//
// §2.6 の3点を実装している。
//   - Ping/Pong を有効にする（TCP keepalive では足りない）
//   - Pong が返らなければ、自分から切って繋ぎ直す
//   - 読み取りに期限を切る（無音で永久に待たない）
func (a *App) receiveOnce(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	c, _, err := dialer.DialContext(ctx, a.cfg.WSURL, nil)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	deadline := a.cfg.PingEvery*time.Duration(a.cfg.MaxNoPong) + a.cfg.PongWait
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	c.SetPongHandler(func(string) error {
		// Pong が来たら期限を延ばす。来なければ期限切れで読み取りが失敗する。
		return c.SetReadDeadline(time.Now().Add(deadline))
	})

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(a.cfg.PingEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = c.SetWriteDeadline(time.Now().Add(a.cfg.PongWait))
				if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
					_ = c.Close()
					return
				}
			case <-stop:
				return
			case <-ctx.Done():
				_ = c.Close()
				return
			}
		}
	}()

	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return err // 期限切れ・切断・Pong 無応答はすべてここに来る
		}
		a.lastRecvNs.Store(time.Now().UnixNano())

		// WebSocket は1件ずつ届くので、そのまま Unmarshal でよい（§6.2）
		var r jsonx.SlimRobot
		if err := json.Unmarshal(data, &r); err != nil {
			a.cfg.Logger.Warn("読めないメッセージを飛ばした", "err", err)
			continue
		}
		// 知らない項目は起動時に1回だけ記録する。処理は止めない（§6.3）
		if unknown := a.unknownFields.Inspect(data, jsonx.SlimKnownFields()); len(unknown) > 0 {
			a.cfg.Logger.Info("API に知らない項目がある（処理は続ける）", "項目", strings.Join(unknown, ","))
		}
		id, obs, err := r.ToObservation(model.SourceWS, a.cfg.BatteryStep)
		if err != nil {
			continue
		}
		a.tr.Observe(id, obs)
	}
}

// ---- 書き出し（§4.3）----

// Flush はバッファを1トランザクションで書く。
// ★成功したときだけ Commit する（§4.2）。失敗したら committed を据え置いて再送する。
func (a *App) Flush(ctx context.Context) error {
	rows := a.tr.Drain()
	if len(rows) == 0 {
		return nil
	}
	if _, err := a.st.UpsertBatch(ctx, a.cfg.Tenant, rows); err != nil {
		a.tr.Requeue(rows)
		return err
	}
	a.tr.Commit(rows, time.Now())
	return nil
}

func (a *App) loopFlush(ctx context.Context) error {
	t := time.NewTicker(a.cfg.BatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = a.Flush(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-t.C:
			if err := a.Flush(ctx); err != nil {
				a.cfg.Logger.Warn("書き込みに失敗（次の周期で再送する）", "err", err)
			}
		}
	}
}

// ---- 観測（§7）----

// Status は毎分ログに出す値。変化率が見えることが要点。
type Status struct {
	Tenant       model.TenantID
	Ready        bool
	Tracked      int
	LastReceived time.Time
	Metrics      worker.Metrics
	Reconnects   int64
	FullSyncs    int64
	FreshChecks  int64
	APIFailures  int64
}

func (a *App) Status() Status {
	return Status{
		Tenant:       a.cfg.Tenant,
		Ready:        a.Ready(),
		Tracked:      a.tr.Len(),
		LastReceived: a.LastReceived(),
		Metrics:      a.tr.Metrics(),
		Reconnects:   a.Reconnects(),
		FullSyncs:    a.FullSyncs(),
		FreshChecks:  a.FreshChecks(),
		APIFailures:  a.APIFailures(),
	}
}

func (s Status) String() string {
	last := "まだ受信なし"
	if !s.LastReceived.IsZero() {
		last = fmt.Sprintf("%.1f秒前", time.Since(s.LastReceived).Seconds())
	}
	return fmt.Sprintf(
		"[%s] 準備完了=%v 対象=%d 最終受信=%s | 受信=%d 変化=%d 近況=%d 無視=%d 変化率=%.2f%% | 書込=%d行/%d回 再接続=%d 全件同期=%d 鮮度=%d API失敗=%d",
		s.Tenant, s.Ready, s.Tracked, last,
		s.Metrics.Received, s.Metrics.Changed, s.Metrics.Touched, s.Metrics.Skipped, s.Metrics.ChangeRate()*100,
		s.Metrics.Written, s.Metrics.Flushes, s.Reconnects, s.FullSyncs, s.FreshChecks, s.APIFailures)
}
