#!/usr/bin/env bash
# postflight.sh — 「テストが exit 0」だけを成功条件にしないための健全性ゲート。
#
# これが検査すること（1つでも欠けたら非0で終わる）:
#   - MySQL が生きている
#   - read/write probe が通る
#   - 接続数が baseline へ戻っている（接続リーク検出）
#   - MySQL が途中で再起動していない（Uptime が減っていない）
#   - go test の子プロセスが残っていない（プロセスリーク検出）
#
# 使い方:
#   scripts/postflight.sh baseline   > /tmp/base   # 実験前に baseline を取る
#   scripts/postflight.sh check <phase> <baseline_uptime> <baseline_conns>
#
# MYSQL_DSN が無ければ MySQL 検査は skip（SQLite だけの実験のため）。
set -uo pipefail

mysql_admin() { mysql -uroot "$@" 2>/dev/null; }

conns() { mysql_admin -N -e "SHOW GLOBAL STATUS LIKE 'Threads_connected'" | awk '{print $2}'; }
uptime_s() { mysql_admin -N -e "SHOW GLOBAL STATUS LIKE 'Uptime'" | awk '{print $2}'; }

rw_probe() {
  mysql_admin -e "
    CREATE DATABASE IF NOT EXISTS postflight_probe;
    USE postflight_probe;
    DROP TABLE IF EXISTS p;
    CREATE TABLE p (k INT PRIMARY KEY, v VARCHAR(16) NOT NULL);
    INSERT INTO p (k,v) VALUES (1,'ok') ON DUPLICATE KEY UPDATE v='ok';
    SELECT v FROM p WHERE k=1;
    DROP TABLE p;
  " | grep -q ok
}

case "${1:-}" in
  baseline)
    if ! mysqladmin -uroot ping >/dev/null 2>&1; then echo "MYSQL_DOWN_AT_BASELINE"; exit 3; fi
    echo "$(uptime_s) $(conns)"
    ;;
  check)
    phase="${2:-?}"; base_up="${3:-0}"; base_conn="${4:-0}"
    fail=0
    if ! mysqladmin -uroot ping >/dev/null 2>&1; then
      echo "POSTFLIGHT FAIL [$phase]: MySQL is DOWN"; exit 2
    fi
    if ! rw_probe; then echo "POSTFLIGHT FAIL [$phase]: read/write probe failed"; fail=1; fi
    now_up=$(uptime_s)
    # Uptime が baseline より小さい = 途中で再起動した
    if [ -n "$now_up" ] && [ "$now_up" -lt "$base_up" ]; then
      echo "POSTFLIGHT FAIL [$phase]: MySQL restarted mid-run (uptime $now_up < $base_up)"; fail=1
    fi
    # 接続が baseline+3 を超えて残っているか。
    # ★ただし、終了直後は「クライアントは exit したが MySQL がまだ TCP を回収していない」
    #   接続が残る（測定器を疑う: 最初これを即時に見て EXP-5 を誤検出した）。
    #   回収を待つため数秒ポーリングしてから判定する。
    now_conn=$(conns)
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if [ -z "$now_conn" ] || [ "$now_conn" -le $((base_conn + 3)) ]; then break; fi
      sleep 1; now_conn=$(conns)
    done
    if [ -n "$now_conn" ] && [ "$now_conn" -gt $((base_conn + 3)) ]; then
      echo "POSTFLIGHT FAIL [$phase]: connection leak (now $now_conn > baseline $base_conn +3, after 10s drain wait)"; fail=1
    fi
    # go test の子プロセスが残っている
    leaked=$(pgrep -af "\.test|/loadsim|/proxylab|sqlitelab|effectlab|fencelab|shutdownlab|migratelab" 2>/dev/null | grep -v postflight | wc -l)
    if [ "$leaked" -gt 0 ]; then
      echo "POSTFLIGHT FAIL [$phase]: $leaked leaked test/child process(es)"
      pgrep -af "\.test|/loadsim|sqlitelab" | grep -v postflight | head; fail=1
    fi
    if [ "$fail" -eq 0 ]; then
      echo "POSTFLIGHT OK [$phase]: mysql alive, rw ok, uptime=$now_up conns=$now_conn (base $base_conn)"
    fi
    exit $fail
    ;;
  *)
    echo "usage: postflight.sh baseline | check <phase> <base_uptime> <base_conns>"; exit 64;;
esac
