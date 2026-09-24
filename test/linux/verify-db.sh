#!/usr/bin/env bash
#
# 真实数据库实例上的备份适配器验证。
#
# 用三个真实的数据库实例（PostgreSQL、MySQL、MariaDB）跑共享合约测试
# （internal/application/backupcontract），覆盖那些**只有真实工具才能证明**的东西：
# argv 与工具的真实行为是否一致、恢复出来的内容是否真的等于归档、独立恢复会不会
# 碰真实数据、隔离恢复的临时库有没有被收尾。
#
# 用法：
#   bash test/linux/verify-db.sh
#
# 证据类型：**Linux 容器**（真实数据库实例）。它与「Linux 主机」是两类不同证据，
# 不得互相替代——真实主机的版本差异、权限差异、长时间大库备份仍需真机验证。
#
# 设计说明（为什么长这样）：
#
#   数据库服务端跑在容器里，**客户端工具也从容器里跑**——通过一组 `docker exec -i`
#   的包装脚本放进 PATH。这样：
#     · 不需要为测试再造一个镜像（golang 镜像里没有 pg_dump/mysqldump）；
#     · 用的是**服务端同版本**的客户端，而版本倒挂本来就会被适配器的预检拒绝；
#     · 测试进程的 PATH 被替换成「只有包装脚本 + /usr/bin:/bin」，因此跑的是哪个
#       客户端是确定的，不会被开发机上碰巧装着的 mysql-client 抢走。
#   代价：凭据要经 `docker exec -e` 传给容器，于是它会出现在**本机**的进程列表里。
#   这些都是 harness 自己的临时口令，与产品的凭据路径无关（产品里密码只经
#   PGPASSWORD / MYSQL_PWD 传给子进程，不进 argv）。

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SUFFIX=$$
NET=frz-db-verify-${SUFFIX}
PG=frz-verify-pg-${SUFFIX}
MYSQL=frz-verify-mysql-${SUFFIX}
MARIA=frz-verify-maria-${SUFFIX}

PG_IMAGE=${FRZ_PG_IMAGE:-postgres:16-alpine}
MYSQL_IMAGE=${FRZ_MYSQL_IMAGE:-mysql:8.0}
MARIA_IMAGE=${FRZ_MARIA_IMAGE:-mariadb:11}

DB_PASSWORD=frz-verify-pw
DB_NAME=frz_orders
DB_USER=frz

WORK_DIR=$(mktemp -d)
PASS_COUNT=0
FAIL_COUNT=0

