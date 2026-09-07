# EXP-42 hub がテナントごとに分離できているか（A の購読者に B のイベントが漏れない）

| | |
| --- | --- |
| Experiment | EXP-42 / hub-tenant-isolation |
| Starting SHA | `79951f6a8015` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) テナントごとに別 hub＋別 poller。A の版が変わっても A の購読者にだけ流れ、B には流れない。 2) B の版が変わっても B にだけ。値の範囲(A=100番台/B=100万番台)で混線を検出する。 3) 全購読者が抜けたテナントの poller だけが止まる（他テナントに影響しない）。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql= sha=79951f6a8015+dirty |
| Started / Ended | 2026-09-07T11:13:04Z / 2026-09-07T11:13:04Z |

## Results

### テナント分離: A の更新は A だけ・B の更新は B だけ・漏れゼロ — OK

| 数えたもの | 値 |
| --- | --- |
| a_got_updates | 1 |
| active_while | 2 |
| b_got_updates | 1 |
| cross_leaks | 0 |

- A範囲=100番台/B範囲=100万番台。A購読者にB値・B購読者にA値が来た回数=0

### 解放: A 全員が抜けても B の poller は残る（テナント独立） — OK

| 数えたもの | 値 |
| --- | --- |
| active_after_A_released | 1 |
| active_after_all | 0 |

- アクティブ: 購読中 2 → A解放後 1 → 全解放後 0

## Verdict

hub はテナントごとに別 hub＋別 poller で分離する。Broadcast は自テナントの購読者にだけ届き、A の購読者に B のイベントは漏れない（実測で混線ゼロ）。poller の load も自テナントだけ読む。あるテナントの購読者が全員抜けても、その poller だけが止まり他テナントには影響しない。テナントの出所を認証済み ctx から取る（EXP-24/41）ことと合わせて、分離は構造で担保される。

## 適用範囲

- 純 Go（DB 不要）/ A・B 各2購読者 / 値の範囲でテナントを区別（A=100番台, B=100万番台）
- 分離の構造: テナントごとに別 hub（別 map エントリ）＋別 poller。Broadcast は自 hub の購読者だけ
- poller の load はテナント引数で自テナントだけ読む（IN の担当限定と同じ原理・EXP-14）

## 保証しない範囲・未検証

- ここは hub 層の配信分離。テナントの出所（認証済み ctx から取る）は resolver/handler の責務（EXP-24/41）
- singleflight も tenant キーで畳むので跨がない（EXP-38）
- 複数プロセスに分けると各プロセスに hub がある。pub/sub のトピックもテナントで分ける必要（EXP-38）

## 再利用できる成果物

- internal/ssehub: テナントごとに独立した hub＋poller（Registry）
- docs/sse-fan-in.md / docs/security-layers.md: hub のテナント分離

## 次の実験

- なし

