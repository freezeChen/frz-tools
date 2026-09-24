#!/usr/bin/env bash
#
# 真实 Linux 主机上的验证。**证据类型：Linux 主机**（不是 Linux 容器）。
#
# 在目标主机上以 root 运行；通常由 test/host/run.sh 从工作站上传后触发：
#
#   bash test/host/run.sh                      # 默认 root@192.168.11.101
#   FRZ_HOST=root@other-host bash test/host/run.sh
#
# 需要的东西（run.sh 会放进 $FRZ_HOST_DIR）：bin/{opsd,opsctl,frz-probe}、
# opsd.host.verify.yaml、probe-file。
#
# 这一轮要证明的是**容器证明不了的东西**：
#   · RHEL 系发行版（Rocky Linux）＋真实 systemd 的 unit 生命周期；
#   · **SELinux 处于 enforcing** 时，文件与进程的实际安全上下文；
#   · 真实主机上的 useradd / chown / systemctl 落到磁盘上的结果；
#   · opsd 本身作为 **systemd 服务**运行的部署形态（含 /run 是 tmpfs 这件事）。
#
# 刻意**不**重复容器 harness 的哪些断言，以及为什么（避免两套断言各自漂移）：
#   · 「以非 root 用户运行 opsd」那一组（socket ACL、非授权可执行文件被拒、计划与
#     审计的端到端）：真机上 opsd 必须以 root 运行才能 useradd/systemctl，那个实例在
#     真机上立不起来；那组语义与内核/发行版无关，容器 harness 与 CI 每轮都跑。
#   · 迭代 0/1b/1d/2a 的多数用例（执行器、调度器、重试、备份）：同上，与发行版无关。
#   本脚本覆盖的是上面那四类容器给不了的证据，外加「同一套 unit 语义在真机上是否还成立」。
#
# 三个阶段（FRZ_HOST_PHASE）：
#   full（默认）  完整跑一遍：建状态 → 断言 → 删干净
#   prepare      只建状态并启动，**不清场**——留下的东西正是要跨重启存活的
#   check        重启后接着断言：真的重启了吗、服务自动起来了吗、磁盘上的状态还在吗，
#                跑完再删干净
#   cleanup      只做收尾（跑到一半被打断时用）
#
# 结束时会把本轮创建的一切**删干净**（用户、组、unit、目录），并逐项报告；
# 设置 FRZ_HOST_KEEP=1 可以保留现场用于排查。

set -euo pipefail

FRZ_HOST_DIR=${FRZ_HOST_DIR:-/opt/frz-host-verify}
PROBE_ENV_VALUE=${FRZ_PROBE_ENV_VALUE:-host-probe-secret-123}

RUNTIME_APP=frz-probe
RUNTIME_USER=frz-probe
OTHER_USER=frz-other
RUNTIME_UNIT=frz-probe.service
RUNTIME_PORT=${FRZ_PROBE_PORT:-28581}
PROBE_DIR=/etc/opsd/apps/${RUNTIME_APP}
MYSQL_POLICY=frz-verify-mysql
OPSD_UNIT=frz-opsd-verify.service
OPSCTL="$FRZ_HOST_DIR/bin/opsctl"
OPSD="$FRZ_HOST_DIR/bin/opsd"

START_OP_ID=""
PASS_COUNT=0
FAIL_COUNT=0
FINDINGS=()

