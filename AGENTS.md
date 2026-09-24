# AGENTS.md

本文件是本仓库的协作约定，供人工开发者和 AI CLI 共同遵守。

## 语言约定（强制）

**所有产出都用中文**，包括：对话回复、`docs/` 下的文档、代码注释（Go、SQL、YAML、
Makefile、CI 配置），以及 Git 提交信息。

以下技术标识保持英文原样，不要翻译：标识符、API 字段名、错误码、命令名、日志字段
的 key、状态值（如 `pending`、`running`）。

## 项目概览

`frz-tools` 是一个 Go 实现的 Linux 运维工具。迭代 0（工程基础与设计冻结）已冻结；
**迭代 1 的四个里程碑 1a / 1b / 1c / 1d 均已实现并提交**——资源模型与制品管理、调度器、
Linux 适配（`RuntimeAdapter` + systemd 双档）、任务引擎的重试与退避。容器断言在本地与
干净 runner 上均为 **106 通过 / 0 失败**。

**迭代 2a（备份：端口、策略模型、文件适配器、传输编码、API+CLI）与 2b
（PostgreSQL/MySQL/MariaDB 适配器、数据库隔离恢复）均已实现并验证**；
2d（GFS 保留与 prune）未开始；迭代 3–5 未开始。
**仍缺 `legacy` 档（systemd 219–239）与真实 Linux 主机的证据**，这两项不得写成已验证。
进度与逐条证据见 `docs/plans/` 下对应迭代文档的第 13 节之后（实现记录与验证记录）。

- `opsd`：目标主机上的守护进程，负责执行需要权限的操作。
- `opsctl`：用户 CLI，负责发起操作、查询状态与日志。
- 两者通过 Unix Socket 上的 HTTP/JSON `api/v1` 协议通信。

设计与验收标准见：

- `docs/plans/2026-09-21-linux-ops-tool-roadmap.md`：总路线图（含历次调整记录）
- `docs/plans/2026-09-21-iteration-0.md`：迭代 0 设计、实现记录与验证记录（已实现并验证）
- `docs/plans/2026-09-21-iteration-1a.md`：资源模型、制品管理与配置版本化（已实现并提交）
- `docs/plans/2026-09-21-iteration-1b.md`：调度器（已实现并提交）
- `docs/plans/2026-09-21-iteration-1c.md`：Linux 适配（已实现并提交）
- `docs/plans/2026-09-21-iteration-1d.md`：任务引擎的重试、退避与并发策略（已实现并提交）
- `docs/plans/2026-09-21-iteration-2.md`：数据库与资源备份（2a、2b 已实现并验证，2d 未开始）

## 代码规范

- 变量命名要有描述性；复杂条件抽取为有含义的布尔变量。
- 遵循仓库既有模式，不引入新的分层或框架。
- 注释只写必要的：解释「为什么」以及非显而易见的约束，不要复述代码本身。
- 依赖保持精简：`modernc.org/sqlite`、`github.com/spf13/cobra`、`gopkg.in/yaml.v3`，
  以及迭代 1b 引入的 `github.com/robfig/cron/v3`（cron 表达式解析）。
  新增依赖前应先确认它无法用标准库合理替代。
- SQL 迁移文件放在仓库根 `migrations/`，由 `migrations` 包的 `go:embed` 导出。

## 架构说明

依赖方向：`cmd` → `internal/adapters` → `internal/application` → `internal/domain`。

- `api/v1`：对外协议类型与错误码，各层共享的叶子包。
- `internal/domain`：领域模型、状态机、错误码载体、脱敏等纯逻辑。
- `internal/application`：定义端口（`ports.go`）并编排用例，不 import 任何具体适配器。
- `internal/adapters`：SQLite、执行器、HTTP API、客户端、配置、日志等实现；
  运行时适配器在 `runtime/systemd`（真实）与 `runtime/proc`（假适配器，供 macOS/CI 跑同一套合约），
  两个适配器**共用** `runtime/unitfile`（unit 渲染与转义）与 `runtime/readiness`（tcp/http 就绪探测
  与连续成功计数）——不允许各自实现一份。

装配层的**平台选择只有一处**：`cmd/opsd` 里 `GOOS == linux` 才注入 systemd 适配器，其它平台
不注入（`runtime.*` 因此返回 `RUNTIME_UNSUPPORTED`）；刻意**不提供** `runtime.adapter` 之类的
配置开关，避免生产误选 `proc` 假适配器。

