# 誤コミットした 9MB binary の記録と再発防止（最終健全性レビュー #4）

`sizeprobe` を現在の tree から消しただけでは、9MB の実行ファイルは Git 履歴に残る。
**今回は履歴改変を自動実施しない。** 事実と判断を記録する。

## 事実

| 項目 | 値 |
| --- | --- |
| binary が入った commit | `de9027e`（PR11 EXP-10 SQLite companion） |
| 削除した commit | `bdc8149`（sizeprobe 削除 + .gitignore） |
| blob | `d140228ac87f4ebfa24576d04af07fd6b93c4d9d` |
| 実サイズ | 9,264,518 bytes（約 8.8 MiB） |
| pack 後（loose object） | 約 5.5 MiB（圧縮済み） |
| .git 全体 | 約 9.5 MiB（うち blob が約 58%） |
| 到達可能な ref | `main`・`claude/exp-load`（local と origin 両方） |

原因: `go build ./cmd/sizeprobe`（`-o` 無し）を叩くと作業ディレクトリに実行ファイルが落ち、
`git add -A` が拾った。

## リポジトリサイズへの影響

- clone サイズが約 5.5 MiB 増える（1 回の誤コミットで .git がほぼ倍）。
- 履歴を辿る操作（`git log -p`、`git clone --mirror`）に blob が乗る。
- 現状は絶対値としては小さい（合計 9.5 MiB）。

## 判断: 履歴から除去するか、9MB を許容するか

**今回は許容し、履歴改変は行わない。** 理由:

- blob は `main` と `claude/exp-load` の両方に push 済み。
  除去には `git filter-repo` で両 branch を書き換え、**force-push が必要**。
- force-push は既存の clone / branch を壊す（他の人の作業ツリーが履歴と食い違う）。
- 影響は 5.5 MiB と小さく、緊急性がない。

**将来やるなら**（容量が問題になったとき、関係者の合意のうえで）:

```
git filter-repo --path sizeprobe --invert-paths
git push --force-with-lease origin main claude/exp-load
```

その際は clone を持つ全員に再 clone を依頼する。単独 blob の除去でしかないので
`filter-repo` の一発で済む。

## 既存 clone / branch への影響

- 現時点では履歴を変えていないので、既存 clone は**壊れていない**（そのまま使える）。
- tip には binary が無い（`bdc8149` で削除済み）ので、**新しい clone や checkout に
  binary は展開されない**。履歴を辿ったときだけ blob が現れる。

## 再発防止（実装済み）

| 対策 | 実装 |
| --- | --- |
| build 出力を固定先へ | ビルドは `./bin/` か tmp へ（`-o`）。root へ実行ファイルを生成しない |
| `.gitignore` | `/sizeprobe` `/worker` `/loadsim` `/proxylab` `/*lab` `/bin/` `/.run-logs/` `*.test` `*.out` |
| tracked binary 検査 | `scripts/check-no-binaries.sh`（ELF/Mach-O/PE を先頭バイトで検出、1 MiB 超を警告） |
| commit 前 gate | `scripts/check-no-binaries.sh --staged`（`git diff --cached --stat` を表示し、staged の実行形式を止める） |
| clean checkout での最終 gate | `scripts/run-all.sh` の postflight が `check-no-binaries.sh` と worktree clean を検査 |

### pre-commit フックの例（任意）

```sh
# .git/hooks/pre-commit
#!/bin/sh
exec scripts/check-no-binaries.sh --staged
```
