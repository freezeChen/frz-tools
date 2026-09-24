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
  # 放在最后：它会故意让探针 unit 停在 failed 状态（验证缺凭据必须起不来）。
  check_runtime

  log "结果"
  printf '%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
  [ "$FAIL_COUNT" -eq 0 ] || exit 1
}

main "$@"
