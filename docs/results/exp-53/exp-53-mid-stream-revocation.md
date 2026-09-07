# EXP-53 接続時だけの認可は失権後も配信し続ける。定期 re-auth なら粒度ぶんで止まる

| | |
| --- | --- |
| Experiment | EXP-53 / mid-stream-revocation |
| Starting SHA | `fd59624975cd` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) 接続時だけ認可（re-auth なし）だと、剥奪後も残り全部を配信してしまう（漏洩）。 2) 定期 re-auth（N event ごと）だと、剥奪から高々 N-1 件で配信が止まる。 3) re-auth を細かくするほど停止は速いが、認可コストが増える（粒度の trade-off）。 4) 剥奪前は両者とも正常に配信する（可用性を壊さない）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=fd59624975cd+dirty |
| Started / Ended | 2026-09-07T12:51:28Z / 2026-09-07T12:51:28Z |

## Results

### 接続時だけ認可（re-auth なし）: 剥奪後も配信し続ける（漏洩） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| delivered | 1000 |
| delivered_after_revoke | 595 |
| stop_latency_events | 595 |

- 剥奪後も 595 件配信（残り全部）。止まらない

### 定期 re-auth（50 ごと）: 剥奪から高々 N-1 件で停止 — OK

| 数えたもの | 値 |
| --- | --- |
| delivered | 450 |
| delivered_after_revoke | 45 |
| stop_latency_events | 45 |

- 剥奪後 45 件で停止（<50）。停止まで 45 件

### re-auth を細かく（10 ごと）: 停止が更に速い（認可コストは増える） — OK

| 数えたもの | 値 |
| --- | --- |
| delivered_after_revoke | 5 |
| stop_latency_events | 5 |

- 剥奪後 5 件で停止（<10）。粒度と認可コストの trade-off

## Verdict

長寿命の subscription は接続時の認可だけでは足りない。権限剥奪後も配信が続き漏洩する（実測: 剥奪後 595 件全部配信）。配信ループで定期 re-auth すれば、剥奪から re-auth 粒度ぶん（<50 件、細かくすれば <10 件）で止まる。剥奪前は両者とも正常配信で可用性は壊さない。即時切断が要るなら失権イベントを push（EXP-43）、最大接続寿命の併用で取りこぼしも塞ぐ。

## 適用範囲

- 純 Go（DB 不要）/ events=1000・revokeAt=400 / re-auth 粒度 50 と 10 / grant 表は EXP-28 相当
- 漏洩 = 剥奪後に配信した件数（理想 0、re-auth 粒度で高々 N-1）
- 実運用の粒度は『event 数』でなく『時間 tick』か『版境界』で持つ（ここは event 数で代表）

## 保証しない範囲・未検証

- 再認可の実体は grant 表の再読 or トークンの有効期限チェック（EXP-13 の期限つき秘密と接続）
- 時間 tick での re-auth は接続数 × 頻度の認可コスト。pub/sub で『失権イベント』を配れば即時に切れる（EXP-43）
- 最大接続寿命（強制再接続）を併用すると、re-auth が漏れても上限で必ず切れる
- 『剥奪 → 即時切断』が要るなら push 型（失権トピック購読）、緩くてよいなら定期 re-auth で足りる

## 再利用できる成果物

- internal/revauthlab: Deliver（接続時のみ vs 定期 re-auth の配信モデル）
- docs/subscription-design.md: 長寿命接続の途中失権

## 次の実験

- （hub 設計の実験はここまで。設計は docs/subscription-design.md へ集約）