上面那条「不 import 任何具体适配器」的约束**只管非测试代码**：`internal/application`
自身的 package 依赖里没有任何适配器。**测试是显式例外，现状如此且不打算现在就收**——
`internal/application` 的 `_test.go` 会为了拿到真实的 SQLite 与本地制品存储而 import
`internal/adapters/sqlite` 与 `internal/adapters/blob`：`artifact_test.go` 是
`package application_test`（外部测试包），`scheduler_test.go`、`service_test.go`、
`runtimeops_test.go` 则是同包内测试包（只 import `sqlite`）。目前不成环的原因是 `sqlite`
只依赖 `domain`，而 `blob` 依赖 `application`——后者一旦在**同包内**测试里被 import 就会形成
import 环，所以 `blob` 只能出现在外部测试包中。
据此的约定（不是硬性禁令）：

- `internal/application` 新增的单元测试优先用本地假实现（例如 `proc` 适配器、内存端口），
  依赖具体适配器的测试按集成测试对待，不要假装它是纯单元测试；
- 引入新的适配器依赖前先确认不会造成 import 环，并在提交信息里说明为什么必须用真适配器
  而不是假实现。

关键不变量：

- Operation 状态机只允许 `pending → {running, cancelled}` 与
  `running → {succeeded, failed, cancelled}`。
- 同一 `resource` 任意时刻最多存在一个未完成 Operation，否则提交返回 `LOCK_BUSY`。
- 长任务执行期间不持有数据库连接：worker 在短事务中领取后提交，再启动进程。
- 执行器只接受 argv，永不经过 shell。

## 常用工作流

```bash
make fmt           # 格式检查
make vet           # 静态检查
make test          # 单元 + 集成 + e2e
make test-race     # 竞态检测
make build         # 构建两个二进制到 output/
make cross         # 交叉编译 linux/amd64 与 linux/arm64 到 output/
make verify-linux  # Linux 容器验证：文件模式、属组、Unix Socket ACL、systemd（需要 docker）
make verify-db     # 真实 PostgreSQL / MySQL / MariaDB 实例上的备份适配器共享合约（需要 docker）
make verify-host   # **真实 Linux 主机**上的验证（需要一个能 ssh 的目标主机，不可能进 CI）
make ci            # fmt + vet + test + test-race + cross，提交前必须通过
```

### 提交后不等 CI 结果（2026-09-24）

**非大模块的提交不要等待 CI 结果。** 文档、注释、单点修复、测试修正这类改动，本地
`make ci` 通过后即可提交、推送，并继续下一步——**不要**在推送后停下来轮询流水线。

CI 的结果由**独立的定时任务或另一个会话**兜底处理（检查 `main` 上的失败运行、定位原因，
对明确的小范围问题直接修复，其余只汇报）。因此：

- 「本地 `make ci` 全绿 + 已推送」即视为本轮工作完成，**不要**把「CI 变绿」写成完成条件，
  也不要为了等 CI 空转；
- 只有**大模块**提交才值得等 CI 结果——判据是：新增端口或适配器、动状态机/数据表/错误码、
  跨多个包的重构。这类改动失败的排查成本高，早发现更划算；
- 无论等与不等，引用 CI 结论时都必须给出真实的运行号与结果，不得凭「上次是绿的」推断这一次。

两个二进制的命令帮助、旗标说明与提示信息一律使用中文；命令名、旗标名、`--json`
输出的字段名保持英文（与 `api/v1` 逐字段对应）。本地化实现见 `internal/cliutil`。

`make verify-linux` 依赖 docker，因此不纳入 `make ci`，但在 CI 中作为独立 job 运行
（`run: bash test/linux/verify.sh`，见 `.github/workflows/ci.yml`）。**断言清单与断言数以
`test/linux/verify.sh` 为准，权威数字是脚本运行时打印的「`%d` 项通过，`%d` 项失败」——
当前为 118**（2026-09-24 迭代 2a 落地后实跑：118 通过 / 0 失败，其中 1c 的 `check_runtime`
49 项、1d 的 `check_retry` 17 项、2a 的 `check_backup` 12 项；历史快照：1d 时点 106 项、
1c 时点 89 项、1b 时点 40 项，A7 落地时的静态推导 87 项偏低）。**不要引用静态推导值当结论。**

