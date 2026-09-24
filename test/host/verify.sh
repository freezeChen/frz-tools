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
#   · opsd 本身作为 **systemd 服务**运行的部署形态（含 /run 是 tmpfs 这件事）；
#   · 迭代 3：部署出来的 release 在真机上跑起来、**跨重启存活**（那需要真的重启一台机器）；
#   · 迭代 3c：用主机上真实的 JDK 跑一个真实的 JAR，并断言资源限制在 systemd 与
#     内核两侧都是声明的那个值。
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
# 一条**被 DAC 允许、但应被挂载保护拦住**的写入路径：它归运行用户所有，因此「写不进去」
# 只可能来自 ProtectSystem 的只读挂载。strict 档是整个文件系统只读，legacy 档的 yes 让
# /usr 只读——两档在这里的期望值都是「被拒」，因此它是唯一一条**跨档都成立**的防护断言。
MS_DIR=frz-host-verify-ms
MYSQL_POLICY=frz-verify-mysql
OPSD_UNIT=frz-opsd-verify.service
OPSCTL="$FRZ_HOST_DIR/bin/opsctl"
OPSD="$FRZ_HOST_DIR/bin/opsd"

# 迭代 3 的部署夹具。deploy 那个用来验证「部署出来的 release 能在真机上跑、重启后还在」，
# java 那个用主机上真实的 JDK 跑一个真实的 JAR（并压到资源限制与解释器预检）。
DEPLOY_APP=frz-dep
DEPLOY_UNIT=${DEPLOY_APP}.service
DEPLOY_PORT=${FRZ_DEPLOY_PORT:-28582}
DEPLOY_VERSION=1.0.0
JAVA_APP=frz-java
JAVA_UNIT=${JAVA_APP}.service
JAVA_PORT=${FRZ_JAVA_PORT:-28583}
JAVA_BAD_APP=frz-javabad
JAVA_HOME=${FRZ_HOST_JAVA_HOME:-/opt/jdk-17.0.1}
JAVA_BUILD_DIR=/opt/frz-ops/java-fixture
JAVA_MARKER=java-marker-ok
JAVA_MEMORY_MAX_BYTES=536870912

# 迭代 3 的夹具应用：清理与前置检查都按这张表走，免得新增一个就漏一处。
HOST_APPS=("$DEPLOY_APP" "$JAVA_APP" "$JAVA_BAD_APP")

START_OP_ID=""
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
# 前置检查的结论。1 表示「这台机器上本来就有不属于本轮的东西」——此时本轮**既不建也不删**。
PRECONDITION_FAILED=0
# 档位相关的期望值：由 check_prepare 探测到的 systemd 版本填好，后面的断言一律读它。
EXPECT_TIER=""
EXPECT_PROTECT_SYSTEM=""
EXPECT_FS_DIRECTIVE=""
EXPECT_STDOUT=""
EXPECT_MEMORY_PROP=""
FINDINGS=()

