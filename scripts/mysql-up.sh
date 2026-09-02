#!/usr/bin/env bash
# 検証用の MySQL 8.0 を用意して、MYSQL_DSN を表示する。
#
#   ./scripts/mysql-up.sh            # docker があれば docker、無ければローカルの mysqld
#   eval "$(./scripts/mysql-up.sh --export)"   # そのまま MYSQL_DSN を環境に入れる
set -euo pipefail

DB=${DB:-workerdb}
USER=${USER_NAME:-worker}
PASS=${PASS:-workerpw}
PORT=${PORT:-3306}
DSN="${USER}:${PASS}@tcp(127.0.0.1:${PORT})/${DB}?parseTime=true&loc=UTC"

export_only=false
[[ "${1:-}" == "--export" ]] && export_only=true

log() { $export_only || echo "$@" >&2; }

if docker info >/dev/null 2>&1; then
  log "docker で MySQL 8.0 を起動する"
  if ! docker ps -a --format '{{.Names}}' | grep -qx worker-mysql; then
    docker run -d --name worker-mysql \
      -e MYSQL_ALLOW_EMPTY_PASSWORD=1 \
      -e MYSQL_DATABASE="${DB}" \
      -e MYSQL_USER="${USER}" \
      -e MYSQL_PASSWORD="${PASS}" \
      -p "${PORT}:3306" \
      mysql:8.0 >/dev/null
  else
    docker start worker-mysql >/dev/null
  fi
  log "起動を待っている..."
  for _ in $(seq 1 60); do
    if docker exec worker-mysql mysqladmin ping -h127.0.0.1 --silent >/dev/null 2>&1; then break; fi
    sleep 1
  done
  # §9 の検証テストは SET GLOBAL を使うので権限を足す
  docker exec worker-mysql mysql -uroot -e \
    "GRANT SYSTEM_VARIABLES_ADMIN, PROCESS ON *.* TO '${USER}'@'%'; FLUSH PRIVILEGES;" >/dev/null 2>&1 || true
else
  log "docker が無いので、ローカルにインストールされた MySQL を使う"
  log "（未インストールなら: sudo apt-get install -y mysql-server）"
  if ! mysqladmin ping --silent >/dev/null 2>&1; then
    mkdir -p /var/run/mysqld && chown mysql:mysql /var/run/mysqld 2>/dev/null || true
    mysqld --user=mysql --daemonize --skip-name-resolve
    for _ in $(seq 1 60); do
      mysqladmin ping --silent >/dev/null 2>&1 && break
      sleep 1
    done
  fi
  mysql -e "
    CREATE DATABASE IF NOT EXISTS ${DB} CHARACTER SET utf8mb4;
    CREATE USER IF NOT EXISTS '${USER}'@'%' IDENTIFIED BY '${PASS}';
    GRANT ALL PRIVILEGES ON ${DB}.* TO '${USER}'@'%';
    GRANT SYSTEM_VARIABLES_ADMIN, PROCESS, RELOAD ON *.* TO '${USER}'@'%';
    FLUSH PRIVILEGES;"
fi

if $export_only; then
  echo "export MYSQL_DSN='${DSN}'"
else
  echo "MYSQL_DSN='${DSN}'"
fi
