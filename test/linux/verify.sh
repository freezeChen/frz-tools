#!/usr/bin/env bash
#
# Linux 容器验证 harness。
#
# 在真实 Linux 内核上验证那些在 macOS 上无法证明的行为：文件模式、用户属组、
# Unix Socket ACL，以及 systemd 作为 PID 1 的 unit 生命周期。
#
# 用法：
#   bash test/linux/verify.sh               # 复用已有镜像
#   bash test/linux/verify.sh --rebuild     # 强制重建镜像
#
# 证据类型：Linux 容器。它与「Linux 主机」是两类不同证据，不得混用——
# 真实 reboot 后的 unit 持久化、SELinux/AppArmor、sudoers/PAM 实际策略仍需真实主机验证。

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
IMAGE=${FRZ_VERIFY_IMAGE:-frz-ops-verify:24.04}
WORK_DIR=$(mktemp -d)
CID=""

PASS_COUNT=0
FAIL_COUNT=0

log()  { printf '\n--- %s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*" >&2; }

cleanup() {
  if [ -n "$CID" ]; then
    docker rm -f "$CID" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

in_container() { docker exec -i "$CID" "$@"; }

# 查询类命令：失败时返回空串，交给断言报错，避免 set -e 提前中断。
q() { in_container "$@" 2>/dev/null || true; }

assert_eq() { # 描述 期望 实际
  if [ "$2" = "$3" ]; then
    pass "$1 = $3"
  else
    fail "$1：期望 $2，实际 ${3:-<空>}"
  fi
}

require_ok() { # 描述 命令...
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then
    pass "$desc"
  else
    fail "$desc"
  fi
}

require_fail() { # 描述 命令...
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then
    fail "${desc}（命令意外成功）"
  else
    pass "$desc"
  fi
}

wait_for_systemd() {
  local state=""
  for _ in $(seq 1 60); do
    state=$(docker exec "$CID" systemctl is-system-running 2>/dev/null || true)
    case "$state" in
      running | degraded) return 0 ;;
    esac
    sleep 0.5
  done
  printf 'systemd 未能就绪，最后状态：%s\n' "${state:-未知}" >&2
  docker logs "$CID" 2>&1 | tail -20 >&2 || true
  exit 1
}

wait_for_socket() {
  for _ in $(seq 1 60); do
    if docker exec "$CID" test -S /run/opsd/opsd.sock 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  printf 'opsd 未创建 socket\n' >&2
  docker exec "$CID" tail -20 /var/log/opsd/opsd.log >&2 || true
  exit 1
}

wait_for_status() { # operation-id 期望状态
  local id=$1 want=$2 status=""
  for _ in $(seq 1 60); do
    status=$(in_container runuser -u frz-ops -- \
      /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock operation get "$id" --json 2>/dev/null |
      sed -n 's/.*"status": *"\([^"]*\)".*/\1/p' || true)
    if [ "$status" = "$want" ]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

build_image() {
  if [ "${1:-}" != "--rebuild" ] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
    log "复用镜像 $IMAGE"
    return 0
  fi
  log "构建镜像 $IMAGE"
  docker build -t "$IMAGE" -f "$REPO_ROOT/test/linux/Dockerfile.systemd" "$REPO_ROOT/test/linux"
}

build_binaries() { # GOARCH
  log "交叉编译 linux/$1 二进制"
  CGO_ENABLED=0 GOOS=linux GOARCH="$1" go build -C "$REPO_ROOT" -o "$WORK_DIR/opsd" ./cmd/opsd
  CGO_ENABLED=0 GOOS=linux GOARCH="$1" go build -C "$REPO_ROOT" -o "$WORK_DIR/opsctl" ./cmd/opsctl
}

start_container() {
  log "启动 systemd 容器"
  # --cgroupns=host 是必需的：private 下 systemd 无法作为 PID 1 启动。
  CID=$(docker run -d --privileged --cgroupns=host \
    --tmpfs /run --tmpfs /tmp \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    "$IMAGE")
  wait_for_systemd
}

provision() {
  log "准备专用用户、目录与配置"
  in_container mkdir -p /opt/frz-ops
  docker cp "$WORK_DIR/opsd" "$CID:/opt/frz-ops/opsd"
  docker cp "$WORK_DIR/opsctl" "$CID:/opt/frz-ops/opsctl"
  in_container chmod 0755 /opt/frz-ops/opsd /opt/frz-ops/opsctl
  # 不能复制到 /tmp：--tmpfs /tmp 会遮住容器根文件系统里的同名目录，
  # docker cp 看似成功，docker exec 却看不到文件。
  docker cp "$REPO_ROOT/test/linux/opsd.verify.yaml" "$CID:/opt/frz-ops/opsd.verify.yaml"

  in_container groupadd --system frz-ops
  in_container useradd --system -g frz-ops --no-create-home --home-dir /var/lib/opsd frz-ops
  in_container useradd --system frz-other
  in_container install -d -m 0750 -o frz-ops -g frz-ops /etc/opsd /var/lib/opsd /var/log/opsd
  in_container install -d -m 0755 -o frz-ops -g frz-ops /run/opsd
  in_container install -m 0600 -o frz-ops -g frz-ops /opt/frz-ops/opsd.verify.yaml /etc/opsd/config.yaml
}

check_systemd() {
  log "systemd 作为 PID 1"
  assert_eq "PID 1 进程名" "systemd" "$(q ps -p 1 -o comm= | tr -d ' ')"

  docker exec -i "$CID" sh -c 'cat > /etc/systemd/system/frz-verify.service' <<'UNIT'
[Unit]
Description=frz-tools verification unit

[Service]
Type=oneshot
ExecStart=/bin/true
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT

  require_ok "daemon-reload" docker exec -i "$CID" systemctl daemon-reload
  require_ok "start unit" docker exec -i "$CID" systemctl start frz-verify.service
  require_ok "enable unit" docker exec -i "$CID" systemctl enable frz-verify.service
  assert_eq "unit 运行状态" "active" "$(q systemctl is-active frz-verify.service)"
  assert_eq "unit 开机自启" "enabled" "$(q systemctl is-enabled frz-verify.service)"
}

check_filesystem() {
  log "文件模式与属主"
  assert_eq "/etc/opsd 目录模式" "750" "$(q stat -c '%a' /etc/opsd)"
  assert_eq "配置文件模式" "600" "$(q stat -c '%a' /etc/opsd/config.yaml)"
  assert_eq "配置文件属主" "frz-ops" "$(q stat -c '%U' /etc/opsd/config.yaml)"
  assert_eq "socket 模式" "660" "$(q stat -c '%a' /run/opsd/opsd.sock)"
  assert_eq "socket 属主" "frz-ops" "$(q stat -c '%U' /run/opsd/opsd.sock)"
  assert_eq "socket 属组" "frz-ops" "$(q stat -c '%G' /run/opsd/opsd.sock)"
  assert_eq "work 目录模式" "750" "$(q stat -c '%a' /var/lib/opsd/work)"

  log "SQLite 落盘"
  assert_eq "数据库文件模式" "600" "$(q stat -c '%a' /var/lib/opsd/opsd.db)"
  assert_eq "WAL 文件模式" "600" "$(q stat -c '%a' /var/lib/opsd/opsd.db-wal)"
  require_ok "WAL 文件存在" docker exec -i "$CID" test -f /var/lib/opsd/opsd.db-wal
}

check_socket_acl() {
  log "Unix Socket 访问控制"
  require_ok "属主 frz-ops 可以访问" \
    docker exec -i "$CID" runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock health

  require_fail "非属组 frz-other 被拒绝" \
    docker exec -i "$CID" runuser -u frz-other -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock health

  require_ok "以属组身份访问可以通过" \
    docker exec -i "$CID" runuser -u frz-other -g frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock health
}

check_executor() {
  log "执行器在 Linux 上真实执行"

  local stdout id
  stdout=$(in_container runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock operation submit \
    --kind executor.command --resource verify-linux --json -- /bin/echo linux-ok)
  id=$(printf '%s' "$stdout" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')

  if [ -z "$id" ]; then
    fail "无法从提交响应解析 operation id"
    return 0
  fi
  pass "提交操作成功（${id}）"

  if wait_for_status "$id" "succeeded"; then
    pass "操作执行成功"
  else
    fail "操作未在超时内达到 succeeded"
  fi

  local logs
  logs=$(in_container runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock operation logs "$id" 2>/dev/null || true)
  if printf '%s' "$logs" | grep -q 'linux-ok'; then
    pass "日志包含捕获的 stdout"
  else
    fail "日志缺少捕获的 stdout"
  fi
}

main() {
  command -v docker >/dev/null 2>&1 || {
    printf '需要 docker\n' >&2
    exit 1
  }

  local arch
  arch=$(docker version --format '{{.Server.Arch}}')
  log "容器架构：$arch"

  build_image "${1:-}"
  build_binaries "$arch"
  start_container
  provision

  log "以专用系统用户 frz-ops 启动 opsd"
  docker exec -d -u frz-ops "$CID" /opt/frz-ops/opsd --config /etc/opsd/config.yaml
  wait_for_socket

  check_filesystem
  check_socket_acl
  check_executor
  check_systemd

  log "结果"
  printf '%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
  [ "$FAIL_COUNT" -eq 0 ] || exit 1
}

main "$@"