log()  { printf '\n--- %s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*" >&2; }
# 跳过必须单独计数、单独打印：它**不是通过**。把「主机上没有 JDK 所以没验」混进
# 通过数里，等于用数字谎报覆盖面——这条纪律在验证脚本里比在别处更要紧。
skip() { SKIP_COUNT=$((SKIP_COUNT + 1)); printf 'SKIP  %s\n' "$*"; }
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

# unit_prop 取一个 unit 的某个属性值。
#
# **不能用 `systemctl show --value`**：那个开关是 systemd 230 才加的，而本工具承诺支持到
# 219——在这台 CentOS 7（219）上它会直接报 `unrecognized option '--value'`，于是每个用到
# 它的断言都会拿到一段错误文本当值。产品代码早就刻意避开了它（见 systemd 适配器里
# unitStatus 的注释），harness 这里跟上同一条规则：解析 `Prop=value` 那一行。
# 这个写法在 219 与 255 上都能用（两边都实测过）。
unit_prop() { # unit 属性名
  # 结尾的 `|| true` 是必需的：脚本开着 pipefail，systemctl 失败时整条管道会返回非零，
  # 而调用点几乎都是赋值语句——那会让整个脚本在 set -e 下退出。
  systemctl show "$1" -p "$2" 2>/dev/null | sed -n "s/^$2=//p" | head -1 || true
}

# memory_limit_of / cpu_quota_of：**内核侧**真正生效的那个值。
#
# cgroup v1 与 v2 的文件名与目录布局都不同（v1 是 /sys/fs/cgroup/<控制器>/… 下、v2 是
# /sys/fs/cgroup/<cgroup>/memory.max 与 cpu.max），因此不能写死一条路径。判据用文件是否存在，
# 而不是猜版本：这台 CentOS 7 是 v1，容器与新版主机是 v2，两边都已实测。
memory_limit_of() { # unit
  local cg
  cg=$(unit_prop "$1" ControlGroup)
  [ -z "$cg" ] && return 0
  if [ -f "/sys/fs/cgroup/memory${cg}/memory.limit_in_bytes" ]; then
    cat "/sys/fs/cgroup/memory${cg}/memory.limit_in_bytes"
  else
    cat "/sys/fs/cgroup${cg}/memory.max" 2>/dev/null
  fi
}

cpu_quota_of() { # unit —— 输出 "<quota> <period>"，两代的语义与数字都一致
  local cg
  cg=$(unit_prop "$1" ControlGroup)
  [ -z "$cg" ] && return 0
  if [ -f "/sys/fs/cgroup/cpu${cg}/cpu.cfs_quota_us" ]; then
    printf '%s %s' "$(cat "/sys/fs/cgroup/cpu${cg}/cpu.cfs_quota_us")" \
      "$(cat "/sys/fs/cgroup/cpu${cg}/cpu.cfs_period_us")"
  else
    cat "/sys/fs/cgroup${cg}/cpu.max" 2>/dev/null
  fi
}

# container_count 报告本机在跑的容器数；**没有 docker（或守护进程不可用）时返回一个说明性
# 字符串**，而不是 0。
#
# 三个刻意写法：①不必用管道——脚本开着 `set -o pipefail`，`docker` 不存在时
# `docker ps -q | wc -l` 整条管道返回 127，而调用点是赋值语句，**整个 check 阶段会因此退出**
# （2026-09-24 在一台没装 docker 的 CentOS 7 上真的踩到了：跨重启的那些断言全绿，脚本却以
# 127 收场，日志里只剩收尾那几行，看起来像「有断言没跑」）；②`docker ps` 失败（守护进程没跑）
# 与「没有 docker」都不是数字，不能拿去和数字比；③循环计数而不是 `wc -l`，因为管道正是要避开的。
container_count() {
  if ! command -v docker >/dev/null 2>&1; then
    printf '无 docker'
    return 0
  fi
  local listing
  if ! listing=$(docker ps -q 2>/dev/null); then
    printf 'docker 不可用'
    return 0
  fi
  local count=0 line
  while IFS= read -r line; do
    [ -n "$line" ] && count=$((count + 1))
  done <<< "$listing"
  printf '%s' "$count"
}

# business_java_count 数一数**主机自有的** JVM（排除本轮夹具用户启动的那些）。
#
# 它只数、只读，绝不按名字杀任何进程：那台主机上跑着不在 systemd 下、也不在容器里的业务
# JVM，而「名字匹配」是最容易误伤它们的动作（2026-09-24 的一次 `pkill -x java` 就误杀过）。
# 重启前后的这个数字要相等，是「重启没有牵连业务」这句话的判据。
business_java_count() {
  local pid user count=0
  for pid in $(pgrep -x java 2>/dev/null || true); do
    user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ' || true)
    case " frz-ops ${HOST_APPS[*]} " in
      *" $user "*) continue ;;
    esac
    count=$((count + 1))
  done
  printf '%s' "$count"
}

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
#
# 例外一条，而且这条很重要：**前置检查失败时什么都不删**。那条路径上的判断是
# 「这台机器本来就不干净」，而清理的动作是「无条件删掉这些路径」——两者放在一起，
# 就等于把「别人的东西还在」这件事变成了「那就删掉它」。2026-09-24 真的踩到了：
# /opt/opsd 存在导致前置失败，紧接着的 cleanup 把它删了。
cleanup() {
  if [ "${FRZ_HOST_KEEP:-0}" = "1" ]; then
    printf '\n保留了现场（FRZ_HOST_KEEP=1）：/etc/opsd /var/lib/opsd /var/log/opsd /run/opsd /opt/frz-ops /opt/opsd %s\n' "$FRZ_HOST_DIR"
    return 0
  fi
  if [ "${PRECONDITION_FAILED:-0}" = "1" ]; then
    printf '\n--- 收尾：**跳过**\n'
    printf '前置检查失败（这台机器上本来就有不属于本轮的东西），因此什么都不删：\n'
    printf '按定义本轮还没创建任何东西，删下去只会删掉别人的。请先查清那些路径是谁的，\n'
    printf '再决定要不要手工清理，然后重跑。\n'
    return 0
  fi

  printf '\n--- 收尾（删除本轮创建的一切）\n'
  systemctl stop "$OPSD_UNIT" >/dev/null 2>&1 || true
  systemctl stop "$RUNTIME_UNIT" >/dev/null 2>&1 || true
  systemctl disable "$OPSD_UNIT" >/dev/null 2>&1 || true
  systemctl disable "$RUNTIME_UNIT" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/$OPSD_UNIT" "/etc/systemd/system/$RUNTIME_UNIT"
  for app in "${HOST_APPS[@]}"; do
    systemctl stop "${app}.service" >/dev/null 2>&1 || true
    systemctl disable "${app}.service" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/${app}.service"
    userdel "$app" >/dev/null 2>&1 || true
    groupdel "$app" >/dev/null 2>&1 || true
    rm -rf "/var/lib/$app" "/var/log/$app" "/etc/opsd/apps/$app"*
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl reset-failed >/dev/null 2>&1 || true

  userdel "$RUNTIME_USER" >/dev/null 2>&1 || true
  userdel "$OTHER_USER" >/dev/null 2>&1 || true
  userdel frz-ops >/dev/null 2>&1 || true
  groupdel "$RUNTIME_USER" >/dev/null 2>&1 || true
  groupdel "$OTHER_USER" >/dev/null 2>&1 || true
  groupdel frz-ops >/dev/null 2>&1 || true

  # /opt/opsd 是 release 目录的父目录（domain.ReleaseDir 的约定路径）。它由
  # RuntimeAdapter 的 Prepare 与 ReleaseAdapter 的物化共同建出来，因此也必须由这里删掉
  # ——生产机上留一棵没人认领的目录树是最不该发生的事。cleanup 敢直接 rm 是因为
  # check_preconditions 已经断言过它本轮之前不存在。
  rm -rf /etc/opsd /var/lib/opsd /var/log/opsd /run/opsd /opt/frz-ops /opt/opsd
  rm -rf "/var/lib/$RUNTIME_APP" "/var/log/$RUNTIME_APP" "/var/lib/${RUNTIME_APP}-extra" "/usr/local/$MS_DIR"
  rm -rf "$FRZ_HOST_DIR"

  printf '已删除：用户/组 frz-ops、%s、%s；unit %s 与 %s；目录 /etc/opsd、/var/lib/opsd、\n' \
    "$RUNTIME_USER" "$OTHER_USER" "$OPSD_UNIT" "$RUNTIME_UNIT"
  printf '        /var/log/opsd、/run/opsd、/opt/frz-ops、/opt/opsd、/var/lib/%s、/var/log/%s、%s\n' \
    "$RUNTIME_APP" "$RUNTIME_APP" "$FRZ_HOST_DIR"
  printf '        以及迭代 3 的三个夹具应用：%s（用户、unit、/var/lib 与 /var/log 下的目录）\n' \
    "${HOST_APPS[*]}"
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
  for user in frz-ops "$RUNTIME_USER" "$OTHER_USER" "${HOST_APPS[@]}"; do
    id "$user" >/dev/null 2>&1 && { fail "$user 用户已存在，无法从「干净主机」开始"; dirty=1; }
  done
  # /opt/opsd 在列表里是**安全前提**而不是洁癖：cleanup 会删掉整棵 /opt/opsd，因此必须
  # 先确认它本轮之前不存在——否则我们就在一台有既有 release 的机器上删了别人的目录树。
  for path in /etc/opsd /var/lib/opsd /run/opsd /opt/frz-ops /opt/opsd "/var/lib/$RUNTIME_APP" "/usr/local/$MS_DIR"; do
    [ -e "$path" ] && { fail "$path 已存在，无法从「干净主机」开始"; dirty=1; }
  done
  [ "$dirty" -eq 0 ] && pass "本轮涉及的用户与目录此时都不存在"

  for port in "$RUNTIME_PORT" "$DEPLOY_PORT" "$JAVA_PORT"; do
    ss -lntH "sport = :$port" 2>/dev/null | grep -q . && { fail "端口 $port 已被占用"; dirty=1; }
  done
  [ "$dirty" -eq 0 ] && pass "三个夹具端口（$RUNTIME_PORT / $DEPLOY_PORT / $JAVA_PORT）都空闲"

  # 这一条让 cleanup 知道「机器不干净」，从而**什么都不删**（见 cleanup 的注释）。
  PRECONDITION_FAILED=$dirty
  return 0
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
  printf '      opsd 进程的 SELinux 上下文: %s\n' "$(tr -d '\0' < "/proc/$(unit_prop "$OPSD_UNIT" MainPID)/attr/current" 2>/dev/null || printf '<不可读>')"
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
    - --deny-write
    - /usr/local/${MS_DIR}/out
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
  if [ -z "$version" ]; then
    fail "Prepare 没有返回 systemd 版本"
    return 0
  fi
  # 档位由探测到的版本决定（两档的分界是 240，见 unitfile.TierFor）。这台主机上跑的是
  # 哪一档，决定了下面**每一条** unit 内容断言与防护断言——把期望值放在这里算一次，
  # 后面一律读它，避免每条断言各自判断一次、各自漂移。
  if [ "$version" -ge 240 ]; then
    EXPECT_TIER=strict
    EXPECT_PROTECT_SYSTEM=strict
    EXPECT_FS_DIRECTIVE=ReadWritePaths
    EXPECT_STDOUT=append
    EXPECT_MEMORY_PROP=MemoryMax
  else
    EXPECT_TIER=legacy
    EXPECT_PROTECT_SYSTEM=yes
    EXPECT_FS_DIRECTIVE=ReadWriteDirectories
    EXPECT_STDOUT=journal
    # 231 之前那一档的拼法（与 MemoryMax= 同义）。2026-09-24 在真实 219 上实测：
    # MemoryMax= 毫无效果，MemoryLimit= 真的落到 cgroup。
    EXPECT_MEMORY_PROP=MemoryLimit
  fi
  assert_eq "Prepare 返回的 unit 档位" "$EXPECT_TIER" "$tier"
  pass "探测到的 systemd 版本 ${version} → 档位 $EXPECT_TIER"
  if [ "$EXPECT_TIER" = "legacy" ]; then
    note "这台主机落在 legacy 档（systemd 219～239）：ProtectSystem 只能到 yes、日志走 journal、内存上限用 MemoryLimit= 表达。这些是**语义损失**，不是缺陷——见 unit.Degradations()"
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
  for want in "User=$RUNTIME_USER" "EnvironmentFile=" \
    "ProtectSystem=$EXPECT_PROTECT_SYSTEM" "$EXPECT_FS_DIRECTIVE=" "Restart=on-failure"; do
    if grep -q "$want" "$unit_path"; then
      pass "unit 内容含 $want"
    else
      fail "unit 内容缺少 $want"
    fi
  done
  # 另一档的指令一个都不能出现：那些取值在旧档上要么不被接受（unit 加载失败）、
  # 要么被忽略，而「被忽略」意味着 unit 看起来有约束、实际没有。
  if [ "$EXPECT_TIER" = "legacy" ]; then
    # **只看指令行**：legacy 的 unit 头注释里就写着「本机无法使用 ProtectSystem=strict、
    # ReadWritePaths= 与 StandardOutput=append:」，拿整个文件去 grep 只会命中那段注释，
    # 断言就退化成文字游戏（Go 侧的同名断言有 directiveLines 做同一件事）。
    local directives
    directives=$(grep -v '^#' "$unit_path")
    for forbidden in "ProtectSystem=strict" "ReadWritePaths=" "StandardOutput=append:" "MemoryMax="; do
      if printf '%s' "$directives" | grep -q "$forbidden"; then
        fail "legacy 档的 unit 不得含 $forbidden（本机不支持或无效）"
      else
        pass "legacy 档的 unit 正确避开了 $forbidden"
      fi
    done
  fi
  if grep -qE "^# systemd 版本=[0-9]+ 档位=$EXPECT_TIER$" "$unit_path"; then
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
  assert_eq "第二次 Prepare 幂等且档位不变" "$EXPECT_TIER" "$tier2"
}

