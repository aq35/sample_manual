package worker_test

// §7 チェックリストのうち「メモリ」「DB 負荷」に関する項目を、実行可能なテストにしたもの。
// DB もネットワークも要らないので、他プロジェクトへ持って行きやすいのはこの層。

import (
	"reflect"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/model"
	"github.com/aq35/sample_manual/internal/worker"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time      { return c.now }
func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }

func newTracker(c *clock) *worker.Tracker {
	return worker.New(worker.Options{
		TouchBase:   30 * time.Second,
		TouchJitter: 30 * time.Second,
		StaleAfter:  60 * time.Second,
		Now:         c.Now,
	})
}

func obs(st model.Status, online bool, batt int8, at time.Time) model.Observation {
	return model.Observation{
		State:      model.State{Status: st, Online: online, Battery: batt},
		ObservedAt: at,
		Source:     model.SourceWS,
	}
}

// §4.2「変化がないときに書いていないか」
func TestObserve_同じ状態は書かない(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)

	if got := tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now)); got != worker.DecisionChange {
		t.Fatalf("初回は change のはず: %v", got)
	}
	rows := tr.Drain()
	tr.Commit(rows, c.now)

	// 同じ状態が10回流れてきても、DB へは行かない
	for i := 0; i < 10; i++ {
		c.add(time.Second)
		if got := tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now)); got != worker.DecisionSkip {
			t.Fatalf("%d回目: skip のはず: %v", i, got)
		}
	}
	if rows := tr.Drain(); len(rows) != 0 {
		t.Fatalf("書き込みが積まれている: %+v", rows)
	}
	m := tr.Metrics()
	if m.Received != 11 || m.Changed != 1 || m.Skipped != 10 {
		t.Fatalf("メトリクスが合わない: %+v", m)
	}
	if got := m.ChangeRate(); got > 0.1 {
		t.Fatalf("変化率が高すぎる: %v", got)
	}
}

// §4.2「鮮度(observed_at)を毎回 DB に書いていないか」＝ たまの近況報告になっているか
func TestObserve_近況報告は間隔をあけて1回だけ(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)
	tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now))
	tr.Commit(tr.Drain(), c.now)

	interval := tr.TouchInterval("r001")
	if interval < 30*time.Second || interval >= 60*time.Second {
		t.Fatalf("近況報告の間隔が範囲外: %v", interval)
	}

	touches := 0
	for i := 0; i < 120; i++ { // 120秒ぶん、毎秒同じ状態が届く
		c.add(time.Second)
		if tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now)) == worker.DecisionTouch {
			touches++
			tr.Commit(tr.Drain(), c.now)
		}
	}
	if touches == 0 || touches > 4 {
		t.Fatalf("120秒で touch が %d 回。毎回書くのも書かないのも間違い", touches)
	}
	t.Logf("120秒・毎秒受信 → DB 書き込みは %d 回（間隔 %v）", touches, interval)
}

// §4.2「必ず踏む罠」: 対象ごとに間隔をずらしているか
func TestTouchInterval_対象ごとにずれている(t *testing.T) {
	c := &clock{now: time.Now()}
	tr := newTracker(c)
	seen := map[time.Duration]int{}
	for i := 0; i < 1000; i++ {
		id := model.ID("r" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
		seen[tr.TouchInterval(id)]++
	}
	if len(seen) < 20 {
		t.Fatalf("間隔が %d 種類しかない。デプロイ時に山が立つ（§3.4）", len(seen))
	}
}

// §2.7「遅れて届いた古い情報で、新しい状態を上書きしない」（メモリ側）
func TestObserve_古い観測は捨てる(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)
	newer := c.now
	older := c.now.Add(-10 * time.Second)

	tr.Observe("r001", obs(model.StatusRunning, true, 80, newer))
	tr.Commit(tr.Drain(), c.now)

	if got := tr.Observe("r001", obs(model.StatusStopped, false, 10, older)); got != worker.DecisionStale {
		t.Fatalf("古い観測は stale のはず: %v", got)
	}
	if _, st := tr.Liveness("r001"); st.Status != model.StatusRunning {
		t.Fatalf("古い情報で上書きされた: %v", st.Status)
	}
}

// §4.2「committed の更新が、DB 書き込み成功後になっているか」
func TestCommit_書き込み失敗時は据え置く(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)

	tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now))
	rows := tr.Drain()

	// 書き込みに失敗した → Commit を呼ばず Requeue する
	tr.Requeue(rows)

	c.add(time.Second)
	// 次のイベントでまた「変化あり」と判定されること（=取りこぼさない）
	if got := tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now.Add(time.Second))); got != worker.DecisionChange {
		t.Fatalf("失敗した書き込みが消えている: %v", got)
	}
	if len(tr.Drain()) != 1 {
		t.Fatal("再送されていない")
	}
}