log()  { printf '\n--- %s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*" >&2; }

cleanup() {
  for name in "$PG" "$MYSQL" "$MARIA"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# docker 与 go 都取绝对路径：跑测试时 PATH 会被替换掉，包装脚本里必须能直接执行 docker。
DOCKER=$(command -v docker) || { printf '需要 docker\n' >&2; exit 1; }
case "$DOCKER" in /*) ;; *) DOCKER=$(cd "$(dirname "$DOCKER")" && pwd)/$(basename "$DOCKER") ;; esac
GO=$(command -v go) || { printf '需要 go\n' >&2; exit 1; }
case "$GO" in /*) ;; *) GO=$(cd "$(dirname "$GO")" && pwd)/$(basename "$GO") ;; esac

docker network create "$NET" >/dev/null

# ==== 起实例 ====
log "启动数据库实例"

# 刻意**不**用 POSTGRES_DB：入口脚本会把库建成 postgres 属主的，而我们要的是
# 测试账号自己是属主（非超级用户的真实形态，也是 --clean 能跑通的前提）。
docker run -d --name "$PG" --network "$NET" \
  -e POSTGRES_PASSWORD="$DB_PASSWORD" \
  "$PG_IMAGE" >/dev/null
docker run -d --name "$MYSQL" --network "$NET" \
  -e MYSQL_ROOT_PASSWORD="$DB_PASSWORD" -e MYSQL_DATABASE="$DB_NAME" \
  "$MYSQL_IMAGE" >/dev/null
docker run -d --name "$MARIA" --network "$NET" \
  -e MARIADB_ROOT_PASSWORD="$DB_PASSWORD" -e MARIADB_DATABASE="$DB_NAME" \
  "$MARIA_IMAGE" >/dev/null

wait_for() { # 容器名 探测命令…
  local container=$1
  shift
  for _ in $(seq 1 120); do
    if "$@" >/dev/null 2>&1; then
      pass "${container} 就绪"
      return 0
    fi
    sleep 1
  done
  fail "${container} 未在 120 秒内就绪"
  docker logs "$container" 2>&1 | tail -20 >&2 || true
  exit 1
}

# 就绪探测一律走 **TCP**。
#
# 数据库容器的入口脚本会先起一个**临时实例**跑初始化、再把它停掉、然后启动真正对外
# 服务的实例；临时实例只监听 unix socket（入口脚本就是这么隔离它的）。因此在容器里
# 用默认的 socket 探测会**提前**判定就绪——随后真正那个实例还在重启，建账号就失败了。
# 这个坑只在 CI 上稳定复现（本地因为慢一点而侥幸躲过），TCP 探测则天然避开临时实例。
wait_for "$PG" docker exec "$PG" pg_isready -h 127.0.0.1 -U postgres
wait_for "$MYSQL" docker exec "$MYSQL" mysqladmin ping -h 127.0.0.1 --protocol=TCP -uroot -p"$DB_PASSWORD" --silent
wait_for "$MARIA" docker exec "$MARIA" mariadb-admin ping -h 127.0.0.1 --protocol=TCP -uroot -p"$DB_PASSWORD" --silent

# ==== 建测试账号 ====
#
# 刻意**不用**超级用户：真实部署里的备份账号不是超级用户，而隔离恢复需要
# CREATEDB（PostgreSQL）或建库权限（MySQL/MariaDB）——这两件事必须在真实例上证明，
# 否则「隔离恢复」在只有普通账号的生产环境里会直接失败，而本地测试看不出来。
log "创建测试账号（非超级用户，带建库权限）"

# 建账号的语句一律写成**幂等**的，好用「以结果为准」的重试兜住残余的时序问题。
create_pg_account() {
  "$DOCKER" exec -i "$PG" psql -U postgres -q -c \
    "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${DB_USER}') \
     THEN CREATE ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}' CREATEDB; END IF; END \$\$" || return 1
  local exists
  exists=$("$DOCKER" exec -i "$PG" psql -U postgres -tAc \
    "SELECT 1 FROM pg_database WHERE datname = '${DB_NAME}'" | tr -d '[:space:]') || return 1
  if [ "$exists" != "1" ]; then
    "$DOCKER" exec -i "$PG" psql -U postgres -q -c \
      "CREATE DATABASE ${DB_NAME} OWNER ${DB_USER}" || return 1
  fi
}

create_sql_account() { # 客户端 容器
  "$DOCKER" exec -i "$2" "$1" -uroot -p"$DB_PASSWORD" <<SQL
CREATE USER IF NOT EXISTS '${DB_USER}'@'%' IDENTIFIED BY '${DB_PASSWORD}';
GRANT ALL ON *.* TO '${DB_USER}'@'%';
FLUSH PRIVILEGES;
SQL
}

wait_until() { # 描述 命令…
  local desc=$1
  shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then
      pass "$desc"
      return 0
    fi
    sleep 2
  done
  fail "$desc 在 120 秒内没有成功"
  return 1
}

wait_until "PostgreSQL 账号 ${DB_USER}（CREATEDB，库属主）" create_pg_account
for pair in "${MYSQL}:mysql" "${MARIA}:mariadb"; do
  wait_until "${pair%%:*} 账号 ${DB_USER}（含建库权限）" create_sql_account "${pair##*:}" "${pair%%:*}"
done

# ==== 包装脚本 ====
#
# 每个引擎一个目录：跑某一家时 PATH 里**只有**那一家的客户端，免得
# 「跑的是哪个容器里的工具」变成一件靠 PATH 顺序决定的事。
log "生成客户端包装脚本"

wrap() { # 目录 容器 工具名
  local dir=$1 container=$2 tool=$3
  printf '#!/bin/sh\nexec %s exec -i -e PGPASSWORD="$PGPASSWORD" -e MYSQL_PWD="$MYSQL_PWD" %s %s "$@"\n' \
    "$DOCKER" "$container" "$tool" >"$dir/$tool"
  chmod 755 "$dir/$tool"
}

PG_BIN=${WORK_DIR}/bin-pg
MYSQL_BIN=${WORK_DIR}/bin-mysql
MARIA_BIN=${WORK_DIR}/bin-maria
mkdir -p "$PG_BIN" "$MYSQL_BIN" "$MARIA_BIN"
for tool in pg_dump pg_restore psql; do wrap "$PG_BIN" "$PG" "$tool"; done
for tool in mysqldump mysql; do wrap "$MYSQL_BIN" "$MYSQL" "$tool"; done
# MariaDB 只放 mariadb-dump / mariadb：官方镜像里就没有 mysqldump 这个名字，
# 让这条路径**端到端**跑一遍，而不是只靠单测。
for tool in mariadb-dump mariadb; do wrap "$MARIA_BIN" "$MARIA" "$tool"; done
pass "三家客户端包装脚本就绪"

# ==== 跑共享合约 ====
run_engine() { # 名称 目录 DSN 环境变量 测试名
  local name=$1 bin=$2 dsn=$3 env=$4 test_name=$5
  log "${name}：共享合约（backupcontract）"

  local output="${WORK_DIR}/${name}.log"
  local status=0
  # PATH 里**只有包装脚本**。
  #
  # 这条很要紧：CI runner 的镜像里自带 mysql / mysqldump / psql（ubuntu-latest 就有），
  # 只要 /usr/bin 还在 PATH 里，夹具与适配器就可能解析到**主机上**的那个客户端，
  # 于是连到一个根本没在监听的主机端口——而报错看起来像「数据库连不上」，
  # 与真正的原因（跑错了客户端）差得很远。
  # -buildvcs=false：PATH 里没有 git，而 Go 在仓库里构建时会去问 git 要版本信息。
  # 注意顺序：`env` 自己要用**当前**的 PATH 才找得到，因此先给变量、再改 PATH。
  # 一并跑指纹的元断言：它证明的是「夹具的指纹对这个引擎真的有分辨力」，
  # 少了它，整条往返断言可能是空转的（MySQL 里 `||` 是逻辑或就是这么骗过去的）。
  if env "${env}=${dsn}" PATH="${bin}" \
    "$GO" test -count=1 -buildvcs=false ./test/dbbackup/ \
    -run "${test_name}|TestFingerprintIsContentSensitive" -v >"$output" 2>&1; then
    status=0
  else
    status=1
  fi

  local passed failed
  passed=$(grep -c -- '--- PASS:' "$output" || true)
  failed=$(grep -c -- '--- FAIL:' "$output" || true)
  local skipped
  skipped=$(grep -c -- '--- SKIP:' "$output" || true)

  if [ "$skipped" -gt 0 ] && [ "$passed" -eq 0 ]; then
    fail "${name}：整组被跳过（DSN 或客户端工具没到位）"
    sed -n '1,40p' "$output" >&2
    return
  fi

  # 每个子用例（含合约的 9 条）都单独计数，便于看出失败落在哪一条。
  if [ "$status" -eq 0 ] && [ "$failed" -eq 0 ]; then
    pass "${name}：${passed} 项通过（含子用例）"
  else
    fail "${name}：${failed} 项失败（退出码 ${status}）"
    # 输出**整份** go test 日志，不做二次过滤。
    # 过滤过一次就吃过亏：用例失败信息是多行的（SQL、stderr 各占一行），
    # 只挑含 ".go:" 的行会把真正的报错丢掉，剩下的信息只够猜。
    printf '%s\n' "---- ${name}：go test 完整输出 ----" >&2
    cat "$output" >&2
    printf '%s\n' "---- 结束 ----" >&2
  fi
}

run_engine "PostgreSQL" "$PG_BIN" "postgres://${DB_USER}:${DB_PASSWORD}@localhost:5432/${DB_NAME}" \
  FRZ_TEST_POSTGRES_DSN TestPostgresAdapterContract
run_engine "MySQL" "$MYSQL_BIN" "mysql://${DB_USER}:${DB_PASSWORD}@127.0.0.1:3306/${DB_NAME}" \
  FRZ_TEST_MYSQL_DSN TestMySQLAdapterContract
run_engine "MariaDB" "$MARIA_BIN" "mysql://${DB_USER}:${DB_PASSWORD}@127.0.0.1:3306/${DB_NAME}" \
  FRZ_TEST_MARIADB_DSN TestMariaDBAdapterContract

# ==== 临时库有没有被收尾 ====
#
# 合约只看适配器的返回值；「隔离恢复用过的临时库有没有真的从实例上消失」要直接
# 问实例。留下来的临时库会一直占着对端磁盘，而且只有在这个层面才看得见。
log "隔离恢复的临时库必须被收尾"

leftovers=$("$DOCKER" exec -i "$PG" psql -U postgres -tAc \
  "SELECT count(*) FROM pg_database WHERE datname LIKE 'frz_restore_%'" | tr -d '[:space:]')
if [ "$leftovers" = "0" ]; then
  pass "PostgreSQL 上没有残留的临时库"
else
  fail "PostgreSQL 上残留了 ${leftovers} 个临时库"
fi

for pair in "${MYSQL}:mysql" "${MARIA}:mariadb"; do
  container=${pair%%:*}
  client=${pair##*:}
  leftovers=$("$DOCKER" exec -i "$container" "$client" -uroot -p"$DB_PASSWORD" -N -B \
    -e "SELECT count(*) FROM information_schema.schemata WHERE schema_name LIKE 'frz_restore_%'" 2>/dev/null |
    tr -d '[:space:]')
  if [ "$leftovers" = "0" ]; then
    pass "${container} 上没有残留的临时库"
  else
    fail "${container} 上残留了 ${leftovers} 个临时库"
  fi
done

printf '\n%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
[ "$FAIL_COUNT" -eq 0 ]
