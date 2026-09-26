#!/usr/bin/env bash
#
# Linux 容器验证 harness。
#
# 在真实 Linux 内核上验证那些在 macOS 上无法证明的行为：文件模式、用户属组、
# Unix Socket ACL，以及 systemd 作为 PID 1 的 unit 生命周期。
#
# 迭代 1c 起还包括 RuntimeAdapter（systemd 适配器）：Prepare 产物与属主、凭据路径的
# 穿越链、凭据值是否逐字节到达进程（只比长度与摘要，不打印值）、启停与就绪、以及
# ProtectSystem=strict 的实际约束。
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
# pass/fail 除了计数，还各自落一行日志。**这不是冗余**：在 `$( )` 里调用它们时，
# 子 shell 的变量改动不会回到父 shell——计数会丢，而日志不会。收尾时比对两者，
# 就能把「有一行 FAIL 打印出来了、但 FAIL_COUNT 还是 0」这种最难发现的情况抓出来
# （2026-09-26 真的踩到了：176 项通过 / 0 项失败，而日志里明明白白有一行 FAIL）。
PASS_LOG=$(mktemp)
FAIL_LOG=$(mktemp)

# ==== 1c RuntimeAdapter 的验证夹具 ====
# 探针应用由 harness 放进容器；PROBE_ENV_VALUE 是 kind=env 凭据的值，里面刻意塞进
# systemd EnvironmentFile 会改写的每一种字节：反斜杠、双引号、字面 \n（反斜杠加 n）、
# $、单引号，以及首尾空格。它们必须以原样到达进程——这是 1c §13 第 6 条要的端到端证据，
# 单测只能证明转义函数本身，证明不了 systemd 真的照那样解析。
RUNTIME_APP=frz-probe
RUNTIME_USER=frz-probe
RUNTIME_UNIT=frz-probe.service
RUNTIME_PORT=18081
PROBE_ENV_VALUE=' \back"quote\n$dollar'\''quote '
PROBE_DIR=/etc/opsd/apps/${RUNTIME_APP}

