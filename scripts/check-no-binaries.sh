#!/usr/bin/env bash
# check-no-binaries.sh — tracked なファイルに実行形式/大容量が紛れていないか検査する。
#
# 誤って `go build ./cmd/x`（-o 無し）を叩くと cwd に実行ファイルが落ち、
# `git add -A` で拾われる。それを commit 前 / CI で止める最終ゲート。
#
#   scripts/check-no-binaries.sh            # tracked 全体を検査
#   scripts/check-no-binaries.sh --staged   # staged 分だけ検査（pre-commit 向き）
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

MAX_BYTES=$((1024*1024))   # 1 MiB を超える tracked ファイルは疑う
fail=0

if [ "${1:-}" = "--staged" ]; then
  files=$(git diff --cached --name-only --diff-filter=ACM)
  echo "=== staged の変更（git diff --cached --stat）==="
  git diff --cached --stat | tail -20
else
  files=$(git ls-files)
fi

while IFS= read -r f; do
  [ -z "$f" ] && continue
  [ -f "$f" ] || continue
  # 実行形式（ELF / Mach-O / PE）を先頭バイトで判定
  magic=$(head -c4 "$f" 2>/dev/null | od -An -tx1 | tr -d ' \n')
  case "$magic" in
    7f454c46)  echo "REJECT: ELF 実行形式が tracked: $f"; fail=1;;      # \x7fELF
    feedface|cffaedfe|cefaedfe|feedfacf)  echo "REJECT: Mach-O 実行形式が tracked: $f"; fail=1;;
    4d5a*)     echo "REJECT: PE/EXE らしきものが tracked: $f"; fail=1;;
  esac
  # 大容量
  sz=$(wc -c < "$f" 2>/dev/null || echo 0)
  if [ "$sz" -gt "$MAX_BYTES" ]; then
    echo "WARN: 1 MiB 超の tracked ファイル ($sz bytes): $f"
    # docs/results の JSON などは許容。実行形式でなければ WARN 止まり
  fi
done <<< "$files"

if [ "$fail" -ne 0 ]; then
  echo "=> 実行形式が tracked に含まれている。ビルド生成物は ./bin/ か tmp へ出すこと（.gitignore 済）。"
  exit 1
fi
echo "OK: tracked に実行形式なし"
