#!/usr/bin/env bash
#
# 在真实 Linux 主机上跑 test/host/verify.sh（**Linux 主机**证据）。
#
# 用法：
#   bash test/host/run.sh                       # 默认 root@192.168.11.101
#   FRZ_HOST=root@10.0.0.5 bash test/host/run.sh
#   FRZ_HOST_KEEP=1 bash test/host/run.sh       # 保留现场用于排查
#
# 它做三件事：交叉编译 linux/amd64 的两个二进制与探针 → 上传到主机 →
# 在主机上以 root 执行断言脚本（脚本经 stdin 送入，因此它结尾把自己所在的目录删掉
# 也不会影响正在执行的自己）。
#
# 主机上会创建并**在结束时删除**：系统用户/组 frz-ops 与 frz-probe、
# /etc/opsd、/var/lib/opsd、/var/log/opsd、/run/opsd、/opt/frz-ops、
# /var/lib/frz-probe、/var/log/frz-probe、以及两个 unit 文件。
# 这是生产机，跑完就走比留现场重要。

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
FRZ_HOST=${FRZ_HOST:-root@192.168.11.101}
FRZ_HOST_DIR=${FRZ_HOST_DIR:-/opt/frz-host-verify}
FRZ_HOST_KEEP=${FRZ_HOST_KEEP:-0}
FRZ_PROBE_PORT=${FRZ_PROBE_PORT:-28581}

log() { printf '\n--- %s\n' "$*"; }

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -p "$WORK_DIR/bin"

# check 阶段不需要重新上传：断言要看的正是**留在主机上**的那份状态
# （包括 prepare 阶段写下的 boot-before.txt），重传会把记录冲掉。
if [ "${FRZ_HOST_PHASE:-full}" = "check" ]; then
  log "check 阶段：跳过编译与上传，直接跑重启后的断言"
  ssh -o BatchMode=yes "$FRZ_HOST" \
    env FRZ_HOST_DIR="$FRZ_HOST_DIR" FRZ_HOST_PHASE=check \
    bash -s < "$REPO_ROOT/test/host/verify.sh"
  exit $?
fi

log "交叉编译 linux/amd64 的 opsd / opsctl / 探针"
for target in "opsd:./cmd/opsd" "opsctl:./cmd/opsctl" "frz-probe:./test/linux/probe"; do
  name=${target%%:*}
  pkg=${target##*:}
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -C "$REPO_ROOT" -o "$WORK_DIR/bin/$name" "$pkg"
done
cp "$REPO_ROOT/test/host/opsd.host.verify.yaml" "$WORK_DIR/"

# kind=file 凭据的源：多行、含反斜杠与引号、**不以换行结尾**——与容器 harness 同一份
# 内容，这样两个 harness 在这条断言上比的是同一件事。
printf '%s' '-----BEGIN FRZ KEY-----
line-two\back"quote$dollar
-----END FRZ KEY-----' > "$WORK_DIR/probe-file"

log "上传到 $FRZ_HOST:$FRZ_HOST_DIR"
ssh -o BatchMode=yes -o ConnectTimeout=10 "$FRZ_HOST" \
  "rm -rf '$FRZ_HOST_DIR' && mkdir -p '$FRZ_HOST_DIR'"
# COPYFILE_DISABLE=1：别把 macOS 的扩展属性（com.apple.provenance 之类）打进包里，
# 否则远端 tar 会刷一屏「忽略未知的扩展头关键字」。
COPYFILE_DISABLE=1 tar -czf - -C "$WORK_DIR" . | ssh -o BatchMode=yes "$FRZ_HOST" "tar -xzf - -C '$FRZ_HOST_DIR'"

log "在主机上执行断言（证据类型：Linux 主机）"
set +e
ssh -o BatchMode=yes "$FRZ_HOST" \
  env FRZ_HOST_DIR="$FRZ_HOST_DIR" FRZ_HOST_KEEP="$FRZ_HOST_KEEP" FRZ_PROBE_PORT="$FRZ_PROBE_PORT" \
  FRZ_HOST_MYSQL_DSN="${FRZ_HOST_MYSQL_DSN:-}" FRZ_HOST_PHASE="${FRZ_HOST_PHASE:-full}" \
  bash -s < "$REPO_ROOT/test/host/verify.sh"
status=$?
set -e

if [ "$status" -ne 0 ]; then
  printf '\n主机验证未通过（退出码 %d）\n' "$status" >&2
fi
exit "$status"
