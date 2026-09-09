# per-tenant worker の容量見積り（20テナント / Aurora 1000接続）

テナント境界のために **worker をテナント単位**にする構成で、**Aurora max_connections=1000・20テナント・
Web も Worker もある**ときの現実値。結論：**接続はまったく逼迫しない（3割も使わない）。20テナントなら
per-tenant worker は妥当。先に効く壁は接続でなく worker コンテナのメモリと Web の CPU。**

前提の考え方は [connection-budget](connection-budget.md)(EXP-60)/[pool-saturation](pool-saturation.md)(EXP-5)/
[worker-tenancy](worker-tenancy.md)/[capacity](capacity.md)(EXP-31)。予算計算は [internal/poolbudget](../internal/poolbudget)。

## 接続予算

```
Aurora max_connections            = 1000
予約（migration/監視/DBA/Aurora内部/余白）≈ 150
使える予算 budget                 ≈ 850   （さらに2割余白 → 狙いは demand ≤ ~680）
```

## 割り当て（20テナント・Web＋per-tenant Worker）

| 役割 | 台数 | 1台のプール | 小計 | 備考 |
| --- | --- | --- | --- | --- |
| **Worker（per-tenant）** | 20（HAで各2＝40） | **2〜4** | 40〜80（HA:80〜160） | 自テナントだけなのでプールは小さくてよい |
| **Web（共有・HA）** | 2〜3 | **16〜20**（[EXP-5](pool-saturation.md) の膝） | 32〜60 | 全テナントの Query/Mutation＋hub poll |
| reconcile/監視/移行/予備 | — | — | ~50 | 予約枠 |
| **合計 demand** | | | **~120〜270** | |

→ **予算 850 に対し demand 120〜270 ＝ 使用率 14〜32%**。接続は大きく余る。HA を厚くしても収まる。

## どこまで増やせる？（per-tenant のまま）

```
per-tenant worker（HA込み ~8接続/tenant）＋ Web固定 ~60
上限テナント数 ≈ (850 − 60) / 8 ≈ 約100テナント
```

- **~100テナント**までは per-tenant worker のまま接続予算に収まる。
- **100超**は per-tenant の掛け算で接続が効く（[EXP-60](connection-budget.md) の 1040 拒否）→ **シャード化（N テナント/worker）**
  か per-worker プールを絞る。

## 先に効く「接続以外」の壁（20なら平気・増えると要注意）

| 壁 | 20テナント | 数百テナント |
| --- | --- | --- |
| **worker コンテナのメモリ**（基準 ~300MB/台・[worked-examples](worked-examples.md)） | 20×300MB ≈ **6GB（普通）** | 300MB×N で**破綻**（[worker-tenancy](worker-tenancy.md)）→ シャード |
| **Web の CPU**（[EXP-31](capacity.md) の律速） | rps 次第でレプリカ数（接続は余る） | 同左＋横割り（[redundancy](redundancy.md)） |
| **Worker の DB 往復**（律速） | 小処理なら余裕 | batch＋coalesce（[EXP-14](fanout.md)） |

→ **接続より先に「per-tenant コンテナのメモリ」が効く**。20 は OK、増えるならシャードへ。

## 推奨設定（この規模の現実値）

- **Worker（per-tenant）**：プール **2〜4**。HA は lease singleton＋2レプリカ（待機側は接続を絞る/張らない、で
  掛け算を抑える・[fencing](fencing.md)）。
- **Web（共有）**：プール **16〜20** × レプリカ **2〜3**（HA/rps）。SSE は DB を 1:1 で持たせない（hub 共有・[sse-fan-in](sse-fan-in.md)）。
- **起動時 Guard**：`Σ(役割 × 台数 × プール) ≤ budget(850)` を検査して fail-fast（[EXP-60](connection-budget.md) の `poolbudget.Guard`）。
- **予約を明示**：migration/監視/DBA に 100〜150 空ける。

## まとめ

| 指標 | 値 |
| --- | --- |
| Aurora 接続予算 | 1000 − 予約150 ＝ **850** |
| 20テナントの demand | **~120〜270（使用率 14〜32%）** ＝余裕 |
| per-tenant のまま増やせる上限 | **~100テナント** |
| 先に効く壁 | 接続でなく **worker コンテナのメモリ（~300MB/台）** と **Web の CPU** |

- **20テナント・1000接続なら per-tenant worker で問題なし・接続は3割も使わない。**隔離のメリットを取りつつ余裕。
- **将来テナントが数百に増えるなら、接続より先に per-tenant コンテナのメモリが効く**ので、その時点で
  **シャード（N テナント/worker）**へ切り替えるのが現実的な設計線。

## 保証しない範囲・未検証

- プール本数・レプリカ数は目安（choice）。`poolbudget.Guard` に実値を入れて起動時チェックにする。
- rps とメモリは実機で測って詰める（[EXP-31](capacity.md)/[worked-examples](worked-examples.md)）。
- Aurora の予約すべき接続数は構成依存（監視/DBA/内部）。ここは 150 と仮定。
- RDS Proxy を挟むと DB 側接続がタスク数に比例しなくなる（[rds-proxy](rds-proxy.md)・未実測）。