# ==== 生命周期 ====
check_lifecycle() {
  log "start / health：真机上的 unit 生命周期"

  # ProtectSystem=strict 只放行 unit 声明过的路径。这个目录刻意建成**运行用户可写**：
  # 这样「写不进去」只能归因于 unit 的只读挂载，而不是 DAC 权限。
  require_ok "为 ProtectSystem 断言准备运行用户可写的目录" \
    install -d -m 0750 -o "$RUNTIME_USER" -g "$RUNTIME_USER" "/var/lib/${RUNTIME_APP}-extra"
  # 这一条归运行用户所有（DAC 允许写），因此探针报「写不进去」只可能来自只读挂载。
  require_ok "为挂载保护断言准备运行用户可写的 /usr/local 子目录" \
    install -d -m 0750 -o "$RUNTIME_USER" -g "$RUNTIME_USER" "/usr/local/$MS_DIR"

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
  main_pid=$(unit_prop "$RUNTIME_UNIT" MainPID)
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
  assert_eq "unit 生效的 ProtectSystem（本档期望值）" "$EXPECT_PROTECT_SYSTEM" "$(unit_prop "$RUNTIME_UNIT" ProtectSystem)"
  assert_eq "声明允许写入的路径确实可写" "yes" \
    "$(grep -qE "^allow-write .* writable=yes$" "$report" && printf yes || printf no)"
  # 这一条是**跨档都成立**的防护证据：路径归运行用户所有（DAC 放行），唯一能拦住它的是
  # ProtectSystem 的只读挂载——strict 档整个文件系统只读，legacy 档的 yes 让 /usr 只读。
  assert_eq "被挂载保护的路径写不进去（两档都应当如此）" "yes" \
    "$(grep -qE "^deny-write /usr/local/$MS_DIR/.* writable=no$" "$report" && printf yes || printf no)"

  # /var/lib 下那条探测的是**另一件事**，而它的期望值随档位不同：strict 档连未声明的
  # /var 路径也只读；legacy 档的 ProtectSystem=yes 只保护 /usr，/var 完全可写。
  # 因此 legacy 档这里断言的是**相反**的事实，并把它记成语义损失——假装它也被拦住，
  # 就等于用一个绿色断言掩盖「这一档的隔离弱得多」。
  if [ "$EXPECT_TIER" = "strict" ]; then
    assert_eq "未声明的 /var 路径的写入被拒（strict）" "yes" \
      "$(grep -qE "^deny-write /var/lib/${RUNTIME_APP}-extra/.* writable=no$" "$report" && printf yes || printf no)"
  else
    assert_eq "未声明的 /var 路径在 legacy 档**可写**（ProtectSystem=yes 只保护 /usr）" "yes" \
      "$(grep -qE "^deny-write /var/lib/${RUNTIME_APP}-extra/.* writable=yes$" "$report" && printf yes || printf no)"
    note "legacy 档的隔离弱于 strict 档：ProtectSystem=yes 只让 /usr 只读，未声明的 /var 路径仍可写（已实测）。工具仍然只声明它需要的路径，但内核不替我们拦——这条写进 unit.Degradations()"
  fi
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

# ==== 迭代 3：部署（真实主机）====
#
# 容器 harness 已经把「部署 / 换版本 / 回滚 / 保留策略」用真 systemd 与真进程验过一遍，
# 这里**刻意不重复**那些断言，只证明容器给不出的东西：
#   · release 目录在真实 useradd/chown 下落盘的模式与属主、以及 SELinux 上下文；
#   · 相对 argv[0] 解析到的确实是 release 里那个文件（不是运维手工放的）；
#   · 部署出来的版本**跨重启存活**（那需要真的重启一台机器，见 check_post_reboot）。
check_deploy_host() {
  log "迭代 3：部署一个 release（真机上的 useradd / chown / SELinux 落盘结果）"

  local app=$DEPLOY_APP port=$DEPLOY_PORT
  require_ok "注册应用" opsctl app create "$app"

  local uploaded artifact_id
  uploaded=$(q artifact put /opt/frz-ops/frz-probe --media-type application/octet-stream --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$artifact_id" ]; then
    fail "制品上传失败：${uploaded}"
    return 0
  fi

  # argv[0] 写成**相对路径**：它必须解析到 release 里的那个文件，而不是主机上别处的同名文件。
  cat > "$FRZ_HOST_DIR/deploy.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: go
artifact:
  id: ${artifact_id}
  version: ${DEPLOY_VERSION}
  fileName: bin/frz-probe
  unpack:
    strategy: none
exec:
  argv:
    - bin/frz-probe
    - --report
    - /var/lib/${app}/probe-report.txt
    - --listen
    - 127.0.0.1:${port}
  workingDirectory: /var/lib/${app}
  runUser: ${app}
  ports:
    - ${port}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${port}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 30
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/${app}
release:
  keepLast: 3
MANIFEST
  chmod 0644 "$FRZ_HOST_DIR/deploy.yaml"

  local out id
  out=$(opsctl app deploy --app "$app" --file "$FRZ_HOST_DIR/deploy.yaml" --json 2>&1) || true
  id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "$id" ]; then
    fail "部署没有返回 operation id：${out}"
    return 0
  fi
  if ! wait_for_status "$id" succeeded; then
    fail "部署未成功：$(q operation logs "$id" --json | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | tail -3 | tr '\n' ' ')"
    return 0
  fi
  pass "app deploy 走 Operation 且成功（${DEPLOY_VERSION}）"

  assert_eq "$DEPLOY_UNIT 状态" "active" "$(systemctl is-active "$DEPLOY_UNIT" 2>/dev/null)"
  # enable 是**部署的一部分**：不 enable，重启后这个版本不会自己起来。
  assert_eq "$DEPLOY_UNIT 开机自启" "enabled" "$(systemctl is-enabled "$DEPLOY_UNIT" 2>/dev/null)"
  assert_eq "部署后端口在监听" "yes" "$(ss -lntH "sport = :$port" | grep -q . && printf yes || printf no)"

  local releases="/opt/opsd/apps/$app/releases"
  local current="$releases/current" resolved
  resolved=$(readlink -f "$current")
  assert_eq "current 是符号链接" "yes" "$([ -L "$current" ] && printf yes || printf no)"
  assert_eq "current 指向该应用的 releases 子树里的一个目录" "yes" \
    "$(case "$resolved" in "$releases"/rel_*) printf yes ;; *) printf no ;; esac)"

  # release 目录的模式与属主是**真机才有意义**的那一类断言：它经过真实的 useradd/chown。
  assert_eq "release 目录模式" "750" "$(stat -L -c '%a' "$current" 2>/dev/null || true)"
  assert_eq "release 目录属主" "${app}:${app}" "$(stat -L -c '%U:%G' "$current" 2>/dev/null || true)"
  assert_eq "release 根模式" "750" "$(stat -c '%a' "$releases" 2>/dev/null || true)"
  assert_eq "release 根属主" "${app}:${app}" "$(stat -c '%U:%G' "$releases" 2>/dev/null || true)"
  # 相对 argv[0] 解析到的那个文件：它就住在 release 里，且是可执行的。
  assert_eq "argv[0] 解析到的文件在 release 里且可执行" "yes" \
    "$([ -x "$current/bin/frz-probe" ] && printf yes || printf no)"

  local main_pid
  main_pid=$(unit_prop "$DEPLOY_UNIT" MainPID)
  if [ -n "$main_pid" ] && [ "$main_pid" != "0" ]; then
    assert_eq "托管进程的运行用户" "$app" "$(ps -o user= -p "$main_pid" 2>/dev/null | tr -d ' ')"
    assert_eq "托管进程的工作目录解析到 release 目录" "yes" \
      "$([ "$(readlink "/proc/$main_pid/cwd" 2>/dev/null)" = "/var/lib/$app" ] && printf yes || printf no)"
    printf '      托管进程的 SELinux 上下文: %s\n' \
      "$(tr -d '\0' < "/proc/$main_pid/attr/current" 2>/dev/null || printf '<不可读>')"
  else
    fail "拿不到 $DEPLOY_UNIT 的 MainPID"
  fi
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce 2>/dev/null)" = "Enforcing" ]; then
    printf '      release 目录的 SELinux 上下文: %s\n' "$(label_of "$resolved")"
    printf '      current 链接的 SELinux 上下文: %s\n' "$(label_of "$current")"
    printf '      unit 文件的 SELinux 上下文   : %s\n' "$(label_of "/etc/systemd/system/$DEPLOY_UNIT")"
  fi
  assert_eq "探针在 release 里跑起来了（写出了报告）" "yes" \
    "$([ -f "/var/lib/$app/probe-report.txt" ] && printf yes || printf no)"
}

