#!/usr/bin/env bash
# postflight-counterproof.sh — 「postflight を外すと MySQL 停止を見逃す」ことの証明。
#
# 主張: `go test ./...` の exit 0 だけを成功条件にすると、MySQL が停止していても
# 「成功」に見える。DB を使うテストは MySQL に繋がらないと **fail ではなく skip** するからだ。
# postflight は能動的に ping + read/write probe をするので、停止を捕まえる。
#
# ここでは MySQL を止めずに、繋がらないエンドポイント（使われていないポート）を相手に
# 両者の振る舞いの差を見せる。
set -uo pipefail
cd "$(dirname "$0")/.."

DEAD_PORT=13306   # 何も居ないポート
DEAD_DSN="worker:workerpw@tcp(127.0.0.1:${DEAD_PORT})/workerdb?parseTime=true&loc=UTC"

echo "=== ① go test（DB パッケージ）を、死んだエンドポイントに向けて走らせる ==="
# mysqltest.DSN は繋がらないと t.Skip する設計なので、テストは skip されて exit 0 になる
if MYSQL_DSN="$DEAD_DSN" go test ./internal/backuplab/ -run TestEXP11 -count=1 -timeout 60s >/tmp/cp_test.txt 2>&1; then
  test_exit=0
else
  test_exit=$?
fi
grep -qE "SKIP|no test|ok " /tmp/cp_test.txt && skipped=yes || skipped=no
echo "go test exit=${test_exit}（skip されて 0 に見える: ${skipped}）"
tail -2 /tmp/cp_test.txt

echo
echo "=== ② 同じ死んだエンドポイントに postflight の probe をかける ==="
# postflight の read/write probe と同じことを、死んだポートに対してやる
if mysqladmin --protocol=TCP -h 127.0.0.1 -P "$DEAD_PORT" -u worker -pworkerpw ping >/dev/null 2>&1; then
  probe=alive
else
  probe=down
fi
echo "probe 結果: MySQL は ${probe}"

echo
if [[ "$test_exit" -eq 0 && "$probe" == "down" ]]; then
  echo "COUNTER-PROOF OK: go test は exit 0（skip）だが、probe は down を検出した。"
  echo "  => postflight を外すと、MySQL 停止が『成功』に埋もれる。だから postflight が要る。"
  exit 0
else
  echo "COUNTER-PROOF INCONCLUSIVE: test_exit=$test_exit probe=$probe"
  exit 1
fi
