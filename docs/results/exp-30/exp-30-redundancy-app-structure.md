# EXP-30 複数レプリカで安全に動かすためのアプリの作り（claim・fence・接続予算）

| | |
| --- | --- |
| Experiment | EXP-30 / redundancy-app-structure |
| Starting SHA | `4107029732ff` (作業ツリーに未コミットの変更あり) |
| Meter version | `expkit/2` |
| Hypothesis (frozen before result) | 1) N レプリカが同じキューを取り合っても、原子的 claim なら各件はちょうど1回だけ処理される（取りこぼしも二重も無い）。 2) 世代交代（handoff）後、古い担当（小さい fence）の書き込みは fence ガードで弾かれ、新しい担当（大きい fence）だけが通る。 3) 接続予算はレプリカ数で割る。N 台 × 1台の取り分 が DB 予算を超えないことを起動時に確かめる。 |
| Environment | go1.26.0 linux/amd64 cpu=4 gomaxprocs=4 mysql=8.0.46-0ubuntu0.24.04.4 sha=4107029732ff+dirty |
| Started / Ended | 2026-09-07T08:24:02Z / 2026-09-07T08:24:03Z |

## Results

### 原子的 claim: 4レプリカで500件を取り合う → 各件ちょうど1回 — OK

| 数えたもの | 値 |
| --- | --- |
| claimed | 500 |
| owners_worked | 4 |
| pending | 0 |
| sum_by_owner | 500 |

- owner ごとの取り分がばらけ、合計＝件数・取りこぼし0・二重0（条件つき UPDATE が保証）

### fence: 新担当(fence=2)は通り、古い担当(fence=1)の遅延書き込みは弾かれる — OK

| 数えたもの | 値 |
| --- | --- |
| newer_accepted | 1 |
| stale_rejected | 1 |

- 最終 val=by-B fence=2（古い担当の値で上書きされない）

### 接続予算: レプリカ数で割る（起動時 Guard で fail-fast） — OK

| 数えたもの | 値 |
| --- | --- |
| budget | 900 |
| demand | 400 |
| fits | 1 |

- web4×100 + worker4×50 = 600 ≤ 900 → OK: はい
- web4×300 = 1200 > 900 → 起動時に弾く: はい

## Verdict

冗長化は『N 台並べれば動く』ではなく、アプリの中がそれ用に出来ていること: 仕事の取得は原子的 claim（二重処理を防ぐ）、書き込みは fence ガード（古い担当を弾く）、書き込みは冪等（EXP-27）、接続予算はレプリカ数で割る（起動時に Guard）。担当割り当ては lease（EXP-2）、速い failover は graceful shutdown（EXP-3）。

## 適用範囲

- MySQL 8.0 / キュー500件を4レプリカ / fence は redstate の条件つき UPDATE / 予算は poolbudget
- claim は『pending を1件見て、その id を条件つき UPDATE』。取れるのは1レプリカだけ
- レプリカ・レジストリやリーダー選出は使わず、DB の原子性と fence だけで安全にする

## 保証しない範囲・未検証

- lease による『テナント→担当レプリカ』の割り当ては EXP-2（ここは claim と fence の意味論に集中）
- 二重の“効果”を完全に防ぐには冪等性（EXP-27）も要る。claim は二重“処理”を、fence は古い“書き込み”を防ぐ
- graceful shutdown（drain して lease を返す）で failover を速くする（EXP-3）
- メモリ上のキャッシュはどのレプリカでも成り立つ形に（テナント単位・共有しない）

## 再利用できる成果物

- internal/redundancylab: 原子的 claim・fence ガード・接続予算の確認
- docs/redundancy.md: 冗長化するときアプリの中はどうあるべきか

## 次の実験

- なし