# ==== 迭代 3c：真机上的 JVM 与资源限制 ====
#
# 这一节的证据 Go 探针给不出来：`-jar app.jar` 能按工作目录解析到制品，说明 systemd 的
# WorkingDirectory= 真的交到了 JVM 手里；`Runtime.maxMemory()` 说明 JVM 真的按 `-Xmx` 设了
# 堆上限；而这两件事同时成立，就说明 argv 里的参数在被改写的情况下根本起不来。
#
# 缺 JDK 时**跳过并显式计数**：跳过不是通过。
check_java_and_resources() {
  log "迭代 3c：真机上的 JVM 部署、资源限制与解释器预检"

  local java="$JAVA_HOME/bin/java" javac="$JAVA_HOME/bin/javac" jar="$JAVA_HOME/bin/jar"
  if [ ! -x "$java" ] || [ ! -x "$javac" ] || [ ! -x "$jar" ]; then
    skip "$JAVA_HOME 下没有可用的 JDK（java/javac/jar 至少缺一个）：java 运行时与资源限制的真机验证本轮**未做**"
    return 0
  fi
  printf '      JDK                      : %s（%s）\n' "$JAVA_HOME" "$("$java" -version 2>&1 | head -1)"
  pass "主机上有可用的 JDK，java 运行时这一档可以验"

  # 在主机上用真 JDK 编译打包：这一档要证明的正是「真 JVM 能跑起来」，
  # 在开发机上交叉编译一个 jar 反而绕开了要验的东西。
  rm -rf "$JAVA_BUILD_DIR"
  install -d -m 0755 "$JAVA_BUILD_DIR"
  cp "$FRZ_HOST_DIR/JavaProbe.java" "$JAVA_BUILD_DIR/"
  if ! (cd "$JAVA_BUILD_DIR" && "$javac" -d classes JavaProbe.java &&
    "$jar" --create --file app.jar --main-class JavaProbe -C classes .) >"$JAVA_BUILD_DIR/build.log" 2>&1; then
    fail "在主机上编译打包 JAR 失败：$(tail -3 "$JAVA_BUILD_DIR/build.log" | tr '\n' ' ')"
    return 0
  fi
  pass "用主机上的 javac/jar 构建出制品 app.jar（$(stat -c %s "$JAVA_BUILD_DIR/app.jar") 字节）"

  local app=$JAVA_APP port=$JAVA_PORT
  require_ok "注册 java 应用" opsctl app create "$app"

  local uploaded artifact_id
  uploaded=$(q artifact put "$JAVA_BUILD_DIR/app.jar" --media-type application/java-archive --json)
  artifact_id=$(printf '%s' "${uploaded}" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
  if [ -z "$artifact_id" ]; then
    fail "JAR 制品上传失败：${uploaded}"
    return 0
  fi

  # workingDirectory 写成 release 的 current —— 这一条既是 Java 的推荐写法（`-jar app.jar`
  # 按工作目录解析），也顺带压到「Prepare 不得建 releases 子树内部」那条分工：
  # 真被建成实体目录的话，符号链接切换会失败，这次部署根本到不了 active。
  cat > "$FRZ_HOST_DIR/java.yaml" <<MANIFEST
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: ${app}
runtime: java
artifact:
  id: ${artifact_id}
  version: 1.0.0
  fileName: app.jar
  unpack:
    strategy: none
exec:
  argv:
    - ${JAVA_HOME}/bin/java
    - -Xmx256m
    - -jar
    - app.jar
    - --listen
    - 127.0.0.1:${port}
  workingDirectory: /opt/opsd/apps/${app}/releases/current
  runUser: ${app}
  environment:
    PROBE_MARKER: ${JAVA_MARKER}
  ports:
    - ${port}
health:
  readiness:
    type: tcp
    target: 127.0.0.1:${port}
    consecutiveSuccesses: 2
  startTimeoutSeconds: 60
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/${app}
resources:
  cpuQuotaPercent: 200
  memoryMaxBytes: ${JAVA_MEMORY_MAX_BYTES}
release:
  keepLast: 3
MANIFEST
  chmod 0644 "$FRZ_HOST_DIR/java.yaml"

  local out id
  out=$(opsctl app deploy --app "$app" --file "$FRZ_HOST_DIR/java.yaml" --json 2>&1) || true
  id=$(printf '%s' "${out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "$id" ]; then
    fail "java 部署没有返回 operation id：${out}"
    return 0
  fi
  if ! wait_for_status "$id" succeeded; then
    fail "java 部署未成功：$(q operation logs "$id" --json | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | tail -3 | tr '\n' ' ')"
    journalctl -u "$JAVA_UNIT" -n 20 --no-pager >&2 || true
    return 0
  fi
  pass "真 JVM 部署成功（runtime: java，制品是单个 JAR）"

  assert_eq "$JAVA_UNIT 状态" "active" "$(systemctl is-active "$JAVA_UNIT" 2>/dev/null)"
  assert_eq "java 应用端口在监听" "yes" "$(ss -lntH "sport = :$port" | grep -q . && printf yes || printf no)"

  local current="/opt/opsd/apps/$app/releases/current"
  local report="$current/java-probe-report.txt"
  if [ ! -f "$report" ]; then
    fail "JVM 没有写出报告 $report（WorkingDirectory 不可写，或 app.jar 没被找到）"
  else
    pass "JVM 把报告写进了自己的工作目录（= release 的 current）"
    local line
    line_of() { sed -n "s/^$1=//p" "$report"; }
    assert_eq "JVM 报告的运行用户" "$app" "$(line_of user)"
    # 工作目录必须解析到 current 指向的那个 release 目录——`-jar app.jar` 能成功就是它给的。
    assert_eq "JVM 报告的工作目录" "$(readlink -f "$current")" "$(line_of dir)"
    assert_eq "EnvironmentFile 里的非敏感变量到达了 JVM" "$JAVA_MARKER" "$(line_of marker)"
    printf '      JVM 版本                 : %s\n' "$(line_of java)"
    # `-Xmx256m` 真的生效：堆上限落在 256 MiB 与它下方一点之间。
    #
    # 两侧都要卡：只卡上界的话，「参数被忽略」这种情况会漏掉——JDK 10 起 JVM 会读
    # cgroup 的内存上限并按 25% 取默认堆，那台机器上的 MemoryMax=512M 会给到 128 MiB，
    # 于是 `-Xmx` 失效反而看起来「上限更小、更安全」。只卡下界则相反。
    local max_memory
    max_memory=$(line_of maxMemory)
    assert_eq "-Xmx256m 生效（堆上限在 [192 MiB, 256 MiB] 之间）" "yes" \
      "$([ "${max_memory:-0}" -le 268435456 ] && [ "${max_memory:-0}" -ge 201326592 ] && printf yes || printf no)"
    printf '      JVM 报告的堆上限         : %s 字节\n' "${max_memory:-<无>}"

    local main_pid
    main_pid=$(unit_prop "$JAVA_UNIT" MainPID)
    if [ -n "$main_pid" ] && [ "$main_pid" != "0" ]; then
      assert_eq "托管 JVM 的运行用户" "$app" "$(ps -o user= -p "$main_pid" 2>/dev/null | tr -d ' ')"
      # 最强的一条 argv 证据：进程**实际收**到的参数逐个元素与 manifest 一致
      # （D4：只解析 argv[0]，其余一个字节都不动）。
      local want_argv got_argv
      want_argv=$(printf '%s\n' "$JAVA_HOME/bin/java" "-Xmx256m" "-jar" "app.jar" \
        "--listen" "127.0.0.1:$port")
      got_argv=$(tr '\0' '\n' < "/proc/$main_pid/cmdline" | sed -e '/^$/d')
      assert_eq "JVM 收到的 argv 与 manifest 逐元素一致" "$want_argv" "$got_argv"
      printf '      托管 JVM 的 SELinux 上下文: %s\n' \
        "$(tr -d '\0' < "/proc/$main_pid/attr/current" 2>/dev/null || printf '<不可读>')"
    else
      fail "拿不到 $JAVA_UNIT 的 MainPID"
    fi
  fi

  # 资源限制：systemd 采纳了，内核按它限制。
  assert_eq "unit 里的 CPUQuota" "CPUQuota=200%" \
    "$(grep -x 'CPUQuota=200%' "/etc/systemd/system/$JAVA_UNIT" || true)"
  # 指令名随档位：strict 用 MemoryMax=，legacy 用同义的 MemoryLimit=（见 unitfile.RenderUnit）。
  assert_eq "unit 里的内存上限写的是原始字节数（$EXPECT_MEMORY_PROP）" "$EXPECT_MEMORY_PROP=${JAVA_MEMORY_MAX_BYTES}" \
    "$(grep -x "$EXPECT_MEMORY_PROP=${JAVA_MEMORY_MAX_BYTES}" "/etc/systemd/system/$JAVA_UNIT" || true)"
  assert_eq "systemd 报告的内存上限（$EXPECT_MEMORY_PROP）" "$JAVA_MEMORY_MAX_BYTES" \
    "$(unit_prop "$JAVA_UNIT" "$EXPECT_MEMORY_PROP")"
  assert_eq "systemctl show 报告的 CPUQuotaPerSecUSec" "2s" \
    "$(unit_prop "$JAVA_UNIT" CPUQuotaPerSecUSec)"
  if [ -z "$(unit_prop "$JAVA_UNIT" ControlGroup)" ]; then
    fail "拿不到 unit 的 cgroup 路径（ControlGroup 为空）"
  else
    # 内核侧的值才是「限制真的生效」的判据（cgroup v1/v2 的读法见 memory_limit_of）。
    assert_eq "cgroup 里的内存上限（限制真正生效的地方）" "$JAVA_MEMORY_MAX_BYTES" \
      "$(memory_limit_of "$JAVA_UNIT")"
    assert_eq "cgroup 里的 CPU 配额（200% = 200000/100000）" "200000 100000" \
      "$(cpu_quota_of "$JAVA_UNIT")"
  fi

  # 解释器预检：JDK 路径写错时必须在**部署之前**、以 MANIFEST_INVALID 失败，
  # 而不是等到 unit 起来、进程退出、报「就绪超时」。后者是这条检查存在前的表现。
  log "解释器预检：JDK 路径写错时部署应当立刻失败，且不留下任何副作用"

  local bad_app=$JAVA_BAD_APP
  require_ok "注册应用" opsctl app create "$bad_app"
  sed -e "s#application: ${JAVA_APP}\$#application: ${bad_app}#" \
    -e "s#${JAVA_HOME}/bin/java#/opt/jdk-does-not-exist/bin/java#" \
    -e "s#/var/log/${JAVA_APP}#/var/log/${bad_app}#" \
    -e "s#runUser: ${JAVA_APP}\$#runUser: ${bad_app}#" \
    -e "s#/opt/opsd/apps/${JAVA_APP}/#/opt/opsd/apps/${bad_app}/#" \
    "$FRZ_HOST_DIR/java.yaml" > "$FRZ_HOST_DIR/java-bad.yaml"
  chmod 0644 "$FRZ_HOST_DIR/java-bad.yaml"

  local bad_out bad_id
  bad_out=$(opsctl app deploy --app "$bad_app" --file "$FRZ_HOST_DIR/java-bad.yaml" --json 2>&1) || true
  bad_id=$(printf '%s' "${bad_out}" | sed -n 's/.*"id": *"\(op_[^"]*\)".*/\1/p' | head -1)
  if [ -z "$bad_id" ]; then
    fail "解释器写错的部署没有返回 operation id：${bad_out}"
  elif wait_for_status "$bad_id" failed; then
    assert_eq "解释器写错时的错误码" "MANIFEST_INVALID" \
      "$(q operation get "$bad_id" --json | sed -n 's/.*"errorCode": *"\([^"]*\)".*/\1/p')"
    assert_eq "报错里带上了那个解释器路径" "yes" \
      "$(q operation logs "$bad_id" --json | grep -q '/opt/jdk-does-not-exist/bin/java' && printf yes || printf no)"
    # 「在部署之前失败」的判据：连运行用户都还没建出来。否则说明这条检查跑在
    # 一堆副作用之后，而那样的失败会把机器留在一个半套状态里。
    assert_eq "预检失败没有留下副作用（运行用户没被创建）" "yes" \
      "$(id "$bad_app" >/dev/null 2>&1 && printf no || printf yes)"
    assert_eq "预检失败没有建出 unit" "yes" \
      "$([ -f "/etc/systemd/system/${bad_app}.service" ] && printf no || printf yes)"
  else
    fail "解释器指向不存在的路径，部署本该失败却没有（$(q operation get "$bad_id" --json | head -c 200)）"
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
    # 迭代 3：把「重启前跑的是哪个 release」记下来。重启后的断言要拿它比对，
    # 而不是拿一个「现在看起来对」的值——那种断言证明不了任何跨重启的事情。
    printf 'deploy_dir=%s\n' "$(readlink -f "/opt/opsd/apps/$DEPLOY_APP/releases/current" 2>/dev/null)"
    printf 'deploy_current=%s\n' "$(readlink "/opt/opsd/apps/$DEPLOY_APP/releases/current" 2>/dev/null)"
    printf 'deploy_port=%s\n' "$DEPLOY_PORT"
    # 主机自有的业务负载（容器数与 JVM 数）：重启后要和这两个数字对上。
    printf 'containers_before=%s\n' "$(container_count)"
    printf 'business_java_before=%s\n' "$(business_java_count)"
  } > "$FRZ_HOST_DIR/boot-before.txt"
  sed 's/^/      /' "$FRZ_HOST_DIR/boot-before.txt"
  pass "已记录重启前的 boot_id、开机时刻、start 操作的 ID、部署的 release 与业务负载计数"
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
  main_pid=$(unit_prop "$RUNTIME_UNIT" MainPID)
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

  log "重启后：部署出来的那个 release 仍然在跑（迭代 3）"
  # 这一节是 3b 留下的「未验证」里最有分量的一条：容器里能验 unit 的 enabled，
  # 但只有真重启才能证明「部署出来的版本自己回来了，而且回来的还是同一个 release」。
  local deploy_dir deploy_current deploy_port
  deploy_dir=$(sed -n 's/^deploy_dir=//p' "$FRZ_HOST_DIR/boot-before.txt")
  deploy_current=$(sed -n 's/^deploy_current=//p' "$FRZ_HOST_DIR/boot-before.txt")
  deploy_port=$(sed -n 's/^deploy_port=//p' "$FRZ_HOST_DIR/boot-before.txt")
  if [ -z "$deploy_dir" ]; then
    fail "重启前的记录里没有部署信息——prepare 阶段那一节没跑完"
  else
    local releases="/opt/opsd/apps/$DEPLOY_APP/releases"
    assert_eq "$DEPLOY_UNIT 重启后的状态" "active" "$(systemctl is-active "$DEPLOY_UNIT" 2>/dev/null)"
    assert_eq "current 仍指向重启前那个 release" "$deploy_current" \
      "$(readlink "$releases/current" 2>/dev/null)"
    assert_eq "current 解析后仍是同一个目录" "$deploy_dir" "$(readlink -f "$releases/current" 2>/dev/null)"
    assert_eq "那个 release 目录还在" "yes" "$([ -d "$deploy_dir" ] && printf yes || printf no)"
    assert_eq "部署的端口重启后自己在监听" "yes" \
      "$(ss -lntH "sport = :$deploy_port" | grep -q . && printf yes || printf no)"
    local deploy_pid
    deploy_pid=$(unit_prop "$DEPLOY_UNIT" MainPID)
    if [ -n "$deploy_pid" ] && [ "$deploy_pid" != "0" ]; then
      assert_eq "重启后托管进程的工作目录仍在那个 release 上" "yes" \
        "$([ "$(readlink "/proc/$deploy_pid/cwd" 2>/dev/null)" = "/var/lib/$DEPLOY_APP" ] && printf yes || printf no)"
    fi
    # 重启后**不许**再出现一次部署：那条路径走的是「运维敲命令」，不是 systemd 的
    # enabled。这里只断言事实（目录数没变多），不去推断中间发生了什么。
    printf '      重启后 releases 下的目录数: %s\n' \
      "$(find "$releases" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')"
  fi

  log "重启后：主机上的业务没有被牵连"
  # 与重启**前记录的值**比，而不是与一个写死的数字比：那台机器上有几个容器是它自己的
  # 事实，换一台主机（或同一台机器上业务变了）写死的数字就变成一句假话。
  local containers_before containers_now
  containers_before=$(sed -n 's/^containers_before=//p' "$FRZ_HOST_DIR/boot-before.txt")
  containers_now=$(container_count)
  printf '      容器数：重启前 %s，现在 %s\n' "${containers_before:-未记录}" "$containers_now"
  if [ -n "$containers_before" ]; then
    assert_eq "业务容器数与重启前一致" "$containers_before" "$containers_now"
  fi
  # JVM 单独数一遍：这台主机上有**不在容器里、也不在 systemd 下**的业务 JVM（迭代 3c 的
  # 那次误杀就是它们）。这里只数，绝不按名字杀（见 run.sh 头部的纪律）。
  local java_before java_now
  java_before=$(sed -n 's/^business_java_before=//p' "$FRZ_HOST_DIR/boot-before.txt")
  java_now=$(business_java_count)
  printf '      主机自有的 JVM 数：重启前 %s，现在 %s\n' "${java_before:-未记录}" "$java_now"
  if [ -n "$java_before" ]; then
    assert_eq "主机自有的 JVM 数与重启前一致" "$java_before" "$java_now"
  fi
}

# 前置失败就停在这里：本轮**不建也不删**。
#
# 之前的行为是「照建不误，最后靠 cleanup 收干净」，而 cleanup 是无条件的——于是
# 「别人的东西还在」会被处理成「那就删掉它」。停下来的代价只是重跑一次。
stop_if_preconditions_failed() {
  if [ "${PRECONDITION_FAILED}" != "1" ]; then
    return 0
  fi
  printf '\n前置检查未通过：这台机器上本来就有不属于本轮的东西。\n' >&2
  printf '本轮**没有创建任何东西，也没有删除任何东西**。请先查清上面那些路径是谁的，\n' >&2
  printf '再决定怎么处理，然后重跑。\n' >&2
  exit 1
}

report() {
  printf '\n--- 观察到的、不属于断言结果的部署事实\n'
  if [ "${#FINDINGS[@]}" -eq 0 ]; then
    printf '（无）\n'
  else
    printf '%s\n' "${FINDINGS[@]}"
  fi
  printf '\n%d 项通过，%d 项失败' "$PASS_COUNT" "$FAIL_COUNT"
  if [ "$SKIP_COUNT" -gt 0 ]; then
    printf '，%d 节跳过（跳过不是通过）' "$SKIP_COUNT"
  fi
  printf '\n'
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
      stop_if_preconditions_failed
      provision
      check_install_state
      install_opsd_unit
      check_prepare
      check_lifecycle
      # 迭代 3：部署一个 release 并留下它。它的跨重启存活由 check 阶段断言。
      check_deploy_host
      # 迭代 3c：真 JVM + 资源限制 + 解释器预检。
      check_java_and_resources
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
      stop_if_preconditions_failed
      provision
      check_install_state
      install_opsd_unit
      check_prepare
      check_lifecycle
      check_stop
      check_database
      # 迭代 3：部署（放在 stop 之后，因为它要的正是「应用没在跑」的起点）。
      check_deploy_host
      # 迭代 3c：真 JVM + 资源限制 + 解释器预检。
      check_java_and_resources
      report
      ;;
  esac
}

main "$@"
