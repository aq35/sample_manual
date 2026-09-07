# poison / dead-letter（EXP-45）

必ず失敗する命令（poison：壊れた payload・存在しないロボット・恒久エラー）を無限リトライすると、
キューが永遠に drain せず、リトライが延々 DB と外部を叩き続ける。試行上限を決め、超えたら
**dead（隔離）**へ落としてアラートし、良い命令は流し続ける。

実装は [internal/dlqlab](../internal/dlqlab)、receipt は
[docs/results/exp-45](results/exp-45/exp-45-poison-dead-letter.md)。

## 結果（10命令中1つが poison / DLQ は maxAttempts=3）

| 方式 | pending | done | dead | poison の試行 |
| --- | --- | --- | --- | --- |
| 上限なし（無限リトライ） | **1**（poison が残り続ける） | 9 | 0 | **8**（周回数ぶん・青天井） |
| 上限→dead（DLQ） | **0**（drain） | 9 | **1** | **3**（有界） |

- 上限なしだと poison は pending のまま残り、試行が周回のたびに増え続ける（キューが drain しない）。
- DLQ は poison を maxAttempts で dead に隔離し、キューは drain。試行は有界。良い命令は両方で done。

## 要点

- **試行上限（maxAttempts）を決め、超えたら dead に落とす**。dead は消さずに残し、**アラート**して
  人が原因を調べる。原因修正後に **replay**（dead→pending へ戻す）する運用にする。
- **失敗しても後続は処理を続ける**（先頭で止めない）。止める実装（strict 順序）だと poison が
  後続もブロックして更に悪い。
- **バックオフ（[EXP-17](adaptive-backoff.md)）と併用**：隔離までの間も間隔を伸ばし、DB と外部を守る。
- 恒久エラー（4xx 相当）と一時エラー（5xx・タイムアウト）を区別し、恒久はすぐ dead、一時だけ
  リトライ、が理想。少なくとも**回数で頭打ち**にする。
- state 遷移の SQL は**代入順序に注意**（`SET state=..., attempts=attempts+1` の順。MySQL は左→右で
  評価し後の式が更新後の値を見る）。EXP-45 で1回踏んで直した。

## 保証しない範囲・未検証

- 無限リトライは実際には永遠に drain しない（本実験は 8 周で代表し、試行が周回数に比例することを示す）。
- 恒久/一時エラーの分類は本実験の対象外（ここは「回数上限で隔離」に集中）。
- dead の replay 手順・アラート連携は運用（本実験外）。
