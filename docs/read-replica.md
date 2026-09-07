# 予定/実績を primary とレプリカで読み分ける（EXP-20）

「予定と実績をマスターとレプリカで読み分けると負担が減る？優先度は？」への
実測の答え。

実装は [internal/readrouter](../internal/readrouter)（`Freshness` で primary/
レプリカを振り分けるルータ）。receipt は
[docs/results/exp-20](results/exp-20/exp-20-read-your-writes-replica-lag.md)。

## 凍結した仮説

1. 書いた直後にレプリカ（ラグあり）から読むと、自分の書き込みが見えない
   （read-your-writes 違反）。
2. 同じ読みを primary（Strong）から読めば、必ず見える。
3. ラグが解消した後の「過去の値」は、レプリカから読んでも一致する。
4. worker の dispatch ポーリングは Strong（primary）で行う。レプリカだと未反映で
   取りこぼす。

## 結果（primary=workerdb / レプリカ相当=workerdb2 / ラグ 300ms 手動模擬）

| 読み | 見えたか |
| --- | --- |
| 書いた直後・Eventual（レプリカ・ラグ中） | **見えない**（read-your-writes 違反） |
| 書いた直後・Strong（primary） | 見える（v=v1） |
| ラグ解消後・Eventual（レプリカ）で過去の値 | 見える（v=v1） |

## 指針

- **Strong（primary）で読むもの**：書いた直後の読み、read-your-writes が要る画面、
  worker の dispatch ポーリング。レプリカに回すと取りこぼす／自分の書き込みが
  見えない。
- **Eventual（レプリカ）へ回してよいもの**：不変な過去だけ。過去の実績・履歴・
  レポート・admin の横断集計。少し古くても壊れない。

```go
r := readrouter.New(primary, replica)
r.DB(readrouter.Strong)   // dispatch ポーリング・直後の読み → primary
r.DB(readrouter.Eventual) // 履歴・レポート → レプリカ（無ければ primary へ縮退）
```

## 優先度は低め

30 テナントでは primary に余裕がある。読み負担を下げたいなら、レプリカより先に
効くものがある：[covering 索引](query-plan-skew.md)・[有界化/keyset](date-search.md)・
[コストゲート](query-cost-gate.md)・キャッシュ。読み QPS が実測で primary を圧迫し
始めてから、**ラグを許容できる読みだけ**をレプリカへ回す。

## 保証しない範囲・未検証

- 実運用のラグは負荷で変動する。ラグ量の測定・監視は別途（`Seconds_Behind_Master`
  等）。ここは遅延つきの手動模擬で振る舞いを再現しただけ。
- read-your-writes を厳密にやるなら、書き込み後しばらく Strong に固定する
  （セッションのピン留め）。
- レプリカのフェイルオーバ・整合（GTID）は本実験外。