log()  { printf '\n--- %s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*" >&2; }
note() { FINDINGS+=("$*"); printf 'NOTE  %s\n' "$*"; }

assert_eq() { # 描述 期望 实际
  if [ "$2" = "$3" ]; then
    pass "$1 = $3"
  else
    fail "$1：期望 $2，实际 ${3:-<空>}"
  fi
}

require_ok() { # 描述 命令…
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then
    pass "$desc"
  else
    fail "$desc（命令：$*）"
  fi
}

require_fail() { # 描述 命令…
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then
    fail "${desc}（命令意外成功）"
  else
    pass "$desc"
  fi
}

mode_of()  { stat -c '%a' "$1" 2>/dev/null || true; }
owner_of() { stat -c '%U:%G' "$1" 2>/dev/null || true; }
label_of() { stat -c '%C' "$1" 2>/dev/null || true; }

opsctl() { "$OPSCTL" --socket /run/opsd/opsd.sock "$@"; }
q()      { "$OPSCTL" --socket /run/opsd/opsd.sock "$@" 2>/dev/null || true; }

wait_for_socket() {
  for _ in $(seq 1 60); do
    [ -S /run/opsd/opsd.sock ] && return 0
    sleep 0.5
  done
  printf 'opsd 未创建 socket，服务日志尾部：\n' >&2
  journalctl -u "$OPSD_UNIT" -n 30 --no-pager >&2 || true
  exit 1
}

wait_for_status() { # operation-id 期望状态
  local id=$1 want=$2 status=""
  for _ in $(seq 1 120); do
    status=$(q operation get "$id" --json | sed -n 's/.*"status": *"\([^"]*\)".*/\1/p')
    [ "$status" = "$want" ] && return 0
    sleep 0.5
  done
  return 1
}

wait_for_ready() { # 就绪要求连续通过 consecutiveSuccesses 次，第一次查询可能还没就绪
  for _ in $(seq 1 60); do
    opsctl runtime health --app "$RUNTIME_APP" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# ==== 收尾 ====
# 一律执行，并报告删了什么。这是生产机，「跑完就走」比「留着现场」重要得多。
cleanup() {
  if [ "${FRZ_HOST_KEEP:-0}" = "1" ]; then
    printf '\n保留了现场（FRZ_HOST_KEEP=1）：/etc/opsd /var/lib/opsd /var/log/opsd /run/opsd /opt/frz-ops %s\n' "$FRZ_HOST_DIR"
    return 0
  fi

  printf '\n--- 收尾（删除本轮创建的一切）\n'
  systemctl stop "$OPSD_UNIT" >/dev/null 2>&1 || true
  systemctl stop "$RUNTIME_UNIT" >/dev/null 2>&1 || true
  systemctl disable "$OPSD_UNIT" >/dev/null 2>&1 || true
  systemctl disable "$RUNTIME_UNIT" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/$OPSD_UNIT" "/etc/systemd/system/$RUNTIME_UNIT"
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl reset-failed >/dev/null 2>&1 || true

  userdel "$RUNTIME_USER" >/dev/null 2>&1 || true
  userdel "$OTHER_USER" >/dev/null 2>&1 || true
  userdel frz-ops >/dev/null 2>&1 || true
  groupdel "$RUNTIME_USER" >/dev/null 2>&1 || true
  groupdel "$OTHER_USER" >/dev/null 2>&1 || true
  groupdel frz-ops >/dev/null 2>&1 || true

  rm -rf /etc/opsd /var/lib/opsd /var/log/opsd /run/opsd /opt/frz-ops
  rm -rf "/var/lib/$RUNTIME_APP" "/var/log/$RUNTIME_APP" "/var/lib/${RUNTIME_APP}-extra"
  rm -rf "$FRZ_HOST_DIR"

  printf '已删除：用户/组 frz-ops、%s、%s；unit %s 与 %s；目录 /etc/opsd、/var/lib/opsd、\n' \
    "$RUNTIME_USER" "$OTHER_USER" "$OPSD_UNIT" "$RUNTIME_UNIT"
  printf '        /var/log/opsd、/run/opsd、/opt/frz-ops、/var/lib/%s、/var/log/%s、%s\n' \
    "$RUNTIME_APP" "$RUNTIME_APP" "$FRZ_HOST_DIR"
}

# ==== 环境事实 ====
env_facts() {
  log "环境事实（证据的一部分，不是断言）"
  printf '发行版   : %s\n' "$(. /etc/os-release && printf '%s %s' "$NAME" "$VERSION")"
  printf '内核     : %s\n' "$(uname -r)"
  printf '架构     : %s\n' "$(uname -m)"
  printf 'systemd  : %s\n' "$(systemctl --version | head -1)"
  printf 'SELinux  : %s\n' "$(getenforce 2>/dev/null || printf '未安装')"
  printf 'cgroup   : %s\n' "$(stat -fc %T /sys/fs/cgroup)"
  printf 'unit 路径: %s\n' "$(systemd-analyze unit-paths 2>/dev/null | grep -E '^/etc/systemd/system$|^/usr/lib/systemd/system$' | tr '\n' ' ')"
}

# ==== 前置 ====
check_preconditions() {
  log "前置：本轮要动的东西必须都还不存在（否则说明上一轮没收干净）"

  assert_eq "运行身份" "root" "$(id -un)"
  local dirty=0
  for user in frz-ops "$RUNTIME_USER" "$OTHER_USER"; do
    id "$user" >/dev/null 2>&1 && { fail "$user 用户已存在，无法从「干净主机」开始"; dirty=1; }
  done
  for path in /etc/opsd /var/lib/opsd /run/opsd /opt/frz-ops "/var/lib/$RUNTIME_APP"; do
    [ -e "$path" ] && { fail "$path 已存在，无法从「干净主机」开始"; dirty=1; }
  done
  [ "$dirty" -eq 0 ] && pass "本轮涉及的用户与目录此时都不存在"

  if ss -lntH "sport = :$RUNTIME_PORT" 2>/dev/null | grep -q .; then
    fail "端口 $RUNTIME_PORT 已被占用，换 FRZ_PROBE_PORT 再跑"
  else
    pass "端口 $RUNTIME_PORT 空闲"
  fi
}

# ==== 安装态（模拟安装脚本留下的状态）====
provision() {
  log "准备专用用户、目录与配置"

  # 与容器 harness 同因：安装脚本留下的 /etc/opsd 是 0750，Prepare 之后会变成 0751
  # （kind=file 凭据要能被运行用户按路径穿越）。两条断言不是矛盾而是时序。
  groupadd --system frz-ops
  useradd --system -g frz-ops --no-create-home --home-dir /var/lib/opsd frz-ops
  useradd --system "$OTHER_USER"
  install -d -m 0750 -o frz-ops -g frz-ops /etc/opsd /var/lib/opsd /var/log/opsd
  install -d -m 0700 -o frz-ops -g frz-ops /etc/opsd/secrets
  install -d -m 0755 /opt/frz-ops

  install -m 0755 "$OPSD" /opt/frz-ops/opsd
  install -m 0755 "$OPSCTL" /opt/frz-ops/opsctl
  install -m 0755 "$FRZ_HOST_DIR/bin/frz-probe" /opt/frz-ops/frz-probe
  install -m 0600 -o frz-ops -g frz-ops "$FRZ_HOST_DIR/opsd.host.verify.yaml" /etc/opsd/config.yaml
  install -m 0600 -o frz-ops -g frz-ops "$FRZ_HOST_DIR/probe-file" /etc/opsd/secrets/probe-file
}

# ==== opsd 作为 systemd 服务 ====
install_opsd_unit() {
  log "把 opsd 装成 systemd 服务并启动"

  # RuntimeDirectory=opsd：/run 是 tmpfs、开机即空，socket 的父目录必须由 systemd
  # 每次启动时建。这是真机部署与「在容器里手工 mkdir」最实质的区别之一——
  # 少了它，重启之后 opsd 起不来，而容器里永远看不出来。
  cat > "/etc/systemd/system/$OPSD_UNIT" <<UNIT
[Unit]
Description=frz-ops daemon（test/host/verify.sh 的验证实例）
After=network.target

[Service]
Type=simple
ExecStart=/opt/frz-ops/opsd --config /etc/opsd/config.yaml
Environment=PROBE_ENV=${PROBE_ENV_VALUE}
RuntimeDirectory=opsd
RuntimeDirectoryMode=0750
Restart=no

[Install]
WantedBy=multi-user.target
UNIT

  require_ok "daemon-reload" systemctl daemon-reload
  # enable 是**部署的一部分**：不 enable，重启后 opsd 不会自己起来，
  # 而「重启后还在不在」正是这一轮要验证的东西。
  require_ok "enable $OPSD_UNIT（开机会起）" systemctl enable "$OPSD_UNIT"
  require_ok "启动 $OPSD_UNIT" systemctl start "$OPSD_UNIT"
  wait_for_socket
  pass "socket 已创建：/run/opsd/opsd.sock"

  assert_eq "$OPSD_UNIT 服务状态" "active" "$(systemctl is-active "$OPSD_UNIT" 2>/dev/null)"
  assert_eq "$OPSD_UNIT 开机自启" "enabled" "$(systemctl is-enabled "$OPSD_UNIT" 2>/dev/null)"
  assert_eq "RuntimeDirectory 建出的 /run/opsd 模式" "750" "$(mode_of /run/opsd)"
  # 属主是**服务的运行用户**（这里 opsd 以 root 运行，因为 RuntimeAdapter 要
  # useradd/chown/systemctl），因此是 root:root——不是 frz-ops。
  printf '      /run/opsd 属主           : %s（RuntimeDirectory 属服务运行用户）\n' "$(owner_of /run/opsd)"
  assert_eq "socket 模式" "660" "$(mode_of /run/opsd/opsd.sock)"
  printf '      socket 属主              : %s\n' "$(owner_of /run/opsd/opsd.sock)"
  printf '      opsd 进程的 SELinux 上下文: %s\n' "$(tr -d '\0' < "/proc/$(systemctl show "$OPSD_UNIT" -p MainPID --value)/attr/current" 2>/dev/null || printf '<不可读>')"
  note "以 root 运行的 opsd 建出的 socket 属主是 $(owner_of /run/opsd/opsd.sock)：非 root 用户用不了 opsctl，而配置里没有 socket 属组项——真机暴露出来的部署缺口，本轮不修，记为待定"
}

# ==== 安装态断言 ====
check_install_state() {
  log "安装态：目录与配置文件的模式、属主、SELinux 上下文"

  assert_eq "/etc/opsd 模式（Prepare 之前，安装脚本状态）" "750" "$(mode_of /etc/opsd)"
  assert_eq "/etc/opsd 属主" "frz-ops:frz-ops" "$(owner_of /etc/opsd)"
  assert_eq "/etc/opsd/config.yaml 模式" "600" "$(mode_of /etc/opsd/config.yaml)"
  assert_eq "/etc/opsd/config.yaml 属主" "frz-ops:frz-ops" "$(owner_of /etc/opsd/config.yaml)"
  assert_eq "/etc/opsd/secrets 模式" "700" "$(mode_of /etc/opsd/secrets)"
  assert_eq "凭据文件模式" "600" "$(mode_of /etc/opsd/secrets/probe-file)"
  assert_eq "凭据文件属主" "frz-ops:frz-ops" "$(owner_of /etc/opsd/secrets/probe-file)"

  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" = "Enforcing" ]; then
    pass "SELinux 处于 Enforcing：下面的上下文记录才有意义"
    printf '      /etc/opsd                : %s\n' "$(label_of /etc/opsd)"
    printf '      /etc/opsd/config.yaml    : %s\n' "$(label_of /etc/opsd/config.yaml)"
    printf '      /opt/frz-ops/opsd        : %s\n' "$(label_of /opt/frz-ops/opsd)"
  else
    printf 'SELinux 未处于 Enforcing，跳过上下文记录\n'
  fi
}

# ==== RuntimeAdapter：Prepare ====
check_prepare() {
  log "RuntimeAdapter：提交 manifest 与 Prepare 产物"

  local uploaded artifact_id
  uploaded=$(q artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "$uploaded" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$artifact_id" ]; then
    fail "runtime 夹具的制品上传失败：${uploaded}"
    return 0
  fi
  pass "runtime 夹具制品上传成功（$artifact_id）"

  cat > "$FRZ_HOST_DIR/frz-probe.yaml" <<MANIFEST
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
  chmod 0644 "$FRZ_HOST_DIR/frz-probe.yaml"

  require_ok "注册应用" opsctl app create "$RUNTIME_APP"
  require_ok "提交 manifest（严格校验通过）" opsctl spec put --app "$RUNTIME_APP" --file "$FRZ_HOST_DIR/frz-probe.yaml"
  require_ok "runtime validate 无副作用通过" opsctl runtime validate --app "$RUNTIME_APP"

  local prepared tier version
  prepared=$(q runtime prepare --app "$RUNTIME_APP" --json)
  tier=$(printf '%s' "$prepared" | sed -n 's/.*"tier": *"\([^"]*\)".*/\1/p')
  version=$(printf '%s' "$prepared" | sed -n 's/.*"systemdVersion": *\([0-9]*\).*/\1/p')
  if [ -z "$tier" ]; then
    fail "runtime prepare 失败：$(opsctl runtime prepare --app "$RUNTIME_APP" 2>&1 | head -3 | tr '\n' ' ')"
    return 0
  fi
  assert_eq "Prepare 返回的 unit 档位" "strict" "$tier"
  if [ -n "$version" ] && [ "$version" -ge 240 ]; then
    pass "探测到的 systemd 版本 ${version} 落在 strict 档（≥240）"
  else
    fail "Prepare 返回的 systemd 版本异常：${version:-<空>}"
  fi

  # unit 落盘位置与内容：真机上这决定了 systemd 会不会加载它、以谁的什么权限跑。
  local unit_path="/etc/systemd/system/$RUNTIME_UNIT"
  if [ ! -f "$unit_path" ]; then
    fail "unit 文件不在 $unit_path"
    return 0
  fi
  pass "unit 文件落在 $unit_path"
  assert_eq "unit 文件模式" "644" "$(mode_of "$unit_path")"
  assert_eq "unit 文件属主" "root:root" "$(owner_of "$unit_path")"
  printf '      unit 的 SELinux 上下文    : %s\n' "$(label_of "$unit_path")"
  for want in "User=$RUNTIME_USER" "EnvironmentFile=" "ProtectSystem=strict" "Restart=on-failure"; do
    if grep -q "$want" "$unit_path"; then
      pass "unit 内容含 $want"
    else
      fail "unit 内容缺少 $want"
    fi
  done
  if grep -qE "^# systemd 版本=[0-9]+ 档位=strict$" "$unit_path"; then
    pass "unit 头注释记录了探测到的版本与档位"
  else
    fail "unit 头注释没有记录版本与档位"
  fi
  require_ok "systemctl cat 能读到这个 unit" systemctl cat "$RUNTIME_UNIT"
  assert_eq "unit 已被 enable（开机会起）" "enabled" "$(systemctl is-enabled "$RUNTIME_UNIT" 2>/dev/null)"
  assert_eq "运行用户已被创建" "yes" "$(id -u "$RUNTIME_USER" >/dev/null 2>&1 && printf yes || printf no)"

  log "凭据与环境的落盘：模式、属主、内容"
  assert_eq "/etc/opsd 模式（Prepare 后补穿越位）" "751" "$(mode_of /etc/opsd)"
  assert_eq "/etc/opsd 的 other 权限（只可穿越，不可读/列）" "1" "$(mode_of /etc/opsd | cut -c3)"
  assert_eq "/etc/opsd/apps 模式" "751" "$(mode_of /etc/opsd/apps)"
  assert_eq "非敏感环境文件模式" "600" "$(mode_of "${PROBE_DIR}.env")"
  assert_eq "非敏感环境文件属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "${PROBE_DIR}.env")"
  assert_eq "非敏感环境文件内容" "yes" \
    "$(grep -q '^PLAIN_VALUE="plain"$' "${PROBE_DIR}.env" && printf yes || printf no)"
  assert_eq "敏感环境文件模式" "600" "$(mode_of "${PROBE_DIR}.secrets.env")"
  assert_eq "敏感环境文件属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "${PROBE_DIR}.secrets.env")"
  assert_eq "敏感环境文件里 kind=file 传的是路径" "yes" \
    "$(grep -q "^PROBE_FILE=\"${PROBE_DIR}.secrets/PROBE_FILE\"$" "${PROBE_DIR}.secrets.env" && printf yes || printf no)"
  assert_eq "凭据目录模式" "700" "$(mode_of "${PROBE_DIR}.secrets")"
  assert_eq "凭据目录属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "${PROBE_DIR}.secrets")"
  assert_eq "凭据副本模式" "600" "$(mode_of "${PROBE_DIR}.secrets/PROBE_FILE")"
  assert_eq "凭据副本属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "${PROBE_DIR}.secrets/PROBE_FILE")"
  require_ok "凭据副本与源逐字节一致" cmp /etc/opsd/secrets/probe-file "${PROBE_DIR}.secrets/PROBE_FILE"
  assert_eq "/var/lib/$RUNTIME_APP 属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "/var/lib/$RUNTIME_APP")"
  assert_eq "/var/log/$RUNTIME_APP 属主" "${RUNTIME_USER}:${RUNTIME_USER}" "$(owner_of "/var/log/$RUNTIME_APP")"

  log "凭据路径的穿越链（kind=file 由运行用户自己按路径打开）"
  require_ok "运行用户可以按路径读到自己的凭据文件" \
    runuser -u "$RUNTIME_USER" -- cat "${PROBE_DIR}.secrets/PROBE_FILE"
  require_fail "无关用户 $OTHER_USER 读凭据文件被拒" \
    runuser -u "$OTHER_USER" -- cat "${PROBE_DIR}.secrets/PROBE_FILE"
  require_fail "无关用户 $OTHER_USER 连凭据目录都进不去" \
    runuser -u "$OTHER_USER" -- ls "${PROBE_DIR}.secrets"

  # 幂等：第二次 Prepare 必须成功且档位不变（unit 与权限已经正确，不该被重写）。
  local second tier2
  second=$(q runtime prepare --app "$RUNTIME_APP" --json)
  tier2=$(printf '%s' "$second" | sed -n 's/.*"tier": *"\([^"]*\)".*/\1/p')
  assert_eq "第二次 Prepare 幂等且档位不变" "strict" "$tier2"
}

# ==== 生命周期 ====
check_lifecycle() {
  log "start / health：真机上的 unit 生命周期"

  # ProtectSystem=strict 只放行 unit 声明过的路径。这个目录刻意建成**运行用户可写**：
  # 这样「写不进去」只能归因于 unit 的只读挂载，而不是 DAC 权限。
  require_ok "为 ProtectSystem 断言准备运行用户可写的目录" \
    install -d -m 0750 -o "$RUNTIME_USER" -g "$RUNTIME_USER" "/var/lib/${RUNTIME_APP}-extra"

  local started op_id
  started=$(q runtime start --app "$RUNTIME_APP" --json)
  op_id=$(printf '%s' "$started" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$op_id" ]; then
    fail "runtime start 没有返回 Operation：${started}"
    return 0
  fi
  START_OP_ID="$op_id"
  pass "runtime start 走 Operation（$op_id）"
  if wait_for_status "$op_id" succeeded; then
    pass "start 操作的终态为 succeeded"
  else
    fail "start 操作未在 60 秒内 succeeded：$(q operation get "$op_id" --json | head -c 300)"
    journalctl -u "$RUNTIME_UNIT" -n 20 --no-pager >&2 || true
    return 0
  fi

  assert_eq "unit 状态" "active" "$(systemctl is-active "$RUNTIME_UNIT" 2>/dev/null)"
  if wait_for_ready; then
    pass "runtime health 就绪（连续两次通过）"
  else
    fail "runtime health 未在 30 秒内就绪"
  fi
  # 注意：CLI 与 HTTP API 都**没有** runtime status（只有 health），端口上的
  # Status（「进程本身的状态」）目前只有内部在用——见 report 里的观察。

  # 进程的实际身份与 SELinux 上下文：真机这一档最有价值的一类证据。
  local main_pid
  main_pid=$(systemctl show "$RUNTIME_UNIT" -p MainPID --value 2>/dev/null)
  if [ -n "$main_pid" ] && [ "$main_pid" != "0" ]; then
    pass "unit 的 MainPID=$main_pid"
    assert_eq "托管进程的运行用户" "$RUNTIME_USER" "$(ps -o user= -p "$main_pid" 2>/dev/null | tr -d ' ')"
    local ctx
    ctx=$(tr -d '\0' < "/proc/$main_pid/attr/current" 2>/dev/null || printf '<不可读>')
    printf '      托管进程的 SELinux 上下文: %s\n' "$ctx"
    note "runtime status 未对外暴露：端口的 Status（进程本身状态）已实现且内部在用，但 CLI 与 HTTP API 都只有 health——「进程活着但没就绪」这个状态运维问不出来，记为待定"
  note "托管进程的 SELinux 上下文是 $ctx：本工具不安装 SELinux 策略模块，服务落在默认域里，SELinux 对托管应用的约束等于未生效——这是**未实现**，不是已支持"
  else
    fail "拿不到 unit 的 MainPID"
  fi

  # 凭据逐字节到达进程（只比长度与 sha256，绝不打印明文）。
  local report="/var/lib/$RUNTIME_APP/probe-report.txt"
  if [ -f "$report" ]; then
    pass "探针写出了自述报告"
    local expected_len expected_hash
    expected_len=${#PROBE_ENV_VALUE}
    expected_hash=$(printf '%s' "$PROBE_ENV_VALUE" | sha256sum | cut -d' ' -f1)
    assert_eq "kind=env 凭据的长度与摘要" "$expected_len $expected_hash" \
      "$(sed -n 's/^env:PROBE_ENV len=\([0-9]*\) sha256=\(.*\)$/\1 \2/p' "$report")"
    expected_len=$(wc -c < /etc/opsd/secrets/probe-file | tr -d ' ')
    expected_hash=$(sha256sum /etc/opsd/secrets/probe-file | cut -d' ' -f1)
    assert_eq "kind=file 凭据的长度与摘要" "$expected_len $expected_hash" \
      "$(sed -n 's/^file-env:PROBE_FILE len=\([0-9]*\) sha256=\([^ ]*\).*$/\1 \2/p' "$report")"
    assert_eq "kind=file 传给应用的是绝对路径" "yes" \
      "$(grep -q 'path-absolute=yes' "$report" && printf yes || printf no)"
  else
    fail "探针没有写出报告 $report"
  fi

  # ProtectSystem=strict 在真机上的实际约束。
  assert_eq "unit 生效的 ProtectSystem" "strict" "$(systemctl show "$RUNTIME_UNIT" -p ProtectSystem --value 2>/dev/null)"
  assert_eq "声明允许写入的路径确实可写" "yes" \
    "$(grep -qE "^allow-write .* writable=yes$" "$report" && printf yes || printf no)"
  assert_eq "未声明路径的写入被拒" "yes" \
    "$(grep -qE "^deny-write .* writable=no$" "$report" && printf yes || printf no)"
}

# ==== 停止 ====
check_stop() {
  log "runtime stop：真机上的停止路径"

  local stopped op_id
  stopped=$(q runtime stop --app "$RUNTIME_APP" --json)
  op_id=$(printf '%s' "$stopped" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$op_id" ]; then
    fail "runtime stop 没有返回 Operation：${stopped}"
    return 0
  fi
  if wait_for_status "$op_id" succeeded; then
    pass "stop 操作的终态为 succeeded"
  else
    fail "stop 操作未在 60 秒内 succeeded"
  fi
  assert_eq "停止后 unit 状态" "inactive" "$(systemctl is-active "$RUNTIME_UNIT" 2>/dev/null)"
  # 停服务不改 enable 状态：这是 systemd 的语义，也是我们想要的（重启后还会起来）。
  assert_eq "停止后 unit 仍处于 enabled" "enabled" "$(systemctl is-enabled "$RUNTIME_UNIT" 2>/dev/null)"
}


# ==== 数据库备份（真实主机上的真实 MySQL）====
#
# 只有给了 FRZ_HOST_MYSQL_DSN 才跑：那台主机上跑着生产 MySQL，**是否**用它做备份
# 验证由调用者显式决定。应用账号的凭据经环境变量传入，不落进仓库；
# 这个账号只对一个专用的库有权限（建库与授权是一次性的、在主机上做的）。
check_database() {
  if [ -z "${FRZ_HOST_MYSQL_DSN:-}" ]; then
    printf '\n跳过数据库备份验证：未设置 FRZ_HOST_MYSQL_DSN\n'
    return 0
  fi
  log "数据库备份：备份 → 校验 → 隔离恢复 → 原地恢复（真实 MySQL）"

  # harness 侧的简易 DSN 解析（口令里不得含 : 与 @ 之外的分隔符）。
  local rest creds hostpart
  rest=${FRZ_HOST_MYSQL_DSN#mysql://}
  creds=${rest%%@*}
  hostpart=${rest#*@}
  local db_user=${creds%%:*} db_password=${creds#*:}
  local hostport=${hostpart%%/*} mysql_db=${hostpart#*/}
  local db_host=${hostport%%:*} db_port=${hostport##*:}

  if ! command -v mysqldump >/dev/null 2>&1 || ! command -v mysql >/dev/null 2>&1; then
    fail "主机上没有 mysql / mysqldump 客户端——备份适配器需要它们"
    return 0
  fi
  pass "主机上的客户端：$(mysqldump --version)"

  mysql_sql() {
    MYSQL_PWD="$db_password" mysql --host="$db_host" --port="$db_port" --user="$db_user" \
      --batch --skip-column-names --execute "$1" "$mysql_db"
  }
  probe_present() {
    mysql_sql "SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'frz_probe'"
  }
  fingerprint() {
    if [ "$(probe_present)" != "1" ]; then
      printf 'absent'
      return
    fi
    # `CONCAT` 而不是 `||`：MySQL 里 `||` 是逻辑或，写错了这个指纹永远是 1。
    mysql_sql "SELECT CONCAT(count(*), '|', coalesce(sum(id),0), '|', coalesce(md5(group_concat(v ORDER BY id SEPARATOR ',')),'-')) FROM frz_probe"
  }

  mysql_sql "DROP TABLE IF EXISTS frz_probe; CREATE TABLE frz_probe (id INT PRIMARY KEY, v VARCHAR(64)); INSERT INTO frz_probe VALUES (1,'alpha'),(2,'beta'),(3,'gamma');"
  local before
  before=$(fingerprint)
  if [ -z "$before" ] || [ "$before" = "absent" ]; then
    fail "源库内容没造出来（应用账号对 ${mysql_db} 的权限不足？）"
    return 0
  fi
  pass "源库内容就绪（指纹 ${before}）"

  # 凭据与加密密钥：都只以 SecretRef 出现，文件放在 opsd 允许的目录里、0600。
  printf '%s' "$FRZ_HOST_MYSQL_DSN" > /etc/opsd/secrets/mysql-dsn
  printf '%s' 'frz-verify-backup-key-material' > /etc/opsd/secrets/backup-key
  chown frz-ops:frz-ops /etc/opsd/secrets/mysql-dsn /etc/opsd/secrets/backup-key
  chmod 0600 /etc/opsd/secrets/mysql-dsn /etc/opsd/secrets/backup-key
  assert_eq "DSN 凭据文件模式" "600" "$(mode_of /etc/opsd/secrets/mysql-dsn)"
  assert_eq "加密密钥文件模式" "600" "$(mode_of /etc/opsd/secrets/backup-key)"

  cat > "$FRZ_HOST_DIR/mysql-policy.yaml" <<POLICY
apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: ${MYSQL_POLICY}
resource:
  kind: mysql
  dsnSecret:
    kind: file
    name: /etc/opsd/secrets/mysql-dsn
  database: ${mysql_db}
encoding:
  compression: gzip
  encryption:
    enabled: true
    keySecret:
      kind: file
      name: /etc/opsd/secrets/backup-key
retention:
  keepLast: 3
timeout:
  backupSeconds: 300
  restoreSeconds: 600
POLICY
  chmod 0644 "$FRZ_HOST_DIR/mysql-policy.yaml"

  require_ok "提交备份策略（mysql，加密开启）" opsctl backup policy put --file "$FRZ_HOST_DIR/mysql-policy.yaml"

  local run_out run_id
  run_out=$(q backup run --policy "$MYSQL_POLICY" --json)
  run_id=$(printf '%s' "$run_out" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$run_id" ]; then
    fail "backup run 未返回 operation id：${run_out}"
    return 0
  fi
  if wait_for_status "$run_id" succeeded; then
    pass "备份走 Operation 且成功（${run_id}）"
  else
    fail "备份未成功：$(q operation get "$run_id" --json | head -c 400)"
    return 0
  fi

  # 备份记录里应当带上「谁备的、服务端什么版本」——恢复时的兼容性判断只能靠它。
  assert_eq "备份记录里的服务端版本非空" "yes" \
    "$(q backup list --policy "$MYSQL_POLICY" --json | grep -q '"serverVersion": *"[0-9]' && printf yes || printf no)"

  local backup_id
  backup_id=$(q backup list --policy "$MYSQL_POLICY" --json | sed -n 's/.*"id": *"\(bkp_[^"]*\)".*/\1/p' | head -1)
  if [ -z "$backup_id" ]; then
    fail "backup list 里没有 bkp_ 开头的记录"
    return 0
  fi
  pass "backup list 能列出刚完成的备份（${backup_id}）"

  # 落盘产物的模式与属主：备份根由 opsd 在真实文件系统上按配置强制。
  assert_eq "备份存储根模式" "750" "$(mode_of /var/lib/opsd/backups)"
  assert_eq "备份 blobs 目录模式" "750" "$(mode_of /var/lib/opsd/backups/blobs)"
  local blob
  blob=$(find /var/lib/opsd/backups/blobs -type f | head -1)
  if [ -z "$blob" ]; then
    fail "备份根下没有落盘的 blob"
  else
    assert_eq "备份文件模式" "640" "$(mode_of "$blob")"
    assert_eq "备份文件属主" "root:root" "$(owner_of "$blob")"
  fi

  # D5：跑一次制品 GC，备份内容必须**完好无损**。
  require_ok "制品 GC（会回收它自己根下的孤儿）" opsctl artifact gc
  assert_eq "制品 GC 之后备份 blob 仍在" "yes" \
    "$(test -n "$(find /var/lib/opsd/backups/blobs -type f | head -1)" && printf yes || printf no)"

  local verify_out verify_id
  verify_out=$(q backup verify "$backup_id" --json)
  verify_id=$(printf '%s' "$verify_out" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -n "$verify_id" ] && wait_for_status "$verify_id" succeeded; then
    pass "制品 GC 之后备份仍能校验通过（含存储摘要核对）"
  else
    fail "制品 GC 之后备份校验失败——很可能备份内容被删了"
    return 0
  fi
  assert_eq "校验结果落库为通过" "yes" \
    "$(q backup show "$backup_id" --json | grep -q '"verifiedOk": *true' && printf yes || printf no)"

  # 隔离恢复：证明归档真的可恢复，且**不碰**源库。
  local restore_out restore_id
  restore_out=$(q backup restore "$backup_id" --mode isolated --json)
  restore_id=$(printf '%s' "$restore_out" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -n "$restore_id" ] && wait_for_status "$restore_id" succeeded; then
    pass "隔离恢复成功（走 Operation，${restore_id}）"
  else
    fail "隔离恢复失败：$(q operation get "${restore_id:-}" --json 2>/dev/null | head -c 900)"
    return 0
  fi
  assert_eq "隔离恢复没有碰源库" "$before" "$(fingerprint)"

  # 原地恢复：先把表删掉再恢复，才能证明「真的写回来了」。
  mysql_sql "DROP TABLE IF EXISTS frz_probe"
  assert_eq "删表之后源库指纹变了" "absent" "$(fingerprint)"
  local inplace_out inplace_id
  inplace_out=$(q backup restore "$backup_id" --mode inPlace --confirm --json)
  inplace_id=$(printf '%s' "$inplace_out" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -n "$inplace_id" ] && wait_for_status "$inplace_id" succeeded; then
    pass "原地恢复成功（走 Operation，${inplace_id}）"
  else
    fail "原地恢复失败：$(q operation get "${inplace_id:-}" --json 2>/dev/null | head -c 300)"
    return 0
  fi
  assert_eq "原地恢复后内容与备份时一致" "$before" "$(fingerprint)"

  # 原地恢复必须显式确认：这是本工具里破坏性最强的动作。
  # 看**退出码**而不是错误文案：码是稳定的契约，文案不是。
  local unconfirmed_rc=0 unconfirmed=""
  unconfirmed=$(opsctl backup restore "$backup_id" --mode inPlace --json 2>&1) || unconfirmed_rc=$?
  if [ "$unconfirmed_rc" -eq 25 ]; then
    pass "原地恢复未加确认时被拒（退出码 25 = BACKUP_RESTORE_UNCONFIRMED）"
  else
    fail "原地恢复未加确认时的退出码是 ${unconfirmed_rc}（期望 25）：$(printf '%s' "$unconfirmed" | head -c 200)"
  fi
}

# ==== 重启验证：记录重启前的主机身份 ====
record_boot_identity() {
  log "记录重启前的主机身份（用来证明真的重启过，而不是在检查一台没重启的机器）"
  # CLI 没有 operation list，因此 start 操作的 ID 由 check_lifecycle 记在变量里。
  local start_op=${START_OP_ID:-}
  {
    printf 'boot_id=%s\n' "$(cat /proc/sys/kernel/random/boot_id)"
    printf 'boot_time=%s\n' "$(uptime -s)"
    printf 'recorded_at=%s\n' "$(date -Is)"
    printf 'start_op=%s\n' "$start_op"
  } > "$FRZ_HOST_DIR/boot-before.txt"
  sed 's/^/      /' "$FRZ_HOST_DIR/boot-before.txt"
  pass "已记录重启前的 boot_id、开机时刻与 start 操作的 ID"
}

# ==== 重启验证：重启之后的断言 ====
check_post_reboot() {
  log "重启后：证明真的重启过"

  local before_id after_id before_time upsec
  before_id=$(sed -n 's/^boot_id=//p' "$FRZ_HOST_DIR/boot-before.txt")
  after_id=$(cat /proc/sys/kernel/random/boot_id)
  before_time=$(sed -n 's/^boot_time=//p' "$FRZ_HOST_DIR/boot-before.txt")
  if [ -n "$before_id" ] && [ "$before_id" != "$after_id" ]; then
    pass "boot_id 变了：确实重启过（${before_id:0:8}… → ${after_id:0:8}…）"
  else
    fail "boot_id 没变（${before_id:0:8}…）：这台机没有重启，后面的断言证明不了任何事"
  fi
  printf '      重启前的开机时刻: %s\n' "$before_time"
  printf '      当前开机时刻    : %s\n' "$(uptime -s)"
  printf '      当前运行时长    : %s\n' "$(uptime -p)"
  upsec=$(cut -d. -f1 /proc/uptime)
  if [ "$upsec" -lt 3600 ]; then
    pass "系统运行时长 ${upsec}s，符合「刚重启」"
  else
    fail "系统已运行 ${upsec}s，不像刚重启过"
  fi

  log "重启后：opsd 作为系统服务自己起来了（/run 是 tmpfs，socket 目录要能重建）"
  assert_eq "$OPSD_UNIT 开机自启" "enabled" "$(systemctl is-enabled "$OPSD_UNIT" 2>/dev/null)"
  assert_eq "$OPSD_UNIT 重启后的状态" "active" "$(systemctl is-active "$OPSD_UNIT" 2>/dev/null)"
  require_ok "socket 被重新创建" test -S /run/opsd/opsd.sock
  assert_eq "/run/opsd 模式（RuntimeDirectory 重建）" "750" "$(mode_of /run/opsd)"
  assert_eq "socket 模式（重建后一致）" "660" "$(mode_of /run/opsd/opsd.sock)"
  require_ok "opsd 能应答" opsctl health

  log "重启后：托管的应用自己起来了（靠 enabled + WantedBy=multi-user.target）"
  assert_eq "$RUNTIME_UNIT 开机自启" "enabled" "$(systemctl is-enabled "$RUNTIME_UNIT" 2>/dev/null)"
  assert_eq "$RUNTIME_UNIT 重启后的状态" "active" "$(systemctl is-active "$RUNTIME_UNIT" 2>/dev/null)"
  if wait_for_ready; then
    pass "runtime health 就绪（重启后自己起回了就绪态）"
  else
    fail "重启后 runtime health 未就绪"
    journalctl -u "$RUNTIME_UNIT" -n 20 --no-pager >&2 || true
  fi
  local main_pid ctx
  main_pid=$(systemctl show "$RUNTIME_UNIT" -p MainPID --value 2>/dev/null)
  if [ -n "$main_pid" ] && [ "$main_pid" != "0" ]; then
    assert_eq "托管进程的运行用户（重启后）" "$RUNTIME_USER" "$(ps -o user= -p "$main_pid" 2>/dev/null | tr -d ' ')"
    ctx=$(tr -d '\0' < "/proc/$main_pid/attr/current" 2>/dev/null || printf '<不可读>')
    printf '      重启后托管进程的 SELinux 上下文: %s\n' "$ctx"
  else
    fail "重启后拿不到 unit 的 MainPID"
  fi

  log "重启后：磁盘上的状态没有被冲掉"
  assert_eq "unit 文件仍在" "yes" "$([ -f "/etc/systemd/system/$RUNTIME_UNIT" ] && printf yes || printf no)"
  assert_eq "/etc/opsd 模式（Prepare 补的穿越位应当保留）" "751" "$(mode_of /etc/opsd)"
  assert_eq "/etc/opsd/config.yaml 模式" "600" "$(mode_of /etc/opsd/config.yaml)"
  assert_eq "凭据文件模式" "600" "$(mode_of /etc/opsd/secrets/probe-file)"
  assert_eq "凭据副本仍逐字节一致" "yes" \
    "$(cmp -s /etc/opsd/secrets/probe-file "${PROBE_DIR}.secrets/PROBE_FILE" && printf yes || printf no)"
  assert_eq "opsd 的数据库文件仍在" "yes" "$([ -f /var/lib/opsd/opsd.db ] && printf yes || printf no)"

  # 重启前创建的 Operation 重启后仍可查、终态未变：证明任务引擎的状态是持久的。
  local start_op
  start_op=$(sed -n 's/^start_op=//p' "$FRZ_HOST_DIR/boot-before.txt")
  if [ -n "$start_op" ]; then
    assert_eq "重启前创建的 Operation 仍可查且终态为 succeeded" "succeeded" \
      "$(q operation get "$start_op" --json | sed -n 's/.*"status": *"\([^"]*\)".*/\1/p')"
  fi

  # 凭据在重启后仍然逐字节到达进程：探针在开机时重新跑过一遍，报告是新的。
  local report="/var/lib/$RUNTIME_APP/probe-report.txt"
  if [ -f "$report" ]; then
    local expected_len expected_hash
    expected_len=${#PROBE_ENV_VALUE}
    expected_hash=$(printf '%s' "$PROBE_ENV_VALUE" | sha256sum | cut -d' ' -f1)
    assert_eq "重启后 kind=env 凭据的长度与摘要" "$expected_len $expected_hash" \
      "$(sed -n 's/^env:PROBE_ENV len=\([0-9]*\) sha256=\(.*\)$/\1 \2/p' "$report")"
  else
    fail "重启后探针没有写出报告——托管应用其实没起来"
  fi

  log "重启后：主机上的业务没有被牵连"
  local running
  running=$(docker ps -q | wc -l | tr -d ' ')
  printf '      当前在跑的容器数: %s\n' "$running"
  if [ "$running" -ge 9 ]; then
    pass "业务容器都回来了（${running} 个）"
  else
    fail "业务容器只有 ${running} 个（重启前是 9 个）"
  fi
}

report() {
  printf '\n--- 观察到的、不属于断言结果的部署事实\n'
  if [ "${#FINDINGS[@]}" -eq 0 ]; then
    printf '（无）\n'
  else
    printf '%s\n' "${FINDINGS[@]}"
  fi
  printf '\n%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
  [ "$FAIL_COUNT" -eq 0 ]
}

main() {
  case "${FRZ_HOST_PHASE:-full}" in
    cleanup)
      # 单独一个收尾入口：跑到一半被打断时，「怎么清干净」必须有一条命令，
      # 而不是去读脚本里那段 trap。
      cleanup
      ;;
    prepare)
      # 不清场：这一阶段留下的东西正是要跨重启存活的那个状态。
      env_facts
      check_preconditions
      provision
      check_install_state
      install_opsd_unit
      check_prepare
      check_lifecycle
      record_boot_identity
      printf '\n--- 重启前的一切就绪\n'
      printf '接下来重启这台机，然后跑：FRZ_HOST_PHASE=check bash test/host/run.sh\n'
      printf '%d 项通过，%d 项失败\n' "$PASS_COUNT" "$FAIL_COUNT"
      [ "$FAIL_COUNT" -eq 0 ]
      ;;
    check)
      trap cleanup EXIT
      env_facts
      check_post_reboot
      report
      ;;
    *)
      trap cleanup EXIT
      env_facts
      check_preconditions
      provision
      check_install_state
      install_opsd_unit
      check_prepare
      check_lifecycle
      check_stop
      check_database
      report
      ;;
  esac
}

main "$@"
