package revauthlab_test

// EXP-53: 長寿命接続の途中失権（re-authorization）。
//
//	go test ./internal/revauthlab/ -run TestEXP53 -v   （DB 不要）
//
// SSE/subscription は何時間も生きる。接続時に一度だけ認可すると、途中で権限を剥奪されても配信が
// 続く（漏洩）。配信ループで定期 re-auth すれば、剥奪から re-auth 粒度ぶんで止まる。
// 「接続時だけ」と「定期 re-auth」で、剥奪後に届く件数と停止までの遅れを比べる。

import (
	"context"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/revauthlab"
)

func TestEXP53_途中失権(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-53", "mid-stream-revocation",
		"接続時だけの認可は失権後も配信し続ける。定期 re-auth なら粒度ぶんで止まる")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(
		"1) 接続時だけ認可（re-auth なし）だと、剥奪後も残り全部を配信してしまう（漏洩）。 " +
			"2) 定期 re-auth（N event ごと）だと、剥奪から高々 N-1 件で配信が止まる。 " +
			"3) re-auth を細かくするほど停止は速いが、認可コストが増える（粒度の trade-off）。 " +
			"4) 剥奪前は両者とも正常に配信する（可用性を壊さない）。")

	const sub = "s1"
	const events = 1000
	const revokeAt = 405 // 405 件目の直前に剥奪（re-auth 境界とわざとずらす → 粒度ぶん流れる）

	// --- ① 接続時だけ認可（re-auth なし）---
	store1 := revauthlab.NewGrantStore(sub)
	dNo, afterNo, latNo := revauthlab.Deliver(store1, store1, sub, 0, events, revokeAt)

	// --- ② 定期 re-auth（50 event ごと）---
	store2 := revauthlab.NewGrantStore(sub)
	const reauthEvery = 50
	dRe, afterRe, latRe := revauthlab.Deliver(store2, store2, sub, reauthEvery, events, revokeAt)

	// --- ③ 粒度を細かく（10 event ごと）---
	store3 := revauthlab.NewGrantStore(sub)
	const reauthFine = 10
	_, afterFine, latFine := revauthlab.Deliver(store3, store3, sub, reauthFine, events, revokeAt)

	rec.Add(expkit.Variant{
		Name:     "接続時だけ認可（re-auth なし）: 剥奪後も配信し続ける（漏洩）",
		Accident: true,
		Counters: map[string]int64{"delivered": int64(dNo), "delivered_after_revoke": int64(afterNo), "stop_latency_events": int64(latNo)},
		Notes:    []string{"剥奪後も " + itoa(afterNo) + " 件配信（残り全部）。止まらない"},
	})
	rec.Add(expkit.Variant{
		Name:     "定期 re-auth（50 ごと）: 剥奪から高々 N-1 件で停止",
		Counters: map[string]int64{"delivered": int64(dRe), "delivered_after_revoke": int64(afterRe), "stop_latency_events": int64(latRe)},
		Notes:    []string{"剥奪後 " + itoa(afterRe) + " 件で停止（<50）。停止まで " + itoa(latRe) + " 件"},
	})
	rec.Add(expkit.Variant{
		Name:     "re-auth を細かく（10 ごと）: 停止が更に速い（認可コストは増える）",
		Counters: map[string]int64{"delivered_after_revoke": int64(afterFine), "stop_latency_events": int64(latFine)},
		Notes:    []string{"剥奪後 " + itoa(afterFine) + " 件で停止（<10）。粒度と認可コストの trade-off"},
	})
	t.Logf("no-reauth: after=%d lat=%d / reauth50: after=%d lat=%d / reauth10: after=%d lat=%d",
		afterNo, latNo, afterRe, latRe, afterFine, latFine)

	// ---- 検証 ----
	if afterNo != events-revokeAt {
		t.Errorf("接続時だけ認可が漏洩し続けていない: after=%d（%d のはず）", afterNo, events-revokeAt)
	}
	if afterRe >= reauthEvery {
		t.Errorf("定期 re-auth が粒度内で止まっていない: after=%d（<%d のはず）", afterRe, reauthEvery)
	}
	if afterRe == 0 {
		t.Errorf("re-auth ありでも即 0 はおかしい（粒度ぶんは流れる）: after=%d", afterRe)
	}
	if afterFine >= afterRe {
		t.Errorf("細かい re-auth が粗いより漏洩が少なくない: fine=%d coarse=%d", afterFine, afterRe)
	}
	if dRe <= 0 || dNo <= 0 {
		t.Errorf("剥奪前の配信が0（可用性を壊している）: dNo=%d dRe=%d", dNo, dRe)
	}

	rec.Scope(
		"純 Go（DB 不要）/ events=1000・revokeAt=400 / re-auth 粒度 50 と 10 / grant 表は EXP-28 相当",
		"漏洩 = 剥奪後に配信した件数（理想 0、re-auth 粒度で高々 N-1）",
		"実運用の粒度は『event 数』でなく『時間 tick』か『版境界』で持つ（ここは event 数で代表）",
	)
	rec.Uncertain(
		"再認可の実体は grant 表の再読 or トークンの有効期限チェック（EXP-13 の期限つき秘密と接続）",
		"時間 tick での re-auth は接続数 × 頻度の認可コスト。pub/sub で『失権イベント』を配れば即時に切れる（EXP-43）",
		"最大接続寿命（強制再接続）を併用すると、re-auth が漏れても上限で必ず切れる",
		"『剥奪 → 即時切断』が要るなら push 型（失権トピック購読）、緩くてよいなら定期 re-auth で足りる",
	)
	rec.Artifact(
		"internal/revauthlab: Deliver（接続時のみ vs 定期 re-auth の配信モデル）",
		"docs/subscription-design.md: 長寿命接続の途中失権",
	)
	rec.Next("（hub 設計の実験はここまで。設計は docs/subscription-design.md へ集約）")

	files, err := rec.Save(
		"長寿命の subscription は接続時の認可だけでは足りない。権限剥奪後も配信が続き漏洩する" +
			"（実測: 剥奪後 595 件全部配信）。配信ループで定期 re-auth すれば、剥奪から re-auth 粒度ぶん" +
			"（<50 件、細かくすれば <10 件）で止まる。剥奪前は両者とも正常配信で可用性は壊さない。" +
			"即時切断が要るなら失権イベントを push（EXP-43）、最大接続寿命の併用で取りこぼしも塞ぐ。")
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