// §4.3「バッファがキー付き(map)か」— 配列だと重複排除が効かない
func TestDrain_重複排除と主キー順ソート(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)

	// 200ms の間に同じ対象が5回変化した想定
	for i := 0; i < 5; i++ {
		c.add(40 * time.Millisecond)
		tr.Observe("r005", obs(model.Status(1+i%3), true, int8(80-i), c.now))
		tr.Observe("r002", obs(model.Status(1+i%3), true, int8(50-i), c.now))
	}
	rows := tr.Drain()
	if len(rows) != 2 {
		t.Fatalf("重複排除が効いていない: %d 行", len(rows))
	}
	if rows[0].ID != "r002" || rows[1].ID != "r005" {
		t.Fatalf("主キー順にソートされていない（デッドロックの原因、§4.3）: %v %v", rows[0].ID, rows[1].ID)
	}
}

// §3.3「名簿から消えたキーを削除しているか」
func TestPrune_名簿から消えた対象を捨てる(t *testing.T) {
	c := &clock{now: time.Now()}
	tr := newTracker(c)
	for _, id := range []model.ID{"r001", "r002", "r003"} {
		tr.Observe(id, obs(model.StatusRunning, true, 80, c.now))
	}
	if n := tr.Prune([]model.ID{"r001", "r003"}); n != 1 {
		t.Fatalf("削除数が違う: %d", n)
	}
	if tr.Len() != 2 {
		t.Fatalf("記憶が残っている: %d", tr.Len())
	}
}

// §2.4「『不明』を、オフラインとは別の値として持っているか」
// §2.5「オフライン判定を、沈黙から行っていないか」
func TestLiveness_沈黙をオフラインにしない(t *testing.T) {
	c := &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(c)

	if lv, _ := tr.Liveness("r404"); lv != worker.LivenessUnobserved {
		t.Fatalf("知らない対象は unobserved のはず: %v", lv)
	}

	tr.Observe("r001", obs(model.StatusRunning, true, 80, c.now))
	if lv, st := tr.Liveness("r001"); lv != worker.LivenessObserved || !st.Online {
		t.Fatalf("観測直後は observed のはず: %v", lv)
	}

	c.add(90 * time.Second) // 沈黙した
	lv, st := tr.Liveness("r001")
	if lv != worker.LivenessUnknown {
		t.Fatalf("鮮度切れは unknown のはず: %v", lv)
	}
	// ★ここが要点: 沈黙しても online は false に「されていない」。
	// オフラインかどうかは API に聞くまで分からない（§2.5）。
	if !st.Online {
		t.Fatal("沈黙を根拠に online を false にしている（§2.5 違反）")
	}
	if ids := tr.Stale(); len(ids) != 1 || ids[0] != "r001" {
		t.Fatalf("B（鮮度チェック）の対象に入っていない: %v", ids)
	}
}

// §4.2「必ず踏む罠」その1 を、機械的に検出するテスト。
//
// 比較対象の struct に time.Time を入れると、毎回値が違うので 100% 変化ありと
// 判定され、仕組みが丸ごと無効になる。しかも動いてはいるので気づかない。
// ★他プロジェクトへ持って行くならこのテストが一番効く。
func TestStateに時刻やポインタを入れていない(t *testing.T) {
	rt := reflect.TypeOf(model.State{})
	if !rt.Comparable() {
		t.Fatal("State が比較可能でない。== で差分判定できない")
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		switch f.Type.Kind() {
		case reflect.Struct:
			if f.Type == reflect.TypeOf(time.Time{}) {
				t.Errorf("State.%s が time.Time。毎回変化ありになる（§4.2 の罠）", f.Name)
			}
		case reflect.Float32, reflect.Float64:
			t.Errorf("State.%s が浮動小数。丸めた整数で持つこと（§4.2 の罠）", f.Name)
		case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			t.Errorf("State.%s が %s。参照型は == で中身を比較しない", f.Name, f.Type.Kind())
		}
	}
	t.Logf("State のサイズ: %d バイト（§3.3 の見積もりの根拠）", rt.Size())
}

// §4.2「浮動小数を丸めてから比較しているか」
func TestRoundBattery_丸めてから比較する(t *testing.T) {
	// 生の値は毎回わずかに違う
	raw := []float64{80.1, 80.4, 79.8, 80.49}
	first := model.RoundBattery(raw[0], 5)
	for _, v := range raw[1:] {
		if got := model.RoundBattery(v, 5); got != first {
			t.Fatalf("丸めが効いていない: %v → %d (期待 %d)", v, got, first)
		}
	}
	if got := model.RoundBattery(87.5, 5); got != 90 {
		t.Fatalf("87.5 を刻み5で丸めると 90: got %d", got)
	}
	if got := model.RoundBattery(120, 5); got != 100 {
		t.Fatalf("上限で頭打ちにする: got %d", got)
	}
}

// §6.3「打ち間違いがコンパイルエラーになる」ことの裏返し:
// API から来た知らない文字列は unknown にして処理を止めない。
func TestParseStatus_知らない値でも止まらない(t *testing.T) {
	if st, ok := model.ParseStatus("runing"); ok || st != model.StatusUnknown {
		t.Fatalf("打ち間違いは unknown 扱い: %v %v", st, ok)
	}
	if st, ok := model.ParseStatus("running"); !ok || st != model.StatusRunning {
		t.Fatalf("正しい値が通らない: %v %v", st, ok)
	}
}
