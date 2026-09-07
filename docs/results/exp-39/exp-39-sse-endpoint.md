# EXP-39 動く SSE エンドポイント: 1つの poller が引いて全接続へ配る（1コンテナ内）

| | |
| --- | --- |
| Experiment | EXP-39 / sse-endpoint |
| Starting SHA | `b6c96f6391b9` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) N 本の SSE 接続でも、テナントの poller は interval ごとに1回だけ DB を引く（接続数に依らない）。 2) 版が変わると全接続が受け取る。 3) 全接続が切れると poller は止まり、レジストリは空になる（goroutine/DB を漏らさない）。 4) これは1コンテナ（1プロセス）内で成立する。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=b6c96f6391b9+dirty |
| Started / Ended | 2026-09-07T10:37:18Z / 2026-09-07T10:37:19Z |

## Results

### N=12 接続・版を5回変更: 全接続が受信、DB 読みは poller の分だけ — OK

| 数えたもの | 値 |
| --- | --- |
| active_tenants | 1 |
| clients | 12 |
| db_reads_connected | 20 |
| min_events_per_client | 6 |

- 素朴なら 接続数×tick で数百回。poller は1本ぶん 20 回。各接続 最低 6 受信

### 全切断後: poller 停止・レジストリ空・DB 読みが止まる — OK

| 数えたもの | 値 |
| --- | --- |
| active_after | 0 |
| reads_120ms_later | 20 |
| reads_at_disconnect | 20 |

- active_tenants 1 → 0 / reads は 20 から増えない(20)

## Verdict

SSE は『テナントに1つの poller が DB を引き、hub で全接続へ配る』を1コンテナ内で実現できる。接続がいくら増えても DB 読みは poller の分だけ。切断は req.Context().Done() で検知し参照カウントでpoller を止める（漏らさない）。接続数がメモリ/fd 上限を超えるか隔離したいときだけ、SSE 層を分けてpub/sub で繋ぐ（EXP-38）。

## 適用範囲

- MySQL 8.0 / httptest の実 SSE 接続 12 本 / poller interval 30ms / バッファ 64
- DB 読みは Registry が数える（poller の load 呼び出し）。1コンテナ・1プロセス内
- 版変更は subq を5回 Bump。各 tick で load し変化時だけ配る（coalesce）

## 保証しない範囲・未検証

- タイミング依存（interval・sleep）。桁（poller 1本 vs 接続数×）が要点で絶対値は環境で動く
- 実運用の接続数上限はメモリ/fd（EXP-31: ~1万〜1.5万/タスク）。ここは仕組みの検証
- 複数プロセスに接続が分かれると各プロセスに poller が要る → プロセス跨ぎは pub/sub（EXP-38）
- 初期スナップショットは poller キャッシュから配る（接続ごとの DB 読みは無い）

## 再利用できる成果物

- internal/ssehub: Registry（テナント単位 poller・参照カウント）と SSE HTTP ハンドラ
- docs/sse-fan-in.md: 多数購読時の対策と hub の作り方

## 次の実験

- なし

