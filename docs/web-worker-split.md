# Worker / Web の分離と接続予算（統合スケルトン）

これまでの実験の結論を、実際に動く形に結線したもの。

- 統合ワーカー: `internal/tenantworker`（lease × fan-out × backoff × テナント別処理 × fence）
- Web プロセス: `cmd/web`（別プロセス・別プール・予算ガード・graceful shutdown）
- 接続予算ガード: `poolbudget.Guard`

## なぜ分けるか（EXP の結論）

| | Web | Worker |
| --- | --- | --- |
| スケール軸 | ユーザートラフィック（急変） | テナント数（30・ほぼ固定） |
| 混ぜると | スパイクでオートスケール → Worker も増え lease 二重・接続飽和（EXP-5/EXP-2） | |

別プロセスにして、プールを別々にサイズし、障害を独立させる。コードは同じモジュール。

## 接続予算ガード（起動時 fail-fast）

web と worker を別プロセスにしても、**合計が DB 上限を超えてはいけない**。
`cmd/web` は起動時に確かめ、超えるなら落ちる:

```
接続予算オーバー: 要求 2040 > 予算 900（max 1000 - 予約 100）[web=100×20=2000 worker=2×20=40]
```

`WEB_REPLICAS × WEB_POOL + WORKER_REPLICAS × WORKER_POOL ≤ DB_MAX - RESERVED` を不変条件にする。
30 テナント・上限 1000 なら、web 40 台 × 20 + worker 2 × 20 = 840 ≤ 900 で収まる（EXP-16 の担当テナント数も 30 なら 1ms）。

## 統合ワーカー `internal/tenantworker`

1ループで:

1. **担当を決める**（EXP-2）: 候補テナントを lease で Renew/Acquire。取れたテナントだけ担当（fence も持つ）。
   取れないテナントは他の worker が担当 → **二重起動しない**。
2. **1クエリに畳んでポーリング**（EXP-14/16）: 担当テナント限定の `IN` で1回引く。全テナント無条件はしない。
3. **テナント別に切り分けて処理**: `tenant_id` で bucket 化し、テナント単位のハンドラへ。**別テナントの命令は渡さない**（misrouted を数え、0 を検証）。
4. **claim は fence 付き**: `UPDATE ... SET fence=? WHERE tenant_id=? AND command_id=? AND state='pending'`。古い担当は掴めない。
5. **実績は独立に記録**（EXP-1）: action の戻り値を receipt にせず `cmd_result` に追記。
   timeout は `ErrOutcomeUnknown` → `unknown` 状態にし、**自動再実行しない**。
6. **適応的バックオフ**（EXP-17）: 空振りで間隔を倍々、仕事で Min へ。
7. **終了時に lease を返す**（EXP-3）。

### 実測（統合テスト）

- 3テナント × 20命令 = 60件を **1回のポーリング**で捌き、**テナント越え 0**、各ハンドラは自分の20件だけ受領、`cmd_result` に60件。
- 同じテナントを2 worker が奪い合う → **w1=40 / w2=0 / 合計40**（lease で片方だけ。二重処理なし）。

## Web プロセス `cmd/web`

- `config` で設定を1箇所読み（DSN 必須で fail-fast、プールは既定つき）。`Audit` は値を出さずキーと出所だけ。
- **Web 専用の小さいプール**（EXP-5 の膝。大きくしても遅延が伸びるだけ）。
- テナントは URL から取り **`repo.Scope` に束縛**（`GET /tenants/{tenant}/robots`）。Web 層でもテナント越えは型で防ぐ。
- `/healthz`（DB ping）・`/readyz`（プールの空き。飽和したら readiness を落とす）。
- **graceful shutdown**（EXP-3）: SIGTERM で受付を止め、処理中を流し切ってから閉じる。

## 環境変数

| 変数 | 既定 | 種別 |
| --- | --- | --- |
| `MYSQL_DSN` | — | **必須**（無ければ起動しない） |
| `PORT` | 8080 | 任意 |
| `WEB_POOL` | 20 | 任意 |
| `DB_MAX_CONNECTIONS` | 1000 | 任意 |
| `DB_RESERVED_CONNECTIONS` | 100 | 任意 |
| `WEB_REPLICAS` / `WORKER_REPLICAS` / `WORKER_POOL` | 40 / 2 / 20 | 任意（予算計算用） |

## 実測: 共有プールだと Worker が待たされる（EXP-29）

「分けた方がいい」を数字で。総接続数を同じ（10）にして、Web と Worker で1プールを共有した
場合と、役割ごとに分けた場合で、Worker の軽い問い合わせのレイテンシを比べた。

実装は [internal/procseplab](../internal/procseplab)、receipt は
[docs/results/exp-29](results/exp-29/exp-29-web-worker-pool-separation.md)。

| 構成 | Worker p50 | Worker p95 |
| --- | --- | --- |
| 共有（Web8+Worker2 を1プール） | 48ms | **150ms** |
| 分離（Worker 専用2 / Web 8） | 0.19ms | **0.27ms** |

- Web バースト（`SLEEP(50ms)`×16 並行）が接続を占有し、共有だと Worker の `SELECT 1` が
  acquire 待ちで p95 が **550 倍**。総接続数は同じなので、効いているのは「分けたこと」。
- 役割ごとにプールを持てば、Worker は Web の影響を受けない。プロセス/コンテナを分ければ
  CPU・メモリ・障害も隔離できる。接続予算の配分は [poolbudget](../internal/poolbudget) で。
