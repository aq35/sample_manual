package orderlab_test

// EXP-47: イベントの順序と冪等消費。
//
//	go test ./internal/orderlab/ -run TestEXP47 -v   （DB 不要）
//
// SSE/キューのイベントは、再送や配送で「重複」「順序入れ替わり」が起きうる。版(version)を持ち、
// 「今より新しい版だけ適用」する単調な消費なら、重複・逆順が来ても最新版に収束する。
// 素朴な last-write-wins（到着順で上書き）は、古いイベントが最後に来ると古い値に退行する。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
)

type event struct {
	key string
	ver int
}

func TestEXP47_順序と冪等消費(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-47", "ordered-idempotent-consume",
		"重複・逆順のイベントでも版で単調適用すれば最新に収束する")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) 版で単調適用（incoming > last のときだけ適用）なら、重複・逆順が来ても各キーは最大版に収束。 " +
			"2) 素朴な last-write-wins（到着順に上書き）は、古い版が最後に来ると古い値に退行する。 " +
			"3) 重複（同じ版）は単調適用では二度目が無視される（冪等）。")

	// 真の最終版: r1=3, r2=6。だが配送は重複・逆順。r1 は古い版1が最後に来る（退行の罠）。
	events := []event{
		{"r1", 2}, {"r2", 6}, {"r1", 3}, {"r1", 3}, {"r2", 5}, {"r1", 1},
	}
	want := map[string]int{"r1": 3, "r2": 6}

	// --- 単調適用（版ガード）---
	guarded := map[string]int{}
	appliedG, ignoredG := 0, 0
	regressionsG := 0
	for _, e := range events {
		cur := guarded[e.key]
		if e.ver > cur {
			guarded[e.key] = e.ver
			appliedG++
		} else {
			ignoredG++
			if e.ver < cur {
				// 退行しかけたが無視した（ガードが効いた回数）
			}
		}
	}
	// 退行チェック: guarded は want と一致するはず（退行なし）
	for k, w := range want {
		if guarded[k] != w {
			regressionsG++
		}
	}

	// --- 素朴 last-write-wins（到着順に上書き）---
	naive := map[string]int{}
	for _, e := range events {
		naive[e.key] = e.ver // 到着順に上書き → 最後の到着が残る
	}
	naiveWrong := 0
	for k, w := range want {
		if naive[k] != w {
			naiveWrong++
		}
	}

	rec.Add(expkit.Variant{
		Name:     "版で単調適用: 重複・逆順でも最新に収束（退行なし）",
		Counters: map[string]int64{"applied": int64(appliedG), "ignored": int64(ignoredG), "r1": int64(guarded["r1"]), "r2": int64(guarded["r2"]), "wrong_keys": int64(regressionsG)},
		Notes:    []string{"最終 r1=" + itoa(guarded["r1"]) + " r2=" + itoa(guarded["r2"]) + "（想定 r1=3,r2=6）。重複/逆順は無視 " + itoa(ignoredG) + " 回"},
	})
	rec.Add(expkit.Variant{
		Name:     "素朴 last-write-wins: 古い版が最後に来て退行",
		Accident: true,
		Counters: map[string]int64{"r1": int64(naive["r1"]), "r2": int64(naive["r2"]), "wrong_keys": int64(naiveWrong)},
		Notes:    []string{"最終 r1=" + itoa(naive["r1"]) + "（版1が最後に到着し退行）・想定 3。誤り " + itoa(naiveWrong) + " キー"},
	})
	t.Logf("guarded: r1=%d r2=%d applied=%d ignored=%d / naive: r1=%d r2=%d wrong=%d",
		guarded["r1"], guarded["r2"], appliedG, ignoredG, naive["r1"], naive["r2"], naiveWrong)

	// ---- 検証 ----
	if guarded["r1"] != want["r1"] || guarded["r2"] != want["r2"] {
		t.Errorf("単調適用が最新に収束していない: r1=%d r2=%d", guarded["r1"], guarded["r2"])
	}
	if regressionsG != 0 {
		t.Errorf("単調適用で退行した: %d キー", regressionsG)
	}
	if appliedG != 3 { // r1: 2,3 の2回 + r2: 6 の1回 = 3（重複3と逆順1と5は無視）
		t.Errorf("適用回数が想定と違う: %d（3 のはず）", appliedG)
	}
	if naiveWrong == 0 {
		t.Errorf("素朴 last-write-wins が退行していない（するはず）")
	}

	rec.Scope(
		"純 Go（DB 不要）/ 配送順 [r1:2, r2:6, r1:3, r1:3(dup), r2:5, r1:1] / 想定最終 r1=3,r2=6",
		"単調適用 = incoming version > last のときだけ適用（それ以外は無視＝冪等・逆順耐性）",
		"版は fence（EXP-30）・SSE の版配信（EXP-38/41）と同じ考え",
	)
	rec.Uncertain(
		"版はキー単位に単調増加する前提（サーバが採番）。版が無い/回る場合は別途（時刻＋タイブレーク等）",
		"差分でなく『版つきスナップショット/最新値』を配る設計だと、逆順・欠落に強い（落としても次で収束）",
		"部分更新（フィールドごとに版）が要るなら、フィールド単位の版か CRDT を検討（本実験外）",
	)
	rec.Artifact("docs/event-ordering.md: 順序・重複に強い冪等消費（版で単調適用）")
	rec.Next("EXP-48 保持期間の運用")

	files, err := rec.Save(
		"イベントは重複・逆順で届きうる。キーごとに版を持ち『今より新しい版だけ適用』する単調消費に" +
			"すれば、重複・逆順が来ても最新版に収束し退行しない（冪等）。素朴な到着順上書きは、古い版が" +
			"最後に来ると退行する。差分でなく版つきの最新値/スナップショットを配ると更に堅い。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