harness 现在会起**两个 `opsd` 实例**：一个以服务用户 `frz-ops` 运行（迭代 0 的既有断言全打在
它上面，前缀未动），另一个**以 root 运行**（配置 `test/linux/opsd.root.verify.yaml`，独立
socket/数据库/日志/制品目录）——因为 `RuntimeAdapter` 要 `useradd`/`chown`/`systemctl`，必须
root；1c 的 `check_runtime`（49 项）只打 root 实例。探针应用 `test/linux/probe/main.go` 只上报
凭据的长度与 sha256，绝不打印明文。用桩跑出来的结果只是「断言体自洽」的证据，
**不是**容器验证。

**边界没有变**：以上都是「**Linux 容器**」证据（Ubuntu 24.04 / systemd 255），
**不等于「Linux 主机」**——sudoers/PAM 实际策略与 `legacy` 档（systemd 219–239，
容器只有 255）都仍是**未验证**。

`make verify-host`（`test/host/run.sh` + `test/host/verify.sh`）是**第一类「Linux 主机」证据**：
在一台真实主机（2026-09-24：**Rocky Linux 10.2 / 内核 6.12 / systemd 257 / SELinux Enforcing** /
x86_64）上把 opsd 装成 systemd 服务、跑完 RuntimeAdapter 的生命周期与真实的 MySQL 8.4 备份闭环，
实跑 **93 项通过 / 0 项失败**（1c 的 71 项 + 2b 的 22 项），结束时把主机上创建的一切删干净。
**重启验证也做了**（用户单独授权重启那台机）：停机 44 秒、`boot_id` 前后不同（这是「真的
重启过」的硬证据）、重启后 **21 项通过 / 0 项失败**——两个 unit 都 enabled 且自动回到 active、
**`/run/opsd` 由 systemd 重新创建**（`/run` 是 tmpfs，容器里被 `install -d` 掩盖的那一点）、
磁盘上的模式与凭据副本一字未变、**重启前创建的 Operation 仍可查且终态仍是 succeeded**、
凭据仍逐字节到达进程。harness 因此有三个阶段（`FRZ_HOST_PHASE=prepare|check|cleanup`）。
它**不进 CI**（需要一个能 ssh 的目标主机），细节见 `docs/plans/2026-09-21-iteration-1c.md` 第 18 节
与 `iteration-2.md` 第 21 节。三条真机才暴露得出的事实：**SELinux enforcing 下 opsd 与托管进程
都落在 `unconfined_service_t`——本工具不提供 SELinux 加固，这一条是「未实现」而不是「已支持」**；
以 root 运行的 opsd 建出的 socket 是 `root:root 0660`，非 root 用户用不了 opsctl（配置里没有
socket 属组项）；MySQL 的隔离恢复对备份账号的权限要求不只是 CREATEDB（见 `iteration-2.md` 第 21 节）。

`make verify-db`（`test/linux/verify-db.sh`）是**另一类**容器证据：它在真实的
PostgreSQL / MySQL / MariaDB 实例上跑备份适配器的共享合约（`test/dbbackup`，由
`FRZ_TEST_*_DSN` 控制，未设置时整包跳过）。服务端跑在容器里，**客户端工具也从容器里跑**
——通过一组 `docker exec -i` 的包装脚本放进 PATH，因此用的是与服务端同版本的客户端，
且测试进程的 PATH 被替换成「只有包装脚本 + 系统目录」，跑的是哪一家的客户端是确定的。
当前实跑：**13 项通过 / 0 失败**（三家各 9 条合约子用例 + 就绪/账号/收尾 4 项），本地 arm64
与干净 runner 的 amd64 上各跑一遍，CI run `35963799621` 三个 job 全绿。两个只在 CI 上
暴露过的坑，都写在脚本的注释里：①就绪探测必须走 TCP（容器入口脚本的临时实例只监听
socket，用 socket 探测会提前判定就绪）；②测试进程的 PATH 里**不能有 `/usr/bin`**
（CI runner 自带 mysql/psql，会把容器里的包装脚本盖掉）。
**它同样不等于「Linux 主机」**：容器里是**同版本**客户端与**同机**实例，本地 socket 连接、
版本组合差异（如 pg_dump 16 备 12 的库）、生产账号的真实权限边界、长时间大库备份都没覆盖；
**GTID 开启的 MySQL 8** 与 `prune`（属 2d）也是**未验证**。
仓库位于 <https://github.com/freezeChen/frz-tools>，module path 为
`github.com/freezeChen/frz-tools`；CI 于 2026-09-22 起真实执行，`test` 与 `linux-verify`
两个 job 在干净的 ubuntu-latest runner 上均通过。**2026-09-24 起 1c 的容器断言已跑全**：
`1168f29` 之后的运行是干净 runner 上的 **89 项通过 / 0 项失败**（此前 CI 那份只有迭代 0/1b
的 40 项，`check_runtime` 尚未进入）；迭代 1d 补了 `check_retry`（17 项）、迭代 2a 又补了
`check_backup`（12 项），总数到 **118**（本地实跑 118/0；106 那一档已在干净 runner 上复现：
run `35949920348` 的 `linux-verify` 106/0）。
该证据因此同时覆盖 **arm64**（本地 OrbStack）与
**amd64**（CI runner）两种架构。
**`/etc/opsd` 的两条断言不是矛盾而是时序**：`check_filesystem` 里的「模式 = `750`」在
`Prepare` **之前**（安装脚本状态），`check_runtime` 里的「`0751`」在 `Prepare` **之后**
（`kind: file` 凭据要能被运行用户穿越，见 `docs/plans/2026-09-21-iteration-1c.md` 第 7 节）。
修改了权限、属组、socket 或 systemd 相关逻辑后必须单独跑它——1c 的一处 `Prepare` 缺陷
（`canChmod` 不认 root，导致以 root 运行时在正常部署上失败）就是这样被容器实跑抓到的。