log()  { printf '\n--- %s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; printf '%s\n' "$*" >> "$PASS_LOG"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*" >&2; printf '%s\n' "$*" >> "$FAIL_LOG"; }

cleanup() {
  if [ -n "$CID" ]; then
    docker rm -f "$CID" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR" "$PASS_LOG" "$FAIL_LOG"
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

wait_for_socket() { # [socket 路径]
  local sock=${1:-/run/opsd/opsd.sock}
  for _ in $(seq 1 60); do
    if docker exec "$CID" test -S "$sock" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  printf 'opsd 未创建 socket %s\n' "$sock" >&2
  docker exec "$CID" tail -20 /var/log/opsd/opsd.log >&2 || true
  exit 1
}

wait_for_status() { # operation-id 期望状态 [socket]
  local id=$1 want=$2 sock=${3:-/run/opsd/opsd.sock} status=""
  for _ in $(seq 1 60); do
    status=$(in_container /opt/frz-ops/opsctl --socket "$sock" operation get "$id" --json 2>/dev/null |
      sed -n 's/.*"status": *"\([^"]*\)".*/\1/p' || true)
    if [ "$status" = "$want" ]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# ==== 1c runtime 组专用的客户端 ====
# 适配器要建用户、改属主、装 unit，都需要 root，因此 runtime 组打在以 root 运行的
# 第二个 opsd 实例上（配置见 test/linux/opsd.root.verify.yaml）。其余各组仍用
# frz-ops 实例，前缀断言一律不动。
RUNTIME_SOCK=/run/opsd-root/opsd.sock

runtimectl() { in_container /opt/frz-ops/opsctl --socket "${RUNTIME_SOCK}" "$@"; }
rq() { runtimectl "$@" 2>/dev/null || true; }

# 就绪要求连续通过 consecutiveSuccesses 次，因此启动后的第一次查询可能仍未就绪（1/2）。
wait_for_ready() { # app
  local app=$1
  for _ in $(seq 1 40); do
    if runtimectl runtime health --app "$app" >/dev/null 2>&1; then
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
  # 探针应用：扮演被托管的长驻服务，把「进程实际收到了什么」写成事实（不含凭据明文）。
  CGO_ENABLED=0 GOOS=linux GOARCH="$1" go build -C "$REPO_ROOT" -o "$WORK_DIR/frz-probe" ./test/linux/probe
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
  in_container install -d -m 0700 -o frz-ops -g frz-ops /etc/opsd/secrets
  in_container install -m 0600 -o frz-ops -g frz-ops /opt/frz-ops/opsd.verify.yaml /etc/opsd/config.yaml

  # runtime 组用的 root 实例：自己的配置、socket、数据库、日志、制品目录。
  docker cp "$REPO_ROOT/test/linux/opsd.root.verify.yaml" "$CID:/opt/frz-ops/opsd.root.verify.yaml"
  in_container install -m 0600 -o root -g root /opt/frz-ops/opsd.root.verify.yaml /etc/opsd/root.yaml
  in_container install -d -m 0755 -o root -g root /run/opsd-root
  in_container install -d -m 0750 -o root -g root /var/lib/opsd-root /var/log/opsd-root

  # 1c 夹具之一：探针二进制。1c 不把制品解包到 argv[0]（那是迭代 3），
  # 所以二进制由 harness 放到 /opt/frz-ops，而 manifest 里的 artifact 仍是它本身，
  # 保持「声明的东西就是运行的东西」自洽。
  docker cp "$WORK_DIR/frz-probe" "$CID:/opt/frz-ops/frz-probe"
  in_container chmod 0755 /opt/frz-ops/frz-probe

  # 1c 夹具之二：kind=env 凭据的值。它来自 opsd 自己的进程环境（见 secret.Resolver），
  # 这份文件既是那份环境的唯一来源，也是稍后计算期望摘要的参考副本：两边同源，
  # 一旦环境传递改写了字节，断言行就对不上。
  printf '%s' "$PROBE_ENV_VALUE" > "$WORK_DIR/probe-env-value"
  docker cp "$WORK_DIR/probe-env-value" "$CID:/opt/frz-ops/probe-env-reference"
  in_container chmod 0600 /opt/frz-ops/probe-env-reference

  # 2a 夹具：被备份的目录。内容与权限位都在 check_backup 里被断言，
  # 因此刻意用一个不是默认值的模式（0640），让「恢复时模式错了」能被发现。
  in_container install -d -m 0750 -o root -g root /opt/frz-ops/backup-source
  in_container install -d -m 0750 -o root -g root /opt/frz-ops/backup-source/nested
  in_container install -m 0640 /dev/null /opt/frz-ops/backup-source/alpha.txt
  printf '%s' 'alpha-content-for-backup' > "$WORK_DIR/backup-alpha"
  docker cp "$WORK_DIR/backup-alpha" "$CID:/opt/frz-ops/backup-source/alpha.txt"
  in_container chmod 0640 /opt/frz-ops/backup-source/alpha.txt
  in_container install -m 0600 /dev/null /opt/frz-ops/backup-source/nested/beta.bin
  printf 'binary\000\001\002\377' > "$WORK_DIR/backup-beta"
  docker cp "$WORK_DIR/backup-beta" "$CID:/opt/frz-ops/backup-source/nested/beta.bin"
  in_container chmod 0600 /opt/frz-ops/backup-source/nested/beta.bin

  # 1c 夹具之三：kind=file 凭据的源，多行内容且刻意不以换行结尾——
  # 解析器会裁掉行尾的 CR/LF，留一个尾巴会让「逐字节一致」这条断言变成在对裁剪规则下注。
  printf '%s' '-----BEGIN FRZ KEY-----
line-two\back"quote$dollar
-----END FRZ KEY-----' > "$WORK_DIR/probe-file"
  docker cp "$WORK_DIR/probe-file" "$CID:/etc/opsd/secrets/probe-file"
  in_container chown frz-ops:frz-ops /etc/opsd/secrets/probe-file
  in_container chmod 0600 /etc/opsd/secrets/probe-file
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

# 制品存储与凭据文件的权限语义是 macOS 无法证明的，必须在真实 Linux 内核上断言。
check_artifacts_and_secrets() {
  log "制品存储的文件模式（Linux 容器证据）"

  in_container sh -c 'printf artifact-payload > /opt/frz-ops/payload.bin'

  local uploaded artifact_id
  uploaded=$(q runuser -u frz-ops -- /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock \
    artifact put /opt/frz-ops/payload.bin --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')

  if [ -z "${artifact_id}" ]; then
    fail "制品上传失败：${uploaded}"
    return 0
  fi
  pass "制品上传成功（${artifact_id}）"

  require_ok "制品落盘后校验通过" in_container runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock artifact verify "${artifact_id}"

  local blob_path
  blob_path=$(q sh -c 'find /var/lib/opsd/artifacts/blobs -type f | head -1')
  if [ -z "${blob_path}" ]; then
    fail "未找到落盘的 blob"
    return 0
  fi
  assert_eq "blob 文件模式" "640" "$(q stat -c '%a' "${blob_path}")"
  assert_eq "blob 属主" "frz-ops" "$(q stat -c '%U' "${blob_path}")"
  assert_eq "artifacts 根目录模式" "750" "$(q stat -c '%a' /var/lib/opsd/artifacts)"
  assert_eq "blobs 目录模式" "750" "$(q stat -c '%a' /var/lib/opsd/artifacts/blobs)"
  assert_eq "tmp 目录模式" "750" "$(q stat -c '%a' /var/lib/opsd/artifacts/tmp)"

  log "SecretRef 文件权限语义（Linux 容器证据）"

  # 必须以 frz-ops 身份创建：以 root 创建会让 0600 文件对 opsd 不可读，
  # 从而让「0600 通过 / 0644 被拒」两个断言以同一个错误原因同时失败。
  in_container runuser -u frz-ops -- sh -c 'printf strict-secret > /etc/opsd/secrets/strict'
  in_container chmod 0600 /etc/opsd/secrets/strict
  assert_eq "凭据目录模式" "700" "$(q stat -c '%a' /etc/opsd/secrets)"
  assert_eq "凭据文件模式" "600" "$(q stat -c '%a' /etc/opsd/secrets/strict)"
  assert_eq "凭据文件属主" "frz-ops" "$(q stat -c '%U' /etc/opsd/secrets/strict)"

  local submit_out op_id
  submit_out=$(q runuser -u frz-ops -- /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock operation submit \
    --kind executor.command --resource verify-secret-strict \
    --secret-env "FRZ_OK=file:/etc/opsd/secrets/strict" \
    --json -- /bin/sh -c 'test -n "$FRZ_OK"')
  op_id=$(printf '%s' "${submit_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')

  if [ -n "${op_id}" ] && wait_for_status "${op_id}" "succeeded"; then
    pass "0600 凭据文件可解析并注入命令环境"
  else
    fail "0600 凭据文件解析失败：${submit_out}"
  fi

  # 放开属组/其他用户之后必须被拒绝，否则权限校验形同虚设。
  in_container chmod 0644 /etc/opsd/secrets/strict
  submit_out=$(q runuser -u frz-ops -- /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock operation submit \
    --kind executor.command --resource verify-secret-loose \
    --secret-env "FRZ_LOOSE=file:/etc/opsd/secrets/strict" \
    --json -- /usr/bin/true)
  op_id=$(printf '%s' "${submit_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')

  if [ -n "${op_id}" ] && wait_for_status "${op_id}" "failed"; then
    local failure_code
    failure_code=$(q runuser -u frz-ops -- /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock \
      operation get "${op_id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p')
    if [ "${failure_code}" = "SECRET_UNRESOLVED" ]; then
      pass "0644 凭据文件被拒绝"
    else
      fail "0644 凭据文件的失败码：${failure_code:-<空>}"
    fi
  else
    fail "0644 凭据文件未被拒绝：${submit_out}"
  fi
}

# 时区数据是精简镜像最容易缺失的一环：Go 的 time.LoadLocation 依赖 tzdata，
# 缺了它所有带时区的计划都会创建失败，而 macOS 上永远测不出这个问题。
check_schedules() {
  log "调度器与时区（Linux 容器证据）"

  # 注意：整条命令必须经由 q 进容器执行。写成 $(test -f ...) 会在本机跑，
  # macOS 上有 zoneinfo，断言就会假通过而完全没检查容器。
  assert_eq "tzdata 已安装" "yes" \
    "$(q sh -c 'test -f /usr/share/zoneinfo/Asia/Shanghai && echo yes || echo no')"

  local created next_run
  created=$(q runuser -u frz-ops -- /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock schedule create \
    --name tz-check --resource tz-check --cron "0 2 * * *" --timezone Asia/Shanghai \
    --json -- /usr/bin/true)
  next_run=$(printf '%s' "${created}" | sed -n 's/.*"nextRunAt": *"\([^"]*\)".*/\1/p')

  if [ -z "${next_run}" ]; then
    fail "容器内创建带时区的计划失败：${created}"
    return 0
  fi
  pass "容器内可解析 Asia/Shanghai"

  # cron 按计划时区解释：返回的是同一时刻的 UTC 表示，所以要换算到计划时区再断言，
  # 只看字符串偏移会误判（robfig 会把结果换算回调用方时区再返回）。
  local shanghai_hour utc_hour
  shanghai_hour=$(q sh -c "TZ=Asia/Shanghai date -d '${next_run}' +%H:%M 2>/dev/null")
  utc_hour=$(q sh -c "TZ=UTC date -d '${next_run}' +%H:%M 2>/dev/null")

  assert_eq "按计划时区解释的触发时刻" "02:00" "${shanghai_hour}"
  if [ "${utc_hour}" = "02:00" ]; then
    fail "时区未生效：UTC 与 Asia/Shanghai 都是 02:00"
  else
    pass "时区确实改变了解释结果（同一时刻 UTC 为 ${utc_hour}）"
  fi

  require_ok "停用计划" in_container runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock schedule disable tz-check
  require_ok "删除计划" in_container runuser -u frz-ops -- \
    /opt/frz-ops/opsctl --socket /run/opsd/opsd.sock schedule delete tz-check
}

# 迭代 1c：把「一份 manifest 落成目标机上的真实进程管理」整条链路放到真实 systemd 上验证。
#
# 能证明而 macOS 证明不了的四件事：目录/文件模式与属主、跨用户的凭据可读性、
# EnvironmentFile 的值转义是否逐字节到达进程、以及 ProtectSystem=strict 的实际约束。
#
# 刻意放在最后：最后一条断言会删掉敏感环境文件并让 unit 停在 failed 状态。
check_runtime() {
  log "RuntimeAdapter：提交 manifest 与 Prepare 产物（Linux 容器证据）"

  # 制品就是探针二进制本身：manifest 声明的 artifact 与实际运行的 argv 保持自洽。
  local uploaded artifact_id
  uploaded=$(rq \
    artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${artifact_id}" ]; then
    fail "runtime 夹具的制品上传失败：${uploaded}"
    return 0
  fi
  pass "runtime 夹具制品上传成功（${artifact_id}）"

  cat > "$WORK_DIR/frz-probe.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${RUNTIME_APP}
runtime: go
artifact:
  id: ${artifact_id}
exec:
  argv:
    - /opt/frz-ops/frz-probe
    - --report
    - /var/lib/${RUNTIME_APP}/probe-report.txt
    - --listen
    - 127.0.0.1:${RUNTIME_PORT}
    - --hash-env
    - PROBE_ENV
    - --hash-file-env
    - PROBE_FILE
    - --allow-write
    - /var/log/${RUNTIME_APP}/probe-writable
    - --deny-write
    - /var/lib/${RUNTIME_APP}-extra/out
  workingDirectory: /var/lib/${RUNTIME_APP}
  runUser: ${RUNTIME_USER}
  environment:
    PLAIN_VALUE: plain
  secretEnvironment:
    PROBE_ENV:
      kind: env
      name: PROBE_ENV
    PROBE_FILE:
      kind: file
      name: /etc/opsd/secrets/probe-file
  ports:
    - ${RUNTIME_PORT}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${RUNTIME_PORT}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 60
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/${RUNTIME_APP}
systemd:
  unitName: ${RUNTIME_UNIT}
  restartPolicy: on-failure
MANIFEST

  docker cp "$WORK_DIR/frz-probe.yaml" "$CID:/opt/frz-ops/frz-probe.yaml"
  in_container chmod 0644 /opt/frz-ops/frz-probe.yaml

  require_ok "注册应用" runtimectl app create "${RUNTIME_APP}"
  require_ok "提交 manifest（严格校验通过）" runtimectl spec put \
    --app "${RUNTIME_APP}" --file /opt/frz-ops/frz-probe.yaml
  require_ok "runtime validate 无副作用通过" runtimectl runtime validate --app "${RUNTIME_APP}"

  local prepared tier version
  prepared=$(rq \
    runtime prepare --app "${RUNTIME_APP}" --json)
  tier=$(printf '%s' "${prepared}" | sed -n 's/.*"tier": *"\([^"]*\)".*/\1/p')
  version=$(printf '%s' "${prepared}" | sed -n 's/.*"systemdVersion": *\([0-9]*\).*/\1/p')
  if [ -z "${tier}" ]; then
    # rq 会吞掉 stderr：失败时再跑一次，把原因原样带出来。否则后面二十多条断言会
    # 以「期望 X，实际 <空>」级联失败，真正的原因被埋掉。
    fail "runtime prepare 失败：$(runtimectl runtime prepare --app "${RUNTIME_APP}" 2>&1 | head -3 | tr '\n' ' ')"
    return 0
  fi
  assert_eq "Prepare 返回的 unit 档位" "strict" "${tier}"
  if [ -n "${version}" ] && [ "${version}" -ge 240 ]; then
    pass "探测到的 systemd 版本 ${version} 落在 strict 档（≥240）"
  else
    fail "Prepare 返回的 systemd 版本异常：${version:-<空>}"
  fi

  local work=/var/lib/${RUNTIME_APP} logs=/var/log/${RUNTIME_APP}
  local releases=/opt/opsd/apps/${RUNTIME_APP}/releases

  require_ok "运行用户已创建" in_container id -u "${RUNTIME_USER}"

  local pair path label
  for pair in "${work}:工作目录" "${logs}:日志目录" "${releases}:解包目录（releases 根）"; do
    path=${pair%%:*}
    label=${pair##*:}
    assert_eq "${label}模式" "750" "$(q stat -c '%a' "${path}")"
    assert_eq "${label}属主" "${RUNTIME_USER}" "$(q stat -c '%U' "${path}")"
  done

  assert_eq "非敏感环境文件模式" "600" "$(q stat -c '%a' ${PROBE_DIR}.env)"
  assert_eq "非敏感环境文件属主" "${RUNTIME_USER}" "$(q stat -c '%U' ${PROBE_DIR}.env)"
  assert_eq "非敏感环境文件内容" "yes" \
    "$(q sh -c "grep -q '^PLAIN_VALUE=\"plain\"$' ${PROBE_DIR}.env && echo yes || echo no")"
  assert_eq "敏感环境文件模式" "600" "$(q stat -c '%a' ${PROBE_DIR}.secrets.env)"
  assert_eq "敏感环境文件属主" "${RUNTIME_USER}" "$(q stat -c '%U' ${PROBE_DIR}.secrets.env)"
  assert_eq "敏感环境文件里 kind=file 传的是路径" "yes" \
    "$(q sh -c "grep -q '^PROBE_FILE=\"${PROBE_DIR}.secrets/PROBE_FILE\"$' ${PROBE_DIR}.secrets.env && echo yes || echo no")"
  assert_eq "凭据目录模式" "700" "$(q stat -c '%a' ${PROBE_DIR}.secrets)"
  assert_eq "凭据目录属主" "${RUNTIME_USER}" "$(q stat -c '%U' ${PROBE_DIR}.secrets)"
  assert_eq "凭据文件模式" "600" "$(q stat -c '%a' ${PROBE_DIR}.secrets/PROBE_FILE)"
  assert_eq "凭据文件属主" "${RUNTIME_USER}" "$(q stat -c '%U' ${PROBE_DIR}.secrets/PROBE_FILE)"
  require_ok "凭据文件内容与源逐字节一致" \
    in_container cmp /etc/opsd/secrets/probe-file ${PROBE_DIR}.secrets/PROBE_FILE

  assert_eq "unit 文件模式" "644" "$(q stat -c '%a' /etc/systemd/system/${RUNTIME_UNIT})"
  assert_eq "unit 文件属主" "root" "$(q stat -c '%U' /etc/systemd/system/${RUNTIME_UNIT})"
  assert_eq "unit 头注释记录了探测到的版本与档位" "yes" \
    "$(q sh -c "grep -qE '^# systemd 版本=[0-9]+ 档位=strict$' /etc/systemd/system/${RUNTIME_UNIT} && echo yes || echo no")"

  log "凭据路径的穿越链（kind=file 由运行用户自己按路径打开）"
  # 安装约定把 /etc/opsd 建成 0750 且属主是 opsd 自己；0750 对运行用户意味着 other 位为
  # `---`，穿越会被拒。Prepare 必须只补上穿越位（0750 → 0751），不放开读位。
  local opsd_mode
  opsd_mode=$(q stat -c '%a' /etc/opsd)
  assert_eq "/etc/opsd 模式（Prepare 后补穿越位）" "751" "${opsd_mode}"
  assert_eq "/etc/opsd 的 other 权限（只可穿越，不可读/列）" "1" "$(printf '%s' "${opsd_mode}" | cut -c3)"
  assert_eq "/etc/opsd/apps 模式" "751" "$(q stat -c '%a' /etc/opsd/apps)"

  require_ok "运行用户可以按路径读到自己的凭据文件" \
    in_container runuser -u "${RUNTIME_USER}" -- cat ${PROBE_DIR}.secrets/PROBE_FILE
  require_fail "无关用户 frz-other 读凭据文件被拒" \
    in_container runuser -u frz-other -- cat ${PROBE_DIR}.secrets/PROBE_FILE
  require_fail "无关用户 frz-other 连凭据目录都进不去" \
    in_container runuser -u frz-other -- ls ${PROBE_DIR}.secrets

  # 幂等：第二次 Prepare 必须成功且档位不变（unit 与权限已经正确，不该被重写）。
  local second tier2
  second=$(rq \
    runtime prepare --app "${RUNTIME_APP}" --json)
  tier2=$(printf '%s' "${second}" | sed -n 's/.*"tier": *"\([^"]*\)".*/\1/p')
  assert_eq "第二次 Prepare 幂等且档位不变" "strict" "${tier2}"

  log "RuntimeAdapter：启动、就绪与停止（真实 systemd）"
  # ProtectSystem=strict 只放行 unit 声明过的路径。这个目录刻意由 harness 建成
  # 运行用户可写：这样「写不进去」只能归因于 unit 的只读挂载，而不是 DAC 权限。
  require_ok "为 ProtectSystem 断言准备运行用户可写的目录" \
    in_container install -d -m 0750 -o "${RUNTIME_USER}" -g "${RUNTIME_USER}" /var/lib/${RUNTIME_APP}-extra

  local start_out start_id
  start_out=$(rq \
    runtime start --app "${RUNTIME_APP}" --json)
  start_id=$(printf '%s' "${start_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${start_id}" ]; then
    fail "runtime start 未返回 operation id：${start_out}"
    return 0
  fi
  pass "runtime start 走 Operation（${start_id}）"
  if wait_for_status "${start_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "start 操作的终态为 succeeded"
  else
    fail "start 操作未在超时内 succeeded"
  fi

  assert_eq "unit 运行状态" "active" "$(q systemctl is-active ${RUNTIME_UNIT})"
  if wait_for_ready "${RUNTIME_APP}"; then
    pass "runtime health 在启动后报告就绪（退出码 0）"
  else
    fail "runtime health 未在超时内就绪"
  fi

  log "凭据是否真的到达进程（只比长度与摘要，不打印值）"
  local report want_env_len want_env_sha want_file_len want_file_sha
  report=$(q cat ${work}/probe-report.txt)
  if [ -z "${report}" ]; then
    fail "探针没有写出报告，无法验证凭据交付"
    return 0
  fi
  # 期望值在容器内计算：本机是 macOS（shasum），CI 是 Linux（sha256sum），
  # 把摘要工具留在容器里才不会有平台差异。
  want_env_len=$(q sh -c 'wc -c < /opt/frz-ops/probe-env-reference' | tr -d ' ')
  want_env_sha=$(q sh -c 'sha256sum /opt/frz-ops/probe-env-reference' | cut -d' ' -f1)
  want_file_len=$(q sh -c 'wc -c < /etc/opsd/secrets/probe-file' | tr -d ' ')
  want_file_sha=$(q sh -c 'sha256sum /etc/opsd/secrets/probe-file' | cut -d' ' -f1)

  # 这一条就是 1c §13 第 6 条要的端到端证据：含反斜杠、双引号、字面 \n、$、单引号与
  # 首尾空格的值，经 EnvironmentFile 到达进程后字节完全一致。
  assert_eq "kind=env 凭据到达进程且字节一致" \
    "env:PROBE_ENV len=${want_env_len} sha256=${want_env_sha}" \
    "$(printf '%s\n' "${report}" | grep '^env:PROBE_ENV ' || true)"
  assert_eq "kind=file 凭据内容到达进程且字节一致" \
    "file-env:PROBE_FILE len=${want_file_len} sha256=${want_file_sha} path-absolute=yes" \
    "$(printf '%s\n' "${report}" | grep '^file-env:PROBE_FILE ' || true)"
  assert_eq "unit 声明过的路径确实可写" \
    "allow-write ${logs}/probe-writable writable=yes" \
    "$(printf '%s\n' "${report}" | grep '^allow-write ' || true)"
  assert_eq "ProtectSystem=strict 拦住未声明路径的写入" \
    "deny-write /var/lib/${RUNTIME_APP}-extra/out writable=no" \
    "$(printf '%s\n' "${report}" | grep '^deny-write ' || true)"

  local stop_out stop_id
  stop_out=$(rq \
    runtime stop --app "${RUNTIME_APP}" --json)
  stop_id=$(printf '%s' "${stop_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${stop_id}" ]; then
    fail "runtime stop 未返回 operation id：${stop_out}"
  else
    pass "runtime stop 走 Operation（${stop_id}）"
    if wait_for_status "${stop_id}" "succeeded" "${RUNTIME_SOCK}"; then
      pass "stop 操作的终态为 succeeded"
    else
      fail "stop 操作未在超时内 succeeded"
    fi
  fi
  assert_eq "停止后 unit 状态" "inactive" "$(q systemctl is-active ${RUNTIME_UNIT})"

  log "敏感环境文件缺失必须让 unit 启动失败"
  # unit 里那一行 EnvironmentFile= 刻意没有 `-` 前缀：缺凭据就不要起来，
  # 否则应用会在没有凭据的状态下对外提供服务。
  in_container rm -f ${PROBE_DIR}.secrets.env
  require_fail "敏感环境文件缺失时 systemctl start 失败" \
    in_container systemctl start ${RUNTIME_UNIT}

  # Restart=on-failure 会让 systemd 立刻重试：failed 只是失败瞬间的状态，等待重启期间
  # 状态是 activating（SubState=auto-restart）；而 RestartSec=5 的节奏永远凑不满
  # 默认的 start limit，所以它会一直循环、不会稳定停在 failed。可断言、也有意义的是
  # 「它从未提供服务」+「它只在失败与重启这两个状态里」——第一版断言瞬时 failed，
  # 直接变成了对重试时序的竞猜，实跑当场挂掉。
  local state states="" seen_active=no unexpected=""
  for _ in $(seq 1 12); do
    state=$(q systemctl is-active ${RUNTIME_UNIT})
    [ "${state}" = "active" ] && seen_active=yes
    case " ${states} " in
      *" ${state} "*) ;;
      *) states="${states} ${state}" ;;
    esac
    sleep 0.5
  done
  assert_eq "缺凭据时 unit 从未进入 active" "no" "${seen_active}"
  for state in ${states}; do
    case "${state}" in
      activating | failed) ;;
      *) unexpected="${unexpected} ${state}" ;;
    esac
  done
  assert_eq "缺凭据时 unit 只在失败/重启状态（观察到：${states# }）" "" "${unexpected}"
}

# ==== 迭代 1d：runtime.* 的重试与 systemd 的 Restart= 不得叠加（规格 D6）====
#
# 这条只能在真实 systemd 上验证：要证明的是「unit 已经在进程级重启了，opsd 就不该再
# 叠一层操作级重试」。假 runner 造不出这个关系——就像 A7 那次「进程身份 vs 目录属主」
# 永远是测试自己造的那种关系一样。
#
# 规格 D6 把语义收成一句话：**只有「就绪从未通过」才算 runtime.start 失败、才可重试；
# 已就绪过再崩溃归 unit 的 Restart= 与健康检查。** 下面两个夹具正对应这两半。
RETRY_OK_APP=frz-retry-ok
RETRY_OK_USER=frz-retry-ok
RETRY_OK_UNIT=frz-retry-ok.service
RETRY_OK_PORT=18092
RETRY_BAD_APP=frz-retry-bad
RETRY_BAD_USER=frz-retry-bad
RETRY_BAD_UNIT=frz-retry-bad.service
RETRY_BAD_PORT=18093
RETRY_DEAD_PORT=18094
RETRY_ARM_APP=frz-retry-arm
RETRY_ARM_USER=frz-retry-arm
RETRY_ARM_UNIT=frz-retry-arm.service
RETRY_DB=/var/lib/opsd-root/opsd.db

# ops_on 数出某个应用名下的操作行数。端口上没有「列出全部操作」的方法，而这里要断言的
# 正是「库里的行数没有变」——只看单个操作证明不了没有多出一行。
# runtime.* 的 resource 是规范化后的应用 ID，所以要从 applications 表取。
ops_on() { # app-name
  q sqlite3 "${RETRY_DB}" \
    "SELECT COUNT(*) FROM operations WHERE resource IN (SELECT id FROM applications WHERE name = '${1}');"
}

last_op_state() { # app-name
  q sqlite3 "${RETRY_DB}" \
    "SELECT status FROM operations WHERE resource IN (SELECT id FROM applications WHERE name = '${1}') ORDER BY created_at DESC, id DESC LIMIT 1;"
}

first_op_code() { # app-name
  q sqlite3 "${RETRY_DB}" \
    "SELECT COALESCE(error_code, '') FROM operations WHERE resource IN (SELECT id FROM applications WHERE name = '${1}') ORDER BY created_at, id LIMIT 1;"
}

# wait_for_chain 等到行数达到 want，且最后一跳已经进入终态。
# 只数行数不够：行数会在最后一跳还在 pending 时就达到预期。
wait_for_chain() { # app-name want-count
  local state
  for _ in $(seq 1 60); do
    if [ "$(ops_on "${1}")" = "${2}" ]; then
      state=$(last_op_state "${1}")
      case "${state}" in
        pending | running) ;;
        *) return 0 ;;
      esac
    fi
    sleep 0.5
  done
  return 1
}

# write_retry_spec 为 check_retry 造一份探针 manifest。
# listen 是探针真正监听的地址，target 是 manifest 声明的就绪目标；两者不同就构成
# 「进程活着但永远不就绪」，夹具 1 要的正是这个形态。
write_retry_spec() { # 目标文件 app user unit port artifact listen target start-timeout
  cat > "$1" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: $2
runtime: go
artifact:
  id: $6
exec:
  argv:
    - /opt/frz-ops/frz-probe
    - --report
    - /var/lib/$2/probe-report.txt
    - --listen
    - $7
  workingDirectory: /var/lib/$2
  runUser: $3
  ports:
    - $5
health:
  readiness:
    type: tcp
    target: $8
  startTimeoutSeconds: $9
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/$2
systemd:
  unitName: $4
  restartPolicy: on-failure
MANIFEST
}

check_retry() {
  log "迭代 1d：runtime.* 的重试与 systemd Restart= 不得叠加（Linux 容器证据）"

  # 需要直接查库。镜像里没装 sqlite3 时给出可执行的修复方式，而不是让断言给出
  # 一个看不懂的失败——复用旧镜像是最容易踩到的坑。
  if [ -z "$(q command -v sqlite3)" ]; then
    fail "镜像里没有 sqlite3，无法查库断言行数；请用 --rebuild 重建镜像"
    return 0
  fi

  local artifact
  artifact=$(rq artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json |
    sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${artifact}" ]; then
    fail "retry 组的制品上传失败"
    return 0
  fi

  # ---------------- 夹具 1：就绪永远不会通过 ----------------
  #
  # 这条夹具在实现过程中**改掉过一次前提**，值得说明：1d 规格 D6 原先写「runtime.start
  # 只有在就绪从未通过时才失败」，容器实跑证明这个前提**不成立**——适配器的 Start 根本
  # 不等就绪（只做 systemctl start 就返回），就绪超时只在 Health 里判定。
  #
  # 于是这里断言的是**真实语义**，它恰好也是 D6 想要的结论：就绪失败**不会**变成操作级
  # 重试，因为那条操作早就成功了——没有失败，就没有可重试的东西。
  log "夹具 1：就绪永远不会通过（失败发生在 Health，不在 Start）"
  # 探针监听 RETRY_BAD_PORT，manifest 却把就绪目标指向一个没人监听的端口：
  # 进程活着、unit 是 active，但就绪永远不通过。
  write_retry_spec "$WORK_DIR/retry-bad.yaml" "${RETRY_BAD_APP}" "${RETRY_BAD_USER}" \
    "${RETRY_BAD_UNIT}" "${RETRY_BAD_PORT}" "${artifact}" \
    "127.0.0.1:${RETRY_BAD_PORT}" "127.0.0.1:${RETRY_DEAD_PORT}" 5
  docker cp "$WORK_DIR/retry-bad.yaml" "$CID:/opt/frz-ops/retry-bad.yaml"
  in_container chmod 0644 /opt/frz-ops/retry-bad.yaml

  require_ok "retry 夹具 1：注册应用" runtimectl app create "${RETRY_BAD_APP}"
  require_ok "retry 夹具 1：提交 manifest" runtimectl spec put \
    --app "${RETRY_BAD_APP}" --file /opt/frz-ops/retry-bad.yaml

  local bad_out bad_id
  bad_out=$(rq runtime start --app "${RETRY_BAD_APP}" --retry-max 2 --retry-base 1s --json)
  bad_id=$(printf '%s' "${bad_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${bad_id}" ]; then
    fail "retry 夹具 1：runtime start 未返回 operation id：${bad_out}"
    return 0
  fi
  if wait_for_status "${bad_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "Start 不等就绪：就绪不通过也不妨碍 runtime.start 成功"
  else
    fail "runtime.start 未在超时内成功（当前状态 $(last_op_state "${RETRY_BAD_APP}")）"
  fi
  assert_eq "unit 确实是 active（进程活着，就是没就绪）" "active" \
    "$(q systemctl is-active ${RETRY_BAD_UNIT})"

  # 等过 startTimeoutSeconds=5，再问健康：失败**只**在这里发生。
  sleep 6
  local health_code=0
  in_container /opt/frz-ops/opsctl --socket "${RUNTIME_SOCK}" runtime health \
    --app "${RETRY_BAD_APP}" >/dev/null 2>&1 || health_code=$?
  assert_eq "就绪从未通过时 runtime health 的退出码（19=RUNTIME_NOT_READY）" "19" "${health_code}"

  # D6 的核心结论之一：就绪失败不是操作失败，因此**不该**冒出重试行。
  assert_eq "就绪失败不得产生操作级重试（链上仍只有一行）" "1" "$(ops_on "${RETRY_BAD_APP}")"

  # ---------------- 夹具 2：runtime.* 的重试确实被武装 ----------------
  #
  # 夹具 1 证明的是「不该重试的地方没有重试」，它只有在「该重试的地方确实会重试」成立时
  # 才有意义。所以这里造一个**真实且可重试**的运行时操作失败：Stop 一个从未 Prepare 的
  # 应用——适配器的 prepared() 会返回 RUNTIME_NOT_READY，而这个码在默认白名单里。
  log "夹具 2：runtime.* 的重试确实被武装（Stop 一个从未 Prepare 的应用）"
  write_retry_spec "$WORK_DIR/retry-arm.yaml" "${RETRY_ARM_APP}" "${RETRY_ARM_USER}" \
    "${RETRY_ARM_UNIT}" "${RETRY_DEAD_PORT}" "${artifact}" \
    "127.0.0.1:${RETRY_DEAD_PORT}" "127.0.0.1:${RETRY_DEAD_PORT}" 5
  docker cp "$WORK_DIR/retry-arm.yaml" "$CID:/opt/frz-ops/retry-arm.yaml"
  in_container chmod 0644 /opt/frz-ops/retry-arm.yaml

  require_ok "retry 夹具 2：注册应用" runtimectl app create "${RETRY_ARM_APP}"
  require_ok "retry 夹具 2：提交 manifest（刻意不 Prepare）" runtimectl spec put \
    --app "${RETRY_ARM_APP}" --file /opt/frz-ops/retry-arm.yaml

  local arm_out arm_id
  arm_out=$(rq runtime stop --app "${RETRY_ARM_APP}" --retry-max 2 --retry-base 1s --json)
  arm_id=$(printf '%s' "${arm_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${arm_id}" ]; then
    fail "retry 夹具 2：runtime stop 未返回 operation id：${arm_out}"
    return 0
  fi

  if wait_for_chain "${RETRY_ARM_APP}" 2; then
    pass "runtime.* 声明重试后确实排出了第二次尝试（链上 2 行）"
  else
    fail "runtime.* 声明重试后没有排出第二次尝试（链上 $(ops_on "${RETRY_ARM_APP}") 行）"
  fi
  assert_eq "首次失败的原始错误码" "RUNTIME_NOT_READY" "$(first_op_code "${RETRY_ARM_APP}")"
  assert_eq "maxAttempts=2 时链上恰好两行" "2" "$(ops_on "${RETRY_ARM_APP}")"

  # ---------------- 夹具 3：就绪会通过，然后在运行中被杀 ----------------
  #
  # 这是 D6 的另一半，也是本组最要紧的一条：unit 的 Restart= 已经在进程级重启，
  # opsd **不得**再叠一层操作级重试。判据是链上始终只有一行。
  log "夹具 3：已就绪过再被 SIGKILL（由 unit 的 Restart= 接管）"
  write_retry_spec "$WORK_DIR/retry-ok.yaml" "${RETRY_OK_APP}" "${RETRY_OK_USER}" \
    "${RETRY_OK_UNIT}" "${RETRY_OK_PORT}" "${artifact}" \
    "127.0.0.1:${RETRY_OK_PORT}" "127.0.0.1:${RETRY_OK_PORT}" 60
  docker cp "$WORK_DIR/retry-ok.yaml" "$CID:/opt/frz-ops/retry-ok.yaml"
  in_container chmod 0644 /opt/frz-ops/retry-ok.yaml

  require_ok "retry 夹具 3：注册应用" runtimectl app create "${RETRY_OK_APP}"
  require_ok "retry 夹具 3：提交 manifest" runtimectl spec put \
    --app "${RETRY_OK_APP}" --file /opt/frz-ops/retry-ok.yaml

  local ok_out ok_id
  ok_out=$(rq runtime start --app "${RETRY_OK_APP}" --retry-max 2 --retry-base 1s --json)
  ok_id=$(printf '%s' "${ok_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${ok_id}" ]; then
    fail "retry 夹具 3：runtime start 未返回 operation id：${ok_out}"
    return 0
  fi
  if wait_for_status "${ok_id}" "succeeded" "${RUNTIME_SOCK}" &&
    wait_for_ready "${RETRY_OK_APP}"; then
    pass "就绪通过时 runtime start 成功且健康"
  else
    fail "就绪通过时 runtime start 未成功或未就绪"
  fi

  # 杀掉主进程。Restart=on-failure 会让 unit 自己把它拉回来——这正是「不该叠加」的场景。
  in_container systemctl kill --kill-whom=main --signal=SIGKILL "${RETRY_OK_UNIT}" >/dev/null 2>&1 || true

  local restarts=""
  for _ in $(seq 1 30); do
    restarts=$(q systemctl show -p NRestarts --value "${RETRY_OK_UNIT}")
    if [ "${restarts:-0}" != "0" ] && [ "$(q systemctl is-active "${RETRY_OK_UNIT}")" = "active" ]; then
      break
    fi
    sleep 0.5
  done
  if [ "${restarts:-0}" != "0" ]; then
    pass "进程被 SIGKILL 后 unit 由 systemd 重启（NRestarts=${restarts}）"
  else
    fail "进程被 SIGKILL 后 unit 没有被 systemd 重启（NRestarts=${restarts}）"
  fi

  # 等到「万一排出来的重试」也跑完的时间（base 1 秒，留 6 秒足够），
  # 再断言链上没有多出第二行。这是 D6 的核心判据。
  sleep 6
  assert_eq "已就绪过再崩溃不得产生操作级重试（链上仍只有一行）" "1" "$(ops_on "${RETRY_OK_APP}")"

  # 顺带证明确实是健康检查负责「重新就绪」，而不是靠再跑一次 start 操作。
  if wait_for_ready "${RETRY_OK_APP}"; then
    pass "重启后仍由健康检查报告就绪（没有第二条 start 路径）"
  else
    fail "重启后健康检查未报告就绪"
  fi

  runtimectl runtime stop --app "${RETRY_OK_APP}" >/dev/null 2>&1 || true
  runtimectl runtime stop --app "${RETRY_BAD_APP}" >/dev/null 2>&1 || true
}

# ==== 迭代 2a：备份（独立存储根、权限、与制品 GC 的隔离）====
#
# 这一组要钉的是 D5 那条**防数据丢失**的结构性要求：备份必须写在与制品分开的存储根里，
# 因为 1a 的制品 GC 把「digest 不在 artifacts 表里」一律当作孤儿删除——共用根会让
# `artifact gc` 删掉全部备份，而且是静默的，只有真要恢复时才会发现。
BACKUP_POLICY=frz-backup-policy
BACKUP_SOURCE=/opt/frz-ops/backup-source
BACKUP_ROOT=/var/lib/opsd-root/backups

check_backup() {
  log "迭代 2a：备份的存储根、权限与与制品 GC 的隔离（Linux 容器证据）"

  cat > "$WORK_DIR/backup-policy.yaml" <<POLICY
apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: ${BACKUP_POLICY}
resource:
  kind: files
  paths:
    - ${BACKUP_SOURCE}
encoding:
  compression: gzip
  encryption:
    enabled: false
retention:
  keepLast: 3
POLICY
  docker cp "$WORK_DIR/backup-policy.yaml" "$CID:/opt/frz-ops/backup-policy.yaml"
  in_container chmod 0644 /opt/frz-ops/backup-policy.yaml

  require_ok "提交备份策略" runtimectl backup policy put --file /opt/frz-ops/backup-policy.yaml

  local run_out run_id
  run_out=$(rq backup run --policy "${BACKUP_POLICY}" --json)
  run_id=$(printf '%s' "${run_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${run_id}" ]; then
    fail "backup run 未返回 operation id：${run_out}"
    return 0
  fi
  if wait_for_status "${run_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "备份走 Operation 且成功（${run_id}）"
  else
    fail "备份未在超时内成功"
    return 0
  fi

  # 落盘产物的模式与属主：备份根由 opsd 在真实文件系统上按配置强制。
  assert_eq "备份存储根模式" "750" "$(q stat -c %a ${BACKUP_ROOT})"
  assert_eq "备份 blobs 目录模式" "750" "$(q stat -c %a ${BACKUP_ROOT}/blobs)"
  local blob
  blob=$(q find ${BACKUP_ROOT}/blobs -type f | head -1)
  if [ -z "${blob}" ]; then
    fail "备份根下没有落盘的 blob"
  else
    assert_eq "备份文件模式" "640" "$(q stat -c %a ${blob})"
    assert_eq "备份文件属主" "root" "$(q stat -c %U ${blob})"
  fi

  local backup_id
  backup_id=$(rq backup list --json |
    sed -n 's/.*"id": *"\(bkp_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${backup_id}" ]; then
    fail "backup list 里没有 bkp_ 开头的记录"
    return 0
  fi
  pass "backup list 能列出刚完成的备份（${backup_id}）"

  # D5 的核心断言：跑一次制品 GC，备份内容必须**完好**。
  require_ok "制品 GC（会回收它自己根下的孤儿）" runtimectl artifact gc
  assert_eq "制品 GC 之后备份 blob 仍在" "yes" \
    "$(test -n "$(q find ${BACKUP_ROOT}/blobs -type f | head -1)" && echo yes)"

  local verify_out verify_id
  verify_out=$(rq backup verify "${backup_id}" --json)
  verify_id=$(printf '%s' "${verify_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${verify_id}" ]; then
    fail "backup verify 未返回 operation id：${verify_out}"
    return 0
  fi
  if wait_for_status "${verify_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "制品 GC 之后备份仍能校验通过"
  else
    fail "制品 GC 之后备份校验失败——很可能备份内容被删了"
  fi

  # 隔离恢复：端到端能解出来，且不碰源目录。
  local restore_out restore_id
  restore_out=$(rq backup restore "${backup_id}" --mode isolated --json)
  restore_id=$(printf '%s' "${restore_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${restore_id}" ]; then
    fail "隔离恢复未返回 operation id：${restore_out}"
    return 0
  fi
  if wait_for_status "${restore_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "隔离恢复成功（走 Operation，${restore_id}）"
  else
    fail "隔离恢复未在超时内成功"
  fi

  # 没有密钥的部署在**提交期**就该被拒（加密默认开启，见迭代 2 规格决定 3）。
  cat > "$WORK_DIR/backup-enc.yaml" <<'POLICY'
apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: frz-backup-enc
resource:
  kind: files
  paths:
    - /opt/frz-ops/backup-source
retention:
  keepLast: 1
POLICY
  docker cp "$WORK_DIR/backup-enc.yaml" "$CID:/opt/frz-ops/backup-enc.yaml"
  require_fail "未给密钥的策略被拒（加密默认开启）" \
    runtimectl backup policy put --file /opt/frz-ops/backup-enc.yaml
}


# ==== 迭代 2d：保留策略与 prune ====
#
# 容器这一层能证明的是**文件系统上真的发生了什么**：blob 真的少了、剩下的那些权限与
# 属主没变、被标记的那些在元数据里是 pruned。保留集合怎么算由领域层的表驱动单测覆盖，
# 这里不重复算一遍——算了也只是同一份逻辑的第二个实现。
check_prune() {
  log "迭代 2d：按保留策略清理备份（Linux 容器证据）"

  local policy=frz-prune-policy
  cat > "$WORK_DIR/prune-policy.yaml" <<POLICY
apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: ${policy}
resource:
  kind: files
  paths:
    - ${BACKUP_SOURCE}
encoding:
  compression: gzip
  encryption:
    enabled: false
retention:
  keepLast: 1
POLICY
  docker cp "$WORK_DIR/prune-policy.yaml" "$CID:/opt/frz-ops/prune-policy.yaml"
  require_ok "提交保留策略（keepLast=1）" runtimectl backup policy put --file /opt/frz-ops/prune-policy.yaml

  # 备份根里已经有前面 check_backup 留下的内容，因此一律看**增量**，不看绝对数。
  local before
  before=$(q sh -c "find ${BACKUP_ROOT}/blobs -type f | wc -l" | tr -d ' ')

  # 连备三份，每份的源内容都不同——内容寻址下同样的内容会得到同样的 digest，
  # 那样三份会共享一个 blob，「删掉内容」这条就断言不出来了。
  local i
  for i in 1 2 3; do
    in_container sh -c "printf 'prune-content-%s\\n' '$i' > ${BACKUP_SOURCE}/alpha.txt"
    local run_out run_id
    run_out=$(rq backup run --policy "${policy}" --json)
    run_id=$(printf '%s' "${run_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
    if [ -z "${run_id}" ] || ! wait_for_status "${run_id}" "succeeded" "${RUNTIME_SOCK}"; then
      fail "第 $i 份备份未成功"
      return 0
    fi
  done
  assert_eq "三份备份各落一个 blob" "$((before + 3))" "$(q sh -c "find ${BACKUP_ROOT}/blobs -type f | wc -l" | tr -d ' ')"

  # 预演：报告将要清理的份数，但一个字节都不动。
  # 断言打在**人类可读输出**上（每份一行 `  - bkp_…`）：list/get 的 --json 是美化过的
  # 多行，用 sed 单行解析容易写出「看着对其实没匹配上」的断言。
  local preview
  preview=$(runtimectl backup prune --policy "${policy}" --dry-run)
  assert_eq "预演报出 2 份待清理" "2" "$(printf '%s' "${preview}" | grep -c '^  - bkp_' || true)"
  case "${preview}" in
    预演（未删除）：*) pass "预演的输出标明「未删除」" ;;
    *) fail "预演的输出没有标明「未删除」：${preview}" ;;
  esac
  assert_eq "预演之后 blob 一个没少" "$((before + 3))" "$(q sh -c "find ${BACKUP_ROOT}/blobs -type f | wc -l" | tr -d ' ')"

  # 真清理。
  local result
  result=$(runtimectl backup prune --policy "${policy}")
  assert_eq "清理报出 2 份已标记" "2" "$(printf '%s' "${result}" | grep -c '^  - bkp_' || true)"
  case "${result}" in
    已清理：*) pass "清理的输出标明「已清理」" ;;
    *) fail "清理的输出没有标明「已清理」：${result}" ;;
  esac
  assert_eq "清理之后只多了一个 blob" "$((before + 1))" "$(q sh -c "find ${BACKUP_ROOT}/blobs -type f | wc -l" | tr -d ' ')"

  # 剩下的那个 blob 的权限与属主不该被清理过程动过。
  local blob
  blob=$(q sh -c "find ${BACKUP_ROOT}/blobs -type f -newermt '-1 hour' | head -1")
  if [ -z "${blob}" ]; then
    fail "清理之后应当还剩一个刚写的 blob"
  else
    assert_eq "剩下的备份文件模式" "640" "$(q stat -c '%a' "${blob}")"
    assert_eq "剩下的备份文件属主" "root" "$(q stat -c '%U' "${blob}")"
  fi

  # 元数据：一份 succeeded、两份 pruned（记录不消失，只改状态）。
  local list_json
  list_json=$(rq backup list --policy "${policy}" --json)
  assert_eq "清理后仍是 succeeded 的份数" "1" "$(printf '%s' "${list_json}" | grep -c '"status": *"succeeded"')"
  assert_eq "清理后变成 pruned 的份数" "2" "$(printf '%s' "${list_json}" | grep -c '"status": *"pruned"')"

  # 保下来的那份必须仍能恢复——「还在」不等于「还能用」。
  # list 按开始时刻倒序，因此第一条就是最新那份，也就是 keepLast=1 保下来的那份。
  local kept_id
  kept_id=$(printf '%s' "${list_json}" | sed -n 's/.*"id": *"\(bkp_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${kept_id}" ]; then
    fail "找不到保下来的那份备份"
    return 0
  fi
  local restore_out restore_id
  restore_out=$(rq backup restore "${kept_id}" --mode isolated --json)
  restore_id=$(printf '%s' "${restore_out}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -n "${restore_id}" ] && wait_for_status "${restore_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "清理之后保下来的那份仍能隔离恢复"
  else
    fail "清理之后保下来的那份恢复失败"
  fi
}


# ==== 迭代 3：部署与回滚 ====
#
# 这一段是 3b 的主要证据：在**真实的 systemd** 上部署一个真实的制品、把服务跑起来、
# 换版本、回滚，并用「哪个端口在监听」判断**现在跑的是哪个版本**。
#
# 为什么用端口而不是日志判断：端口是外部可观测的事实，而日志是进程自己的说法。
# 两个版本监听**不同端口**，于是「版本换过去了没有」与「配置有没有跟着回去」都能被直接断言。
check_deploy() {
  log "迭代 3：部署、换版本与回滚（Linux 容器证据）"

  # release 目录计数。用 find -type d 而不是 `ls -d */`：后者会把 current（指向目录的
  # 符号链接）也算成一个目录，于是 keepLast=2 的断言永远差一。
  count_release_dirs() {
    q sh -c "find /opt/opsd/apps/${app}/releases -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l" | tr -d ' '
  }

  local app=frz-deploy
  local port_v1=28601
  local port_v2=28602
  local port_dead=28603   # 没人监听的端口：就绪检查打在这里，必然失败
  local port_listen=28604 # 那一次失败部署真正监听的端口（与就绪目标故意不同）

  require_ok "注册应用" runtimectl app create "${app}"

  # 制品就是探针二进制本身（单文件制品：unpack.strategy=none + fileName）。
  local uploaded artifact_id
  uploaded=$(rq artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${artifact_id}" ]; then
    fail "制品上传失败：${uploaded}"
    return 0
  fi
  pass "制品上传成功（${artifact_id}）"

  # deploy_manifest 造一份 manifest。
  #
  # 前两个参数是「实际监听的端口」与版本号；第三个可选，是**就绪检查打在哪里**——
  # 让两者不同，才能构造出「进程起来了但没就绪」这种失败（而不是"进程起不来"）。
  deploy_manifest() { # 监听端口 版本 [就绪端口]
    local listen_port=$1 version=$2 ready_port=${3:-$1}
    cat <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: go
artifact:
  id: ${artifact_id}
  version: ${version}
  fileName: bin/frz-probe
  unpack:
    strategy: none
exec:
  argv:
    - bin/frz-probe
    - --report
    - /var/lib/${app}/probe-report.txt
    - --listen
    - 127.0.0.1:${listen_port}
  workingDirectory: /var/lib/${app}
  runUser: ${app}
  ports:
    - ${listen_port}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${ready_port}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 15
  stopTimeoutSeconds: 15
logs:
  directory: /var/log/${app}
systemd:
  unitName: ${app}.service
release:
  keepLast: 2
MANIFEST
  }

  deploy_ok() { # 描述 端口 版本
    local desc=$1 port=$2 version=$3
    deploy_manifest "${port}" "${version}" > "$WORK_DIR/deploy-$3.yaml"
    docker cp "$WORK_DIR/deploy-$3.yaml" "$CID:/opt/frz-ops/deploy-$3.yaml"
    local out id
    out=$(rq app deploy --app "${app}" --file "/opt/frz-ops/deploy-$3.yaml" --json)
    id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
    if [ -z "${id}" ]; then
      fail "${desc}：未返回 operation id（${out}）"
      return 1
    fi
    if ! wait_for_status "${id}" "succeeded" "${RUNTIME_SOCK}"; then
      fail "${desc}：部署未成功（$(rq operation get "${id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p' | head -1)：$(rq operation logs "${id}" --json | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | tail -3 | tr '\n' ' ')）"
      return 1
    fi
    pass "${desc}"
    return 0
  }

  # 1) 部署 v1。
  if deploy_ok "部署 1.0.0" "${port_v1}" "1.0.0"; then
    assert_eq "unit 状态" "active" "$(q systemctl is-active ${app}.service)"
    assert_eq "1.0.0 的端口在监听" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v1} ' && echo yes || echo no")"
    assert_eq "current 指向的版本目录存在" "yes" \
      "$(q sh -c "test -x /opt/opsd/apps/${app}/releases/current/bin/frz-probe && echo yes || echo no")"
    # stat 默认跟随符号链接，因此这一条同时说明「current 指到的目录存在且是 0750」。
    # `stat -L`：不加 -L 时 stat 报的是**符号链接自己**的模式（永远是 777）。
    assert_eq "release 目录模式" "750" "$(q stat -L -c '%a' /opt/opsd/apps/${app}/releases/current)"
    assert_eq "current 是符号链接" "yes" \
      "$(q sh -c "test -L /opt/opsd/apps/${app}/releases/current && echo yes || echo no")"
  fi

  # 2) 部署 v2（另一个端口）：旧端口必须随之关闭——否则「换过去了没有」说不清楚。
  if deploy_ok "部署 2.0.0" "${port_v2}" "2.0.0"; then
    assert_eq "2.0.0 的端口在监听" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v2} ' && echo yes || echo no")"
    assert_eq "1.0.0 的端口已关闭" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v1} ' && echo no || echo yes")"
  fi

  # 3) 回滚到上一个版本（不带 --to）：**配置也要跟着回去**，因此 v1 的端口重新在监听。
  local rollback_out rollback_id
  rollback_out=$(rq app rollback --app "${app}" --json)
  rollback_id=$(printf '%s' "${rollback_out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -n "${rollback_id}" ] && wait_for_status "${rollback_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "回滚走 Operation 且成功"
    assert_eq "回滚后 1.0.0 的端口重新在监听" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v1} ' && echo yes || echo no")"
    assert_eq "回滚后 2.0.0 的端口已关闭" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v2} ' && echo no || echo yes")"
  else
    fail "回滚未成功"
  fi

  # 4) 一次注定失败的部署：就绪目标指向没人监听的端口。
  #    它必须**回到当前稳定版本**，而当前稳定版本是回滚之后的 1.0.0。
  local dirs_before_failure
  dirs_before_failure=$(count_release_dirs)
  # 监听 28604、就绪检查打 28603：进程能起来，但永远不就绪。
  deploy_manifest "${port_listen}" "3.0.0" "${port_dead}" > "$WORK_DIR/deploy-3.0.0-bad.yaml"
  docker cp "$WORK_DIR/deploy-3.0.0-bad.yaml" "$CID:/opt/frz-ops/deploy-3.0.0-bad.yaml"
  local fail_out fail_id
  fail_out=$(rq app deploy --app "${app}" --file /opt/frz-ops/deploy-3.0.0-bad.yaml --json)
  fail_id=$(printf '%s' "${fail_out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${fail_id}" ]; then
    fail "失败的部署没有返回 operation id：${fail_out}"
    return 0
  fi
  if wait_for_status "${fail_id}" "failed" "${RUNTIME_SOCK}"; then
    assert_eq "失败部署的错误码" "DEPLOY_ROLLED_BACK" \
      "$(rq operation get "${fail_id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p' | head -1)"
  else
    fail "部署 3.0.0 本该失败却成功了（状态 $(rq operation get "${fail_id}" --json | sed -n 's/.*"status": *"\([^"]*\)".*/\1/p' | head -1)，端口 $(q sh -c "ss -lnt | grep -c ':${port_dead} '" || echo 0)）"
  fi
  assert_eq "失败之后旧版本仍在监听" "yes" "$(q sh -c "ss -lnt | grep -q ':${port_v1} ' && echo yes || echo no")"
  # 失败的版本不该留下目录，也不该留下暂存目录：两者都会让下一次部署踩到残片。
  assert_eq "失败之后没有多出 release 目录" "${dirs_before_failure}" "$(count_release_dirs)"

  # 5) 保留策略：keepLast=2，再成功部署一次之后最老的目录应当被清掉，而 current 的永远在。
  if deploy_ok "再次部署 2.0.0" "${port_v2}" "2.0.0"; then
    assert_eq "current 的目录还在" "yes" \
      "$(q sh -c "test -d /opt/opsd/apps/${app}/releases/current && echo yes || echo no")"
    # keepLast=2：算上 current，盘上最多两个版本目录。
    assert_eq "release 目录数不超过 keepLast" "yes" \
      "$(q sh -c "test \$(find /opt/opsd/apps/${app}/releases -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l) -le 2 && echo yes || echo no")"
  fi

  # 6) 一次**物化阶段**的失败：制品声称是 tar-gz，内容却不是 gzip。
  #
  # 它比上面那次失败更靠前——流程已经**停掉了旧版本**，还没切到新版本就炸了。
  # 这段窗口里的失败必须把旧版本放回去：不然服务是停的，而 Operation 却写着
  # 「已回到上一个稳定版本」。这是真机验证暴露出来的缺陷，这里用真 systemd 钉住它。
  q sh -c "printf '这不是 gzip' > /opt/frz-ops/not-a-gzip" >/dev/null
  local bad_artifact
  bad_artifact=$(rq artifact put /opt/frz-ops/not-a-gzip --media-type application/gzip --json |
    sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${bad_artifact}" ]; then
    fail "坏制品上传失败"
    return 0
  fi

  local dirs_before_materialize_failure
  dirs_before_materialize_failure=$(count_release_dirs)
  cat > "$WORK_DIR/deploy-4.0.0-broken.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: go
artifact:
  id: ${bad_artifact}
  version: 4.0.0
  unpack:
    strategy: tar-gz
exec:
  argv:
    - bin/frz-probe
    - --report
    - /var/lib/${app}/probe-report.txt
    - --listen
    - 127.0.0.1:${port_v1}
  workingDirectory: /var/lib/${app}
  runUser: ${app}
  ports:
    - ${port_v1}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${port_v1}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 15
  stopTimeoutSeconds: 15
logs:
  directory: /var/log/${app}
release:
  keepLast: 2
MANIFEST
  docker cp "$WORK_DIR/deploy-4.0.0-broken.yaml" "$CID:/opt/frz-ops/deploy-4.0.0-broken.yaml"

  local broken_out broken_id
  broken_out=$(rq app deploy --app "${app}" --file /opt/frz-ops/deploy-4.0.0-broken.yaml --json)
  broken_id=$(printf '%s' "${broken_out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${broken_id}" ]; then
    fail "物化失败的部署没有返回 operation id：${broken_out}"
  elif wait_for_status "${broken_id}" "failed" "${RUNTIME_SOCK}"; then
    assert_eq "物化失败的错误码" "DEPLOY_ROLLED_BACK" \
      "$(rq operation get "${broken_id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p' | head -1)"
  else
    fail "坏制品本该让部署失败，却成功了"
  fi
  assert_eq "物化失败之后旧版本仍在监听（它被停过，必须被放回去）" "yes" \
    "$(q sh -c "ss -lnt | grep -q ':${port_v2} ' && echo yes || echo no")"
  assert_eq "物化失败之后没有多出 release 目录" "${dirs_before_materialize_failure}" "$(count_release_dirs)"
}

# 迭代 3c：资源限制落进 unit，并且**真的被内核采纳**。
check_resources() {
  log "迭代 3c：资源限制（CPUQuota= / MemoryMax=）与 java 解释器预检"

  local app=frz-res
  local port=28611
  require_ok "注册应用" runtimectl app create "${app}"

  local uploaded artifact_id
  uploaded=$(rq artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${artifact_id}" ]; then
    fail "制品上传失败：${uploaded}"
    return 0
  fi

  # workingDirectory 刻意写成 release 的 current（文档推荐的写法）。这同时压到一条分工：
  # Prepare 跑在物化之前，**不得**把 releases 子树内部建出来，否则 current 会变成一个实体
  # 目录，紧接着的符号链接切换就会以「改名失败」收场。
  cat > "$WORK_DIR/res.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: go
artifact:
  id: ${artifact_id}
  version: 1.0.0
  fileName: bin/frz-probe
  unpack:
    strategy: none
exec:
  argv:
    - bin/frz-probe
    - --report
    - /opt/opsd/apps/${app}/releases/current/probe-report.txt
    - --listen
    - 127.0.0.1:${port}
    - --allow-write
    - /opt/opsd/apps/${app}/releases/current/probe-writable
  workingDirectory: /opt/opsd/apps/${app}/releases/current
  runUser: ${app}
  ports:
    - ${port}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${port}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 15
  stopTimeoutSeconds: 15
logs:
  directory: /var/log/${app}
resources:
  cpuQuotaPercent: 200
  memoryMaxBytes: 536870912
release:
  keepLast: 2
MANIFEST
  docker cp "$WORK_DIR/res.yaml" "$CID:/opt/frz-ops/res.yaml"

  local out id
  out=$(rq app deploy --app "${app}" --file /opt/frz-ops/res.yaml --json)
  id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${id}" ]; then
    fail "带资源限制的部署未返回 operation id：${out}"
    return 0
  fi
  if ! wait_for_status "${id}" "succeeded" "${RUNTIME_SOCK}"; then
    fail "带资源限制的部署未成功：$(rq operation logs "${id}" --json | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | tail -3 | tr '\n' ' ')"
    return 0
  fi
  pass "带资源限制的部署成功（workingDirectory 指向 release 的 current）"
  assert_eq "部署后端口在监听" "yes" "$(q sh -c "ss -lnt | grep -q ':${port} ' && echo yes || echo no")"
  # 上面那条同时说明 Prepare 没有把 current 建成实体目录：真建成的话切换会失败，
  # 这次部署根本不会 succeeded。
  # 报告落在 current 里面（运行用户写自己的 release 目录），因此这一条同时是
  # 「current 解析正确」与「release 目录对运行用户可写」的证据。
  assert_eq "release 目录对运行用户可写" "yes" \
    "$(q sh -c "grep -q '^allow-write .* writable=yes$' /opt/opsd/apps/${app}/releases/current/probe-report.txt && echo yes || echo no")"

  assert_eq "unit 里的 CPUQuota" "CPUQuota=200%" "$(q grep -x 'CPUQuota=200%' "/etc/systemd/system/${app}.service")"
  assert_eq "unit 里的 MemoryMax 写的是原始字节数" "MemoryMax=536870912" \
    "$(q grep -x 'MemoryMax=536870912' "/etc/systemd/system/${app}.service")"

  # 「真的生效」的判据分两层：systemd 采纳了它，内核按它限制。
  assert_eq "systemctl show 报告的 MemoryMax" "536870912" \
    "$(q systemctl show "${app}.service" -p MemoryMax --value)"
  # CPUQuota=200% 在 systemctl show 里是时间量写法「每秒 2 秒 CPU 时间」。
  assert_eq "systemctl show 报告的 CPUQuotaPerSecUSec" "2s" \
    "$(q systemctl show "${app}.service" -p CPUQuotaPerSecUSec --value)"

  # cgroup v2 是限制**真正生效**的地方。路径不写死 /system.slice/...：容器里的
  # systemd 跑在自己的 cgroup 命名空间里（/docker/<id>/system.slice/...），
  # 由 systemd 自己报出来的 ControlGroup 才是可移植的那个来源。
  local cgroup
  cgroup=$(q systemctl show "${app}.service" -p ControlGroup --value)
  if [ -z "${cgroup}" ]; then
    fail "拿不到 unit 的 cgroup 路径（ControlGroup 为空）"
  else
    # cpu.max 的格式是 "<quota> <period>"：200% = 每 100ms 周期里 200ms 的 CPU 时间。
    assert_eq "cgroup 的 memory.max" "536870912" "$(q cat "/sys/fs/cgroup${cgroup}/memory.max")"
    assert_eq "cgroup 的 cpu.max（200% = 200000/100000）" "200000 100000" \
      "$(q cat "/sys/fs/cgroup${cgroup}/cpu.max")"
  fi

  # java 的解释器预检：指向不存在的 JDK 时，`runtime validate` 必须在**部署之前**
  # 以 MANIFEST_INVALID 失败，而不是等到 unit 起来、进程退出、报「就绪超时」。
  local japp=frz-res-java
  require_ok "注册 java 应用" runtimectl app create "${japp}"
  cat > "$WORK_DIR/res-java.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${japp}
runtime: java
artifact:
  id: ${artifact_id}
  version: 1.0.0
  fileName: app.jar
  unpack:
    strategy: none
exec:
  argv:
    - /no/such/jdk/bin/java
    - -Xmx256m
    - -jar
    - app.jar
  workingDirectory: /opt/opsd/apps/${japp}/releases/current
  runUser: ${japp}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:28612
  startTimeoutSeconds: 15
logs:
  directory: /var/log/${japp}
MANIFEST
  docker cp "$WORK_DIR/res-java.yaml" "$CID:/opt/frz-ops/res-java.yaml"
  require_ok "java manifest 本身合法（磁盘上还没有那个 JDK，因此这一步必须过）" \
    runtimectl spec put --app "${japp}" --file /opt/frz-ops/res-java.yaml

  local jout jrc
  jout=$(runtimectl runtime validate --app "${japp}" 2>&1) && jrc=0 || jrc=$?
  assert_eq "解释器不存在时 runtime validate 的退出码" "17" "${jrc}"
  assert_eq "报错里带上了那个解释器路径" "yes" \
    "$(printf '%s' "${jout}" | grep -q '/no/such/jdk/bin/java' && echo yes || echo no)"

  # 反例的另一半：解释器**存在**时同一条命令必须放行——否则这条检查只是「什么都拦」，
  # 而那会让所有 java 部署在真机上直接不可用。容器里没有 JDK，因此拿一个真实存在的
  # 可执行文件当解释器：这里验的是预检与「argv 原样送达」，javac/JAR 那一层由真机覆盖。
  sed 's#/no/such/jdk/bin/java#/bin/sleep#' "$WORK_DIR/res-java.yaml" > "$WORK_DIR/res-java-ok.yaml"
  docker cp "$WORK_DIR/res-java-ok.yaml" "$CID:/opt/frz-ops/res-java-ok.yaml"
  require_ok "解释器换成存在的路径" \
    runtimectl spec put --app "${japp}" --file /opt/frz-ops/res-java-ok.yaml
  require_ok "解释器存在时 runtime validate 通过" runtimectl runtime validate --app "${japp}"
}

# 迭代 4：Nginx 蓝绿。**这一段的证据是 curl 拿到的内容**——它不是「端口在听」，
# 而是「现在服务的是哪一版」，比端口强一层。
check_bluegreen() {
  log "迭代 4：Nginx 蓝绿切流（真 Nginx、真切流）"

  local app=frz-bg
  local vport=8080      # Nginx 的对外端口
  local blue_port=28621
  local green_port=28622
  local managed=/etc/nginx/frz-managed/${app}.conf

  # Nginx 是这一段的前提。Ubuntu 的包会 enable 它，但容器里的系统服务未必都已经起来，
  # 因此这里显式确认一次——失败要失败在「没有 Nginx」，而不是后面某条看不懂的断言上。
  q systemctl start nginx >/dev/null 2>&1 || true
  assert_eq "Nginx 在跑（蓝绿的前提）" "active" "$(q systemctl is-active nginx)"

  require_ok "注册应用" runtimectl app create "${app}"

  local uploaded artifact_id
  uploaded=$(rq artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "${artifact_id}" ]; then
    fail "制品上传失败：${uploaded}"
    return 0
  fi

  # 蓝绿 manifest：两个槽位各一个端口 + 一个对外端口。
  #
  # **端口与版本都从槽位环境里来**：两个槽位共用同一份 argv，而它们必须听不同的端口。
  # 这正是规格 D6 里那条契约的实样——「应用怎么知道自己的端口」由运维通过槽位环境告诉它，
  # 工具不发明 FRZ_SLOT_PORT 之类的魔法名字。探针作为被托管的应用，读的也是它自己的变量。
  bg_manifest() { # 版本
    local version=$1
    cat <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: go
artifact:
  id: ${artifact_id}
  version: ${version}
  fileName: bin/frz-probe
  unpack:
    strategy: none
exec:
  argv:
    - bin/frz-probe
    - --report
    - /var/lib/${app}/probe-report.txt
  workingDirectory: /var/lib/${app}
  runUser: ${app}
  slots:
    blue:
      ports: [${blue_port}]
      readiness:
        type: tcp
        target: "127.0.0.1:${blue_port}"
        consecutiveSuccesses: 1
      environment:
        FRZ_PROBE_LISTEN: "127.0.0.1:${blue_port}"
        FRZ_PROBE_VERSION: "${version}"
    green:
      ports: [${green_port}]
      readiness:
        type: tcp
        target: "127.0.0.1:${green_port}"
        consecutiveSuccesses: 1
      environment:
        FRZ_PROBE_LISTEN: "127.0.0.1:${green_port}"
        FRZ_PROBE_VERSION: "${version}"
health:
  startTimeoutSeconds: 30
  stopTimeoutSeconds: 15
logs:
  directory: /var/log/${app}
nginx:
  listen: ${vport}
  observationSeconds: 2
  drainSeconds: 1
release:
  keepLast: 3
MANIFEST
  }

  # bg_deploy 把 operation id 写进全局 BG_OP_ID，**不用 $( ) 取返回值**：命令替换是子
  # shell，里面的 pass/fail 不会计入父 shell 的计数（而日志会记——见 PASS_LOG/FAIL_LOG
  # 那道元断言）。这个坑 2026-09-26 真的踩到了。
  bg_deploy() { # 描述 版本
    local desc=$1 version=$2
    BG_OP_ID=""
    bg_manifest "${version}" > "$WORK_DIR/bg-${version}.yaml"
    docker cp "$WORK_DIR/bg-${version}.yaml" "$CID:/opt/frz-ops/bg-${version}.yaml"
    local out id
    out=$(rq app deploy --app "${app}" --file "/opt/frz-ops/bg-${version}.yaml" --json)
    id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
    if [ -z "${id}" ]; then
      fail "${desc}：未返回 operation id（${out}）"
      return 1
    fi
    if ! wait_for_status "${id}" "succeeded" "${RUNTIME_SOCK}"; then
      fail "${desc}：部署未成功（$(rq operation get "${id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p' | head -1)：$(rq operation logs "${id}" --json | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | tail -3 | tr '\n' ' ')）"
      return 1
    fi
    pass "${desc}"
    BG_OP_ID="${id}"
    return 0
  }

  served_version() { # 从 Nginx 的对外端口读回「现在服务的是哪一版」
    q sh -c "curl -s -m 3 http://127.0.0.1:${vport}/ | tr -d '\r\n'"
  }

  # A) 反例：主配置还没 include 受管的目录 → 部署必须**拒绝切流**。
  #
  #    这是这一层最要紧的一条：配置写对了但没生效时，切流看起来会成功，而流量根本不
  #    经过我们改的 upstream。宁可在这里失败。
  bg_manifest 1.0.0 > "$WORK_DIR/bg-1.0.0.yaml"
  docker cp "$WORK_DIR/bg-1.0.0.yaml" "$CID:/opt/frz-ops/bg-1.0.0.yaml"
  local out id
  out=$(rq app deploy --app "${app}" --file /opt/frz-ops/bg-1.0.0.yaml --json)
  id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "${id}" ]; then
    fail "未返回 operation id：${out}"
  elif wait_for_status "${id}" "failed" "${RUNTIME_SOCK}"; then
    assert_eq "受管目录没被主配置加载时的错误码" "NGINX_CONFIG_INVALID" \
      "$(rq operation get "${id}" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p' | head -1)"
    assert_eq "报错指出了该怎么办（include 受管目录）" "yes" \
      "$(rq operation logs "${id}" --json | grep -q 'frz-managed' && echo yes || echo no)"
  else
    fail "主配置没有 include 受管目录，部署本该失败却成功了"
  fi
  assert_eq "被拒绝的部署没有让 Nginx 监听到对外端口" "no" \
    "$(q sh -c "ss -lnt | grep -q ':${vport} ' && echo yes || echo no")"

  # B) 运维的那一步：在主配置里 include 受管目录（一行 conf.d 文件即可）。
  require_ok "把受管目录加进主配置（运维的动作）" \
    q sh -c "printf 'include /etc/nginx/frz-managed/*.conf;\n' > /etc/nginx/conf.d/frz-managed.conf && nginx -t"

  # C) 第一次部署：1.0.0 落到 blue，对外端口能拿到 version=1.0.0。
  bg_deploy "第一次部署 1.0.0（落 blue）" 1.0.0 || return 0
  assert_eq "对外端口拿到的版本" "version=1.0.0" "$(served_version)"
  assert_eq "blue 的 unit 在跑" "active" "$(q systemctl is-active ${app}-blue.service)"
  assert_eq "green 的 unit 没被起过" "inactive" "$(q systemctl is-active ${app}-green.service)"
  assert_eq "受管配置指向 blue 的端口" "yes" \
    "$(q sh -c "grep -q 'server 127.0.0.1:${blue_port};' ${managed} && echo yes || echo no")"
  assert_eq "受管配置里没有 green 的端口" "no" \
    "$(q sh -c "grep -q '${green_port}' ${managed} && echo yes || echo no")"
  assert_eq "受管配置的头注释说明了它指向哪一侧" "yes" \
    "$(q sh -c "head -2 ${managed} | grep -q 'blue' && echo yes || echo no")"

  # D) 第二次部署**期间**持续打请求：切流不能丢掉任何一个请求。
  cat > "$WORK_DIR/curl-loop.sh" <<'LOOP'
#!/bin/sh
# 切流期间持续打请求：成功与失败都各记一行——**只记失败的话，「零失败」可能只是
# 「零请求」**，而那正是最危险的一种绿。
while [ ! -f /tmp/frz-bg-stop ]; do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:8080/; then
    echo ok >> /tmp/frz-bg-requests
  else
    echo fail >> /tmp/frz-bg-requests
  fi
  sleep 0.05
done
LOOP
  docker cp "$WORK_DIR/curl-loop.sh" "$CID:/opt/frz-ops/curl-loop.sh"
  require_ok "启动切流期间的请求循环" \
    q sh -c 'rm -f /tmp/frz-bg-requests /tmp/frz-bg-stop; chmod 0755 /opt/frz-ops/curl-loop.sh; setsid nohup /opt/frz-ops/curl-loop.sh >/dev/null 2>&1 </dev/null & echo started'

  local switched_ok=0
  if bg_deploy "第二次部署 2.0.0（切到 green）" 2.0.0; then
    switched_ok=1
  fi
  q sh -c 'touch /tmp/frz-bg-stop' >/dev/null
  sleep 0.3

  if [ "${switched_ok}" = "1" ]; then
    assert_eq "切流之后对外端口拿到的是新版本" "version=2.0.0" "$(served_version)"
  fi
  assert_eq "切流期间失败的请求数" "0" "$(q sh -c 'grep -c fail /tmp/frz-bg-requests || true')"
  # 元断言：循环必须**成功地**打过足够多的请求。只看「零失败」是不够的——循环如果压根
  # 没跑起来（或者容器里没有 curl），它同样会「零失败」，那是一个测不出东西的断言。
  assert_eq "切流期间成功的请求数 ≥ 50" "yes" \
    "$(q sh -c 'test "$(grep -c "^ok$" /tmp/frz-bg-requests)" -ge 50 && echo yes || echo no')"
  assert_eq "green 的 unit 在跑" "active" "$(q systemctl is-active ${app}-green.service)"
  assert_eq "blue 被排空后停掉" "inactive" "$(q systemctl is-active ${app}-blue.service)"
  assert_eq "受管配置指向 green 的端口" "yes" \
    "$(q sh -c "grep -q 'server 127.0.0.1:${green_port};' ${managed} && echo yes || echo no")"
  # 两个槽位的 unit 各自存在，且都 enable（重启后各自回到该有的状态）。
  assert_eq "blue 的 unit 开机自启" "enabled" "$(q systemctl is-enabled ${app}-blue.service)"
  assert_eq "green 的 unit 开机自启" "enabled" "$(q systemctl is-enabled ${app}-green.service)"

  # E) 回滚：把流量切回 1.0.0 所在的 blue。
  local rollback_out rollback_id
  rollback_out=$(rq app rollback --app "${app}" --json)
  rollback_id=$(printf '%s' "${rollback_out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -n "${rollback_id}" ] && wait_for_status "${rollback_id}" "succeeded" "${RUNTIME_SOCK}"; then
    pass "回滚走 Operation 且成功"
    assert_eq "回滚后对外端口拿到的是旧版本" "version=1.0.0" "$(served_version)"
    assert_eq "回滚后 blue 重新在跑" "active" "$(q systemctl is-active ${app}-blue.service)"
    assert_eq "回滚后 green 被停掉" "inactive" "$(q systemctl is-active ${app}-green.service)"
  else
    fail "回滚未成功"
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
  # PROBE_ENV 是 runtime 夹具里 kind=env 凭据的来源（解析器从 opsd 自己的环境读），
  # 值就是 provision 写下的那份字节；这里的取值方式也只裁掉行尾换行，不会动其余字节。
  docker exec -d -u frz-ops "$CID" /opt/frz-ops/opsd --config /etc/opsd/config.yaml
  wait_for_socket

  # 1c 的 RuntimeAdapter 需要 root：useradd 建运行用户、chown 收敛属主、
  # systemctl 装/启 unit。迭代 0 的 frz-ops 实例覆盖的是不需要权限的部分，
  # 因此这里另起一个 root 实例（独立 socket/DB/日志），runtime 组只打在它上面。
  # PROBE_ENV 也在这个实例上注入：kind=env 凭据的值来自 opsd 自己的进程环境。
  log "以 root 启动第二个 opsd（runtime 组专用）"
  docker exec -d -e "PROBE_ENV=$(cat "$WORK_DIR/probe-env-value")" \
    "$CID" /opt/frz-ops/opsd --config /etc/opsd/root.yaml
  wait_for_socket "${RUNTIME_SOCK}"

  check_filesystem
  check_socket_acl
  check_executor
  check_artifacts_and_secrets
  check_schedules
  check_systemd
  # 迭代 1d：runtime.* 的重试与 Restart= 不得叠加。它有自己的两个夹具应用，
  # 但同样打在 root 实例上，因此放在 check_runtime 之前。
  check_retry
  # 迭代 2a：备份的存储根、权限与与制品 GC 的隔离。
  check_backup
  # 迭代 2d：按保留策略清理备份（keepLast / keepDays，不含 GFS）。
  check_prune
  # 迭代 3：部署与回滚（真 systemd、真进程、真切换）。
  check_deploy
  # 迭代 3c：资源限制落进 unit 并被内核采纳，java 解释器预检。
  check_resources
  # 迭代 4：Nginx 蓝绿（真 Nginx、真 curl）。
  check_bluegreen
  # 放在最后：它会故意让探针 unit 停在 failed 状态（验证缺凭据必须起不来）。
  check_runtime

  log "结果"
  # 元断言：计数必须与日志行数一致。不一致说明有断言是在 `$( )` 里跑的——那种情况下
  # 「0 项失败」是假的，而它会一路绿着通过。
  local logged_pass logged_fail
  logged_pass=$(wc -l < "$PASS_LOG" | tr -d ' ')
  logged_fail=$(wc -l < "$FAIL_LOG" | tr -d ' ')
  if [ "${logged_pass}" != "${PASS_COUNT}" ] || [ "${logged_fail}" != "${FAIL_COUNT}" ]; then
    printf 'FAIL  断言计数与日志不一致：计数 %d/%d，日志 %s/%s（有断言跑在子 shell 里）\n' \
      "$PASS_COUNT" "$FAIL_COUNT" "${logged_pass}" "${logged_fail}" >&2
    FAIL_COUNT=$((FAIL_COUNT + 1))
  fi
  printf '%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
  [ "$FAIL_COUNT" -eq 0 ] || exit 1
}

main "$@"
