# EXP-58 共有ワーカーでスコープ強制を外すと越境する。強制ありは cross-leak=0

| | |
| --- | --- |
| Experiment | EXP-58 / tenant-scope-enforcement |
| Starting SHA | `67f65330bcc4` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) スコープ強制あり（WHERE tenant_id=? を必ず付ける）: テナント A の処理は A の行だけ。 他テナント B への読み漏れ=0・書き込み=0（cross-leak=0）。 2) スコープ書き忘れ（status だけで絞る）: B の行まで読める（情報漏洩）し、B の行まで done にしてしまう（データ破壊）。cross-leak>0。 3) 共有ワーカーの安全は『プロセス分離』でなく『クエリのテナント境界強制』で決まる。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=67f65330bcc4+dirty |
| Started / Ended | 2026-09-07T21:28:37Z / 2026-09-07T21:28:37Z |

## Results

### スコープ強制あり・読み: A だけ見える（cross-leak=0） — OK

| 数えたもの | 値 |
| --- | --- |
| cross_leak | 0 |
| mine | 100 |

### スコープ書き忘れ・読み: B の行まで見える（情報漏洩） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| cross_leak | 100 |
| mine | 100 |

- A の処理なのに B の 100 行が読めた（越境）

### スコープ強制あり・書き: A だけ done（他テナント破壊=0） — OK

| 数えたもの | 値 |
| --- | --- |
| cross_write | 0 |
| mine_done | 100 |

### スコープ書き忘れ・書き: B の行まで done（データ破壊） — **事故あり**

| 数えたもの | 値 |
| --- | --- |
| cross_write | 100 |
| mine_done | 100 |

- A の処理なのに B の 100 行を done にした（越境更新）

## Verdict

共有ワーカーの安全は『プロセスを分けること』でなく『全クエリにテナント境界を強制すること』で決まる。WHERE tenant_id=? を強制すればテナント A の処理は A の 100 行だけを触り、他テナント B への読み漏れ=0・書き込み=0。境界を1本書き忘れると B の 100 行まで読め（情報漏洩）、done にしてしまう（データ破壊）。だから境界は人手でなく repo.Scope で機械注入し、生 SQL は sqllint で禁止する。プロセス分離は最後の砦。

## 適用範囲

- MySQL 8.0 / 共有表 scope_item に tenantA・tenantB 各 100 行が同居 / A の処理を実行
- cross_leak = 他テナント(B)の行を読んだ数、cross_write = 他テナント(B)の行を done にした数
- scoped = クエリに WHERE tenant_id=? を付ける（repo.Scope 相当）。unscoped = 書き忘れ

## 保証しない範囲・未検証

- 実コードでは repo.Scope が :tenant を機械的に注入し、生 SQL は sqllint(rawdb/layerimport)で禁止する（EXP-8/9）
- テナントは ctx から取る（クライアント入力を信じない）。ここは processing を固定して境界の有無だけを見た
- 共有ワーカーの分離はプロセス境界でなく『全クエリのスコープ強制』で決まる。だから機械強制が要る
- 高感度テナントはさらにプロセス/資格情報を分けてブラスト半径を物理的に限定（docs/worker-tenancy.md）

## 再利用できる成果物

- internal/scopelab: ProcessRead/ProcessWrite（スコープ強制の有無で越境を測る）
- docs/tenant-scope.md: 共有ワーカーのテナントスコープ強制

## 次の実験

- （セキュリティ実測の追加ぶん）