本地运行：

```bash
go run ./cmd/opsd --config opsd.example.yaml
go run ./cmd/opsctl --socket /run/opsd/opsd.sock health
```

## 验证约定

- 每个迭代的验收标准必须给出「命令 / 结果 / 证据类型」。
- 证据类型必须显式区分：静态检查、单元测试、集成测试、**Linux 容器**、**Linux 主机**、真实服务。
- 「Linux 容器」与「Linux 主机」是两类不同证据，不得混用。容器只验证内核级语义（文件模式、
  Unix Socket ACL、用户/属组、`runuser` 行为）；**sudoers/PAM 实际策略**仍需 Linux 主机验证，
  到 2026-09-24 为止**仍未验证**。**reboot 后的 unit 持久化已于 2026-09-24 在真实主机上验证**
  （见上），引用它时必须带主机与 systemd 版本（Rocky Linux 10.2 / systemd 257）。
- SELinux 的措辞必须精确：enforcing 下的实际行为**已观测**（进程落在 `unconfined_service_t`，
  本工具不安装策略模块），因此只能说「能装能跑、**不提供** SELinux 加固」，
  不得写成「已支持 SELinux」或「SELinux 已加固」。AppArmor 未在 RHEL 系上存在，仍未验证。
- 源码检查不能替代真实 Linux 主机、systemd、Nginx、数据库实例的验证；未验证内容要显式
  标注为「未验证」。
- 若修改 API、状态机、数据表或错误码，先更新对应迭代文档并记录兼容性影响。

### Linux 容器验证的固定配方

harness 位于 `test/linux/`（`Dockerfile.systemd` + `verify.sh`，另含探针应用
`test/linux/probe/main.go`：只上报凭据的长度与 sha256，绝不打印明文），用 `make verify-linux`
运行。以下是它使用的容器参数，手工排查时同样适用。

systemd 在容器内必须用 `--cgroupns=host`；用 `private` 时 systemd 无法作为 PID 1 启动。

```bash
# 镜像：FROM ubuntu:24.04，加装 systemd systemd-sysv procps iproute2
# （jrei/systemd-ubuntu 只有 amd64 清单，Apple Silicon 不可用）
docker run -d --privileged --cgroupns=host \
  --tmpfs /run --tmpfs /tmp \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  frz-ops-verify:24.04
```

两个已知坑：

- **不得把 macOS 目录 bind mount 进容器做权限测试**：virtiofs 不保真文件属主与模式，结果不可信。
  二进制应通过容器内构建或 `docker cp` 进入，测试状态留在容器文件系统内。
- **`docker cp` 不要写进 `/tmp`**：`--tmpfs /tmp` 会遮住容器根文件系统里的同名目录，
  复制看似成功但 `docker exec` 看不到文件。改用 `/opt` 下的路径中转。

shell 脚本中变量名后紧跟中文全角字符时，必须写成 `${var}`。macOS 自带的 bash 3.2 会把
多字节字符的前几个字节算进变量名，报 `unbound variable`；CI 上的 bash 5 不会暴露这个问题。
