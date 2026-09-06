# 有効期限つきの秘密（SecretManager 相当）の扱い方

- 実装: `internal/secretcache`（期限つきキャッシュ）・`internal/config`（ManagedSecret）
- DB 資格情報のローテーション: EXP-13（`internal/credlab`, `docs/credential-rotation.md`）

## 原則

**期限を持つ値は「リース」として扱う**（EXP-2 の lease と同じ）。取って終わりにしない。

- 取得時に `expires_at` を一緒に保持する。
- **期限の手前で先回りして更新**（残りが `RefreshBefore` を切ったら）。期限ちょうどで切れるのを待たない。
- 更新に失敗しても、期限内の古い値は**握ったまま延命**（fail-open）。完全に無いときだけエラー（fail-closed）。
- 起動時に必須の秘密が取れなければ**落とす**（`config` と同じ fail-fast）。

## テナント分離

必要。SecretManager 側で**パスをテナントで分ける**のが基本（`secrets/tenant/<id>/db`）。
1つの JSON にテナント全部を詰めない:
- IAM をテナント単位で効かせられない
- 監査ができない
- ローテーションの単位が粗くなる（1テナントの更新で全テナントを巻き込む）

メモリに載せるときも `(tenant, name)` をキーにする（`secretcache` はこれを型で強制）。
フラットな `map[name]` は、テナント越えの罠（`tenantcache` と同型）。

## メモリキャッシュと排他

SecretManager は遅く・課金され・レート制限があるので**キャッシュしてよい**（ディスクには書かない）。
排他は要る:

1. **thundering herd 防止**: 期限切れの瞬間に全 goroutine が一斉に取りに行かないよう、
   **singleflight** で1本にまとめる。`secretcache` で実測: **100 goroutine 同時アクセスでも元の取得は1回**。
2. **書き込みの一貫性**: 更新中に古い値と新しい値が混ざらないよう `RWMutex`（読みは並行・差し替えは排他）。

```
Get(tenant, name):
  RLock で読む
  ├ 期限まで余裕      → そのまま返す
  ├ 期限が近い(有効)  → 今の値を返しつつ、裏で先回り更新（singleflight で1本）
  └ 無い/期限切れ     → singleflight で1本だけ取得、全員でその結果を待つ
                        取得失敗かつ古い値あり → 古い値で延命（fail-open）
```

## config での必須・期限つき

`config.Loader.RequiredSecret(name, fetch, refreshBefore)`:
- 起動時に一度取得し、取れなければ `Err()` に積む（fail-fast）。
- `Start(onRotate)` で先回り更新のループを回す。値が変わったら `onRotate(newValue)` が呼ばれる。
- DB 資格情報なら、この `onRotate` から**プールの graceful 入れ替え**（EXP-13）を呼ぶ。

`Audit()` はキーと出所だけを出し、**値そのものは出さない**（DSN・鍵の漏洩防止）。
