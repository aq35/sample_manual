# 冗長化するとき、アプリの中はどうあるべきか（EXP-30）

冗長化＝同じアプリを N 個並べて、1つ落ちても止まらないようにすること。だが「N 台並べれば
動く」ではない。**アプリの中がそれ用に出来ていないと、二重処理・古い担当の上書き・接続枯渇**
を起こす。実測で確かめた3点。

実装は [internal/redundancylab](../internal/redundancylab)、receipt は
[docs/results/exp-30](results/exp-30/exp-30-redundancy-app-structure.md)。

## 1. 仕事の取得は「原子的 claim」（二重処理を防ぐ）

N レプリカが同じキューを見る。各レプリカは「pending を1件見つける → その id を**条件つき
UPDATE** で claim（`... WHERE id=? AND state='pending'`）」を繰り返す。取れるのは1レプリカ
だけ（`affected=1`）、先を越されたら `affected=0` で次へ。

実測（500 件・4 レプリカ）: **claimed=500 / pending=0 / 二重=0**、取り分は 138/138/94/130 と
ばらけた。リーダー選出もレジストリも使わず、DB の原子性だけで exactly-once の**処理**になる。

> 「まず全部 SELECT してから処理」だと N 台が同じ行を処理して二重になる。条件つき UPDATE で
> 取ってから処理すること。

## 2. fence（世代番号）で古い担当の書き込みを弾く（正しさ）

claim だけでは足りない。担当が切り替わった（handoff）後に、**遅れていた古い担当**が目を覚まして
書き込むことがある。各書き込みに世代番号 fence を付け、`WHERE fence <= ?`（書き手の fence 以上の
ときだけ通す）で守る。

実測: handoff 後、新担当（fence=2）の書き込みは通り、古い担当（fence=1）の遅延書き込みは
弾かれ、値は新担当のまま。fence が「古い担当が生き返って上書きする」事故を防ぐ。

## 3. 接続予算をレプリカ数で割る（枯渇を防ぐ）

1台のプール設定のまま台数を増やすと、DB への接続は**台数に比例**して増え、`max_connections`
を食い潰す。予算はレプリカ数で割り、起動時に [poolbudget](../internal/poolbudget) の `Guard` で
確かめて fail-fast にする。

実測: `web 4×100 + worker 4×50 = 600 ≤ 900` は OK、`web 4×300 = 1200 > 900` は起動時に拒否。
Web と Worker を別プロセスで冗長化しても、**合計**が予算を超えないことをここで担保する。

## まとめ（冗長化のためにアプリが持つべき性質）

- [ ] 仕事の取得は原子的 claim（条件つき UPDATE）。read-then-act にしない
- [ ] 書き込みは fence ガード（古い担当＝小さい fence を弾く）
- [ ] 書き込みは冪等（[EXP-27](graphql.md)）。claim は二重*処理*を、fence は古い*書き込み*を、冪等は二重*効果*を防ぐ
- [ ] 接続プールはレプリカ数で割り、起動時に `poolbudget.Guard`
- [ ] 状態は DB に持つ（レプリカはステートレス）。メモリキャッシュはどのレプリカでも成り立つ形（テナント単位・共有しない）
- [ ] 担当の割り当ては lease（[EXP-2](fencing.md)）、速い failover は graceful shutdown（[EXP-3](shutdown.md)）

## 保証しない範囲・未検証

- lease による「テナント→担当レプリカ」の割り当ての詳細は EXP-2（ここは claim と fence の意味論）。
- 実際の failover 時間・split-brain の窓は lease の TTL に依る（別途）。
- 分散環境のキャッシュ整合（複数レプリカのメモリ）は「共有しない・DB を真とする」前提。
