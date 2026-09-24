# Linux 运维工具总路线图

> 文档日期：2026-09-21  
> 文档状态：Proposed / 供后续实现与交叉验证使用  
> 项目状态：2026-09-21 撰写时为「空目录，尚未初始化 Go 工程和 Git 仓库」；
> 截至 2026-09-24：迭代 0 已冻结，1a / 1b / 1c / 1d 均已实现并提交（容器断言在干净
> runner 上跑全，本地实跑 106 通过 / 0 失败），**迭代 2 的规格已冻结、实现未开始**，
> 迭代 3–5 未开始。详见文末调整记录

## 1. 目标与当前假设

目标是从空目录开始建设一个 Go 实现的 Linux 运维工具，先完成“单机可用、可审计、可恢复”的闭环，再扩展到多主机和更多插件：

- 快速部署 Go、Java 等进程型服务。
- 通过 Nginx + 双 Slot 实现近零停机的蓝绿发布、切流与回滚。
- 支持数据库及文件资源的定时备份、保留、校验和恢复。
- 通用能力通过稳定的领域模型和适配器接口扩展，而不是把逻辑写死在 CLI 中。

默认假设：目标系统为使用 systemd、Nginx 的主流 Linux；第一版优先单机/单服务闭环，后续再做多主机控制面；第一版不引入容器编排、Kubernetes 或任意 shell 工作流编排。

当前已确认的首个实现周期是迭代 0。首个真实端到端适配器在迭代 0 完成后优先选择 Go/Java 单服务部署。

## 2. 总体架构决策

- `opsctl`：用户 CLI，负责配置校验、发起操作、查看状态和日志。
- `opsd`：目标 Linux 主机上的守护进程，负责执行需要权限的部署、备份、systemd、Nginx 操作；第一版通过 Unix Socket 与 CLI 通信，后续增加 mTLS 远程访问。
- 核心层：任务状态机、幂等、锁、重试、审计、配置模型，不依赖具体数据库或运行时。
- 适配器层：`Runtime`、`Backup`、`Storage`、`Proxy`、`Executor`、`Scheduler` 等接口。
- 状态存储：第一版 SQLite（WAL 模式）；所有操作、发布、备份、审计记录均持久化，预留 PostgreSQL 适配能力。
- 扩展策略：第一版使用 Go 内置适配器和版本化接口，不做动态 Go plugin；稳定后再以 gRPC 扩展外部插件。

建议的工程边界：`cmd/opsctl`、`cmd/opsd`、`api/v1`、`internal/domain`、`internal/application`、`internal/adapters`、`migrations`、`test/e2e`。

## 3. 迭代 0：工程基础与设计冻结

### 交付内容

- 初始化 Go module、目录结构、构建/发布脚本、单元测试和 Linux CI。
- 定义版本化配置格式、错误码、Operation ID、结构化 JSON 日志和退出码。
- 实现 `opsctl`/`opsd` 最小通信链路及 SQLite migration。
- 建立统一执行器：命令使用 argv，不默认经过 shell；支持超时、取消、输出截断、退出码和敏感信息脱敏。
- 建立幂等键、资源锁、dry-run、审计事件、状态恢复机制。
- 设计权限模型：专用系统用户、受控目录、文件权限 0600/0750、可配置 sudo 白名单。

### 验收标准

- `opsctl health` 能连接 `opsd`；重复提交同一操作不会重复执行。
- 进程重启后操作状态、锁和审计记录可恢复。
- dry-run 不修改服务、Nginx、备份和文件系统目标。
- CI 覆盖配置校验、超时取消、并发锁和敏感日志脱敏。

## 4. 迭代 1：通用能力（作为后续功能底座）

### 交付内容

- 资源模型：Host、Application、Environment、Artifact、Release、Operation、Schedule、SecretRef、AuditEvent。
- 配置加载：YAML 文件 + schema 校验 + 环境变量/Secret 引用；配置版本向后兼容。
- 制品管理：本地制品目录、SHA-256 校验、临时文件、原子落盘、过期清理；预留 S3/MinIO 存储。
- 任务引擎：阶段状态、超时、指数退避、最大重试、取消、断点/恢复和并发策略。
- 调度器：持久化 cron/间隔计划、时区、错过执行策略、单任务锁、重试和运行历史。
- Linux 适配：systemd service/timer 模板、用户/目录/环境文件管理。
- 日志和可观测性：按 Operation ID 查询日志，基础 metrics，成功/失败通知接口。
- CLI 初始命令：`init`、`validate`、`status`、`logs`、`operation`、`schedule`、`artifact`。

### 扩展接口

- `RuntimeAdapter`：校验、准备、启动、停止、健康检查、收集运行信息。
- `BackupAdapter`：预检查、备份、恢复、校验、清理。
- `StorageBackend`：Put/Get/List/Delete、校验和、对象元数据。
- `ProxyAdapter`：生成配置、配置校验、切流、回滚、优雅 reload。
- `Executor`：本机执行、SSH/远程 agent 执行；第一版只实现本机。

## 5. 迭代 2：数据库与资源备份

### 第一批支持

- PostgreSQL：`pg_dump`/`pg_restore`。
- MySQL/MariaDB：`mysqldump`/恢复工具。
- 文件/目录资源：作为通用 `FileBackupAdapter`，支持排除规则和符号链接策略。

### 备份流程

1. 校验连接、客户端版本、磁盘空间、目标存储和凭据引用。
2. 获取资源锁，生成唯一 Backup Job ID，在临时目录产生备份流。
3. 压缩、可选加密、计算 SHA-256，上传到 StorageBackend，先写临时对象再原子提交。
4. 写入数据库名/版本/大小/时间/校验和/工具版本/保留标签等元数据。
5. 完成后执行保留策略；未完成或校验失败的对象不得进入清理范围。
6. 支持独立 `verify` 和在隔离目录/临时实例中的 `restore` 验证。

### 策略与边界

- 支持保留天数、数量、按日/周/月标签的 GFS 策略。
- 凭据只能引用 SecretRef，不写入命令日志；备份文件默认加密。
- 数据库命令非零退出、网络中断、空间不足、校验不一致时标记失败并可重试。
- 同一数据库默认禁止并发备份；恢复默认需要显式确认和独立权限。

### 验收标准

- 定时任务能产生可查询的运行记录和失败原因。
- 备份文件可通过 checksum 验证，至少完成一次真实 restore 验证。
- 备份进程被杀死、opsd 重启、上传中断后，不产生“成功”记录或污染保留策略。
- MySQL/PostgreSQL 适配器遵循同一接口，新增数据库不需要修改调度器和存储层。

## 6. 迭代 3：Go/Java 通用进程部署

定义版本化 `Application` manifest，至少包含：

- `runtime: go | java`。
- 制品地址、版本、SHA-256、解包方式。
- `exec` argv、工作目录、运行用户、环境变量/SecretRef、端口。
- readiness/liveness 健康检查、启动超时、优雅停止超时。
- 日志目录、资源限制、发布保留数、回滚策略。
- Nginx upstream 名称、域名/监听入口和发布策略。

### 部署流程

1. 下载/校验不可变制品，创建带版本号的 release 目录，不覆盖当前版本。
2. 为服务准备运行用户、目录、环境文件和 systemd unit。
3. Go 使用二进制或 tar 包；Java 使用 JAR/发行包，并允许显式配置 Java 路径和 JVM 参数。
4. 启动新 release，检查进程、端口、readiness、连续健康次数和超时。
5. 记录 release、slot、日志、制品 checksum 和 systemd 状态。
6. 失败时只清理未激活 release，保留当前稳定版本。

为避免 shell 注入和参数歧义，manifest 优先使用 argv 数组；如必须使用脚本，脚本需作为受控制品并显式声明。

## 7. 迭代 4：Nginx 蓝绿发布、快速热更新与回滚

### 运行布局

- 每个应用固定两个 slot：`blue`/`green`，各自使用独立 release 目录、systemd unit 和端口。
- Nginx 只代理稳定 upstream 名称；由工具维护独立 include 文件，不改写用户主配置。
- 状态库记录 active slot、候选 slot、release、切换时间和观察窗口。

### 切流流程

1. 选择非 active slot，部署并启动新版本。
2. 执行本地健康检查、端口检查、依赖检查及可选 smoke test。
3. 生成临时 Nginx include，执行 `nginx -t`；校验失败禁止切流。
4. 原子替换受管配置并执行 graceful reload，将 upstream 指向新 slot。
5. 进入可配置观察窗口，采集健康、错误率和退出状态。
6. 观察成功后 promote，旧 slot 优雅摘流、等待连接排空后停止；保留最近若干可回滚 release。
7. 任意阶段失败时切回旧 upstream，reload Nginx，恢复旧 slot，并将新 slot 标记为 failed。

### 必须覆盖的边界

- 新服务启动但 health check 失败、端口冲突、Nginx 配置非法、reload 失败、旧版本已不存在、磁盘不足、操作被中断。
- 切流和状态落库不一致时，重启后通过 Nginx 当前配置及 systemd 状态做 reconcile。
- 禁止删除最后一个 healthy release；回滚也必须重新执行配置校验和健康检查。
- “热更新”定义为新进程预启动 + Nginx graceful 切流，不承诺 Go/Java 进程内无重启。

### 验收标准

- 正常发布期间旧版本继续服务，健康的新版本才允许接流。
- 新版本故障不会自动切流，切流后故障可在观察窗口内自动/手动回滚。
- Nginx reload 失败、opsd 重启、发布重复提交均不会破坏当前稳定版本。
- 以压测和故障注入验证发布期间无预期 5xx 峰值，并记录可追踪的发布时间线。

## 8. 迭代 5：远程、多主机与生产加固

在单机闭环稳定后增加：

- SSH 执行器或远程 `opsd`，优先 mTLS/短期 token、主机注册和最小权限。
- Host/Environment inventory、批量发布、并发上限、批次/暂停/继续策略。
- 中央状态库和对象存储；主机本地缓存、断点续传和离线恢复。
- RBAC、审批、操作签名、审计导出、通知（Webhook/邮件等）。
- Prometheus metrics、告警阈值、发布前后对比和操作报表。
- 备份加密密钥轮换、跨主机/跨区域复制、定期灾备演练。

多主机发布第一版只做“分批蓝绿”，不做跨主机事务回滚；每台主机必须有独立的成功/失败状态。

## 9. 公共 CLI/API 与数据兼容要求

建议的 CLI 表面：

- `app validate|deploy|status|rollback|promote`
- `release list|inspect|cleanup`
- `backup run|list|verify|restore|prune`
- `host list|check`
- `operation get|cancel|logs`

`opsctl` 与 `opsd` 之间采用 `api/v1` 版本化协议，所有长操作异步返回 Operation ID，并提供状态查询、取消和日志流；manifest 和 BackupPolicy 也带 `apiVersion`/`kind`。新增字段默认向后兼容，删除字段必须经过版本迁移。状态机使用显式终态：`succeeded`、`failed`、`cancelled`、`rolled_back`，避免仅依赖日志判断结果。

## 10. 测试、发布和质量门禁

- 单元测试：状态机、配置 schema、锁、重试、保留策略、路径安全、命令构造和敏感信息脱敏。
- 合约测试：每个 Runtime/Backup/Storage/Proxy 适配器必须通过同一套接口测试。
- 集成测试：临时 SQLite、systemd 模拟/真实 Linux VM、Nginx 配置校验、MySQL/PostgreSQL 实例。
- 端到端测试：Go 发布、Java 发布、健康失败不切流、切流后回滚、备份/恢复、进程和网络中断恢复。
- 安全测试：路径穿越、恶意 manifest、凭据泄露、权限越界、并发操作和重放请求。
- 每个迭代完成后必须有可运行的 demo manifest、故障注入记录和升级/回滚说明。

静态检查、构建和单元测试不能替代真实 Linux 主机、systemd、Nginx、数据库实例或网络中断验证；后续报告必须区分这些证据类型。

## 11. 首期明确不做的内容

- Kubernetes/容器编排和跨云资源编排。
- 任意用户 shell 脚本作为默认扩展机制。
- 数据库主从切换、在线 schema migration、跨数据库事务备份。
- 复杂灰度流量（按用户/地域/百分比）；蓝绿稳定后再增加 canary。
- 在单机蓝绿和备份恢复验收前开发 Web UI。

## 12. 最终完成标准

首个可交付版本应能在一台干净 Linux 主机上：安装 `opsd`，声明一个 Go 或 Java 应用，上传并校验制品，使用 Nginx 完成蓝绿发布和失败回滚；声明一个 PostgreSQL 或 MySQL 备份策略，按计划执行、保存校验元数据，并能在隔离环境恢复验证；所有操作均可通过 Operation ID 查询、审计和重试。

## 13. 后续调整记录

后续 AI CLI 或人工开发如需调整路线图，应在本文件末尾追加日期、变更原因、影响迭代和验证要求，不直接删除历史决策。实现状态、失败原因和验证证据应写入对应迭代文档或单独的验证记录。

### 2026-09-21 里程碑拆分

- **变更原因**：第 4 节的迭代 1 同时包含资源模型、制品管理、任务引擎、调度器、Linux 适配、
  可观测性与 CLI 七类内容，一次性冻结会让设计审查失效，且各部分的依赖关系不同。
- **变更内容**：迭代 1 拆为三个里程碑，**功能范围不增不减**，只是分批冻结与验收：
  - **1a**：资源模型、制品管理、配置版本化
    → [2026-09-21-iteration-1a.md](./2026-09-21-iteration-1a.md)（已实现并提交，验证记录见第 15 节）
  - **1b**：调度器（cron/interval、时区、错过执行策略、运行历史）
    → [2026-09-21-iteration-1b.md](./2026-09-21-iteration-1b.md)（已实现并提交，验证记录见第 16 节）
  - **1c**：Linux 适配（`RuntimeAdapter`、systemd、Host/Environment、manifest）
    → [2026-09-21-iteration-1c.md](./2026-09-21-iteration-1c.md)（规格已冻结；**部分实现（进行中）**，
    见「2026-09-23 迭代 1c 部分实现与迭代 0 冻结确认」）
- **拆分依据**：1a 产出的 `Artifact`/`Release`/`SecretRef` 是 1b 与 1c 的共同词汇；
  1b 的调度语义与 1c 的 systemd 细节彼此独立，可分别设计与验收。
- **影响迭代**：仅迭代 1；迭代 2（备份）、3（部署）、4（蓝绿）的范围与顺序不变，
  但它们的入口从「迭代 1 完成」改为「1a + 1c 完成」，因为发布流程需要 manifest 与运行时适配。
- **新增验证基础设施**：`test/linux/` 提供 Linux 容器验证 harness（`make verify-linux`，
  当前 40 项断言，已接入 CI 独立 job），用于验证 macOS 无法证明的文件模式、属组、
  Unix Socket ACL 与 systemd 行为。它引入了一类新的证据类型「Linux 容器」，
  与「Linux 主机」不得混用（见 `AGENTS.md`）。
- **验证要求**：三个里程碑各自必须给出「命令 / 结果 / 证据类型」的验证记录，
  并区分单元 / 集成 / e2e / Linux 容器 / Linux 主机五类证据；未验证内容显式标注「未验证」。

### 2026-09-22 迭代 1b 实现完成；发现迭代 1 仍有内容未归属

- **变更原因**：1b 已实现并验证（第 16 节记录）。回顾时发现 1a/1b/1c 三个里程碑合起来
  并未覆盖迭代 1 原始交付内容的全部七类，与上面「功能范围不增不减」的说法不符。
- **缺口**（已在 1a/1b/1c 规格之外，需要决定归属）：
  - 任务引擎的**重试/指数退避/断点恢复与并发策略**：迭代 0 只做了超时、取消、状态机与
    `DAEMON_RESTARTED` 恢复，重试与退避至今没有实现。
  - **可观测性**：基础 metrics 与成功/失败通知接口均未实现。
  - **CLI**：`init`、`validate`、`status` 三个命令未实现。
- **待决定**：这三块是补一个 1d，还是并入迭代 2 或迭代 5（生产加固）。
- **影响迭代**：迭代 2/3/4 的入口仍是「1a + 1c 完成」，不受影响。

### 2026-09-22 三项待决事项拍板

- **1. 凭据允许落盘**：1c 允许把 `SecretRef` 解析出的明文写到 `0600` 凭据文件
  （目录 `0700`、属主为运行用户），`opsd` 自身仍在日志/审计/错误信息中按值脱敏。
  被否决的替代方案：opsd 以子进程方式注入环境（会绕开 systemd，失去启停、重启策略与开机自启）、
  接外部秘密管理系统（引入外部依赖，超出范围）。
- **2. 目标 systemd 版本范围**：支持 **systemd ≥ 219**（CentOS 7 / Ubuntu 18.04 起）。
  **连带影响（必须落实，不是可选）**：unit 模板不能写死一套，需要按 `systemctl --version`
  选档（`strict` ≥ 240 / `legacy` 219～239）；`ProtectSystem=strict`、`ReadWritePaths=`、
  `StandardOutput=append:`、`LoadCredential=` 在旧档上都不可用（详见 1c 第 6 节）。
  **验证影响**：现有 harness 是 Ubuntu 24.04（systemd 255），证明不了 219 上的任何事；
  要给出该范围的证据需要第二个镜像（CentOS 7，已 EOL）或一台 Linux 主机，
  在此之前该范围标注**未验证**。
  **与第 1 项的冲突**：`LoadCredential=` 需 ≥ 247，与 219 不相容，收敛方式见 1c 第 15 节。
- **3. 迭代 1 缺口归属**：拆开处理——
  - **重试 / 指数退避 / 断点恢复 / 并发策略** → 新建 **1d**（它是任务引擎的核心，
    迭代 2 的备份与迭代 3 的部署都要用，放到最后做会让前两者将就）；
  - **metrics + 成功失败通知接口**、**CLI `init`/`validate`/`status`** → 并入**迭代 5**
    （生产加固）。因此迭代 5 的范围要相应扩充。
- **新增里程碑**：**1d（任务引擎重试与并发策略）**，规格待编写；迭代顺序变为
  1c → 1d → 迭代 2 → 3 → 4 → 5。

### 2026-09-22 凭据交付方式收敛；老发行版验证改由用户提供主机

- **冲突起因**：「允许用 `LoadCredential=`」与「支持 systemd ≥ 219」不相容——前者需要 ≥ 247。
- **收敛结论**：**所有版本统一走 `EnvironmentFile=`，完全不使用 `LoadCredential=`。**
  一套 unit 模板、一个应用契约（应用只认环境变量）。代价是凭据以环境变量形式出现
  （同用户与 root 可读 `/proc/<pid>/environ`）；换来的是不必让应用契约随宿主机变化。
- **实测支撑（systemd 255 容器内实测，非推断）**：
  - `EnvironmentFile` **无法承载含真实换行的值**——`"a\nb"` 得到字面量反斜杠加 n，
    行尾反斜杠续行会把换行直接吃掉。因此 `kind: env` 的凭据必须单行，
    多行凭据（PEM、JSON 服务账号）走 `kind: file`，环境变量里传**文件路径**。
  - systemd 会处理值里的反斜杠转义（`a\\b` → `a\b`）。opsd 写环境文件时必须
    整体加双引号并把 `\` → `\\`、`"` → `\"`，否则凭据会被**静默改写**。
    该规则已实测逐字节确认（含 `$` 不展开、单引号不特殊、首尾空格保留）。
- **老发行版验证安排**：容器路已确认走不通（CentOS 7 镜像仅 amd64；systemd 219 需要
  cgroup v1，而当前 Docker Desktop 是 cgroup v2）。用户曾提供 `192.168.11.101`，经只读
  探测为 **Rocky Linux 10.2 / systemd 257 的生产机**（跑着 MES、MySQL、Redis、TDengine、
  EMQX、OpenResty，1Panel 管理），既不是老版本也不适合做验证机，已排除；探测过程中未对
  该机做任何改动。**`legacy` 档的验证主机目前仍未落实**，在拿到之前该档一律标注**未验证**，
  不得声称「已支持」。

### 2026-09-22 远程仓库落地，CI 首次真实执行

- **变更内容**：远程仓库确定为 `https://github.com/freezeChen/frz-tools.git`（public）。
  module path 由临时的 `frz-tools` 固定为 `github.com/freezeChen/frz-tools`，
  59 个文件、145 处 import 前缀整体替换。项目名仍为 `frz-tools`，
  按项目名生成的字符串（systemd 单元描述、e2e 临时目录前缀、`frz-ops` 用户与镜像名）
  刻意不动。
- **CI 首次真实执行**：本地已有 20 个提交，此前**从未推送过**，`.github/workflows/ci.yml`
  里的 `linux-verify` job 一次都没跑过。推送后两个 job 均成功：
  - `test`：fmt + vet + test + test-race + cross 全部通过；
  - `linux-verify`：在干净的 ubuntu-latest runner 上 **40 项断言全绿**，
    已核对日志确认断言真的执行（含容器内 systemd 作为 PID 1、tzdata 可用、
    带时区的计划按计划时区解释、`0600` 凭据可解析而 `0644` 被拒绝），不是被跳过。
- **意义**：此前「Linux 容器」类证据只在本地 macOS 的 Docker 上产生；
  现在它在干净 runner 上可复现，该证据类型的可信度提高一档。
  但这仍不是「Linux 主机」证据：GitHub runner 也是 cgroup v2 的虚拟环境，
  真实 reboot 后的 unit 持久化、SELinux/AppArmor、sudoers/PAM 依然挂起。
- **推送方式**：GitHub 已不支持 HTTPS 密码认证，`gh` 配置的是 SSH，
  因此 origin 使用 `git@github.com:freezeChen/frz-tools.git`。

### 2026-09-23 迭代 1c 部分实现与迭代 0 冻结确认

- **变更原因**：1c 的规格文档此前标注「未实现」，但仓库里已有两个 1c 提交；迭代 0 的全部交付物
  与 8 条验收标准也已实现并有测试覆盖，状态行却仍写「待实现」。本次只回填文档，**未改任何代码**。
- **迭代 0 判为已完成、可冻结**：8 条验收标准全部有实现与测试。证据：`gofmt -l .` 空、
  `go vet ./...` 无输出、`go test ./...` 12 个包 ok、`go test -race -count=1 ./...` 全 ok
  （e2e 44.6s）、linux amd64/arm64 交叉编译成功；`go test -v -count=1 ./...` 为 181 个顶层用例
  0 FAIL 0 SKIP（含子测试 313 PASS）；远端 GitHub Actions 共 3 次 run 全部 success，
  HEAD 的 `test` 与 `linux-verify` 两个 job 均 success（2026-09-22T06:28:30Z–06:31:29Z）。
  **遗留项**：`SudoConfig`（`internal/adapters/config/config.go` 第 90 行）只有模型、零行为，
  却出现在 `opsd.example.yaml`，示例注释自述「实际行为由 1c 的 Linux 适配实现」——目前仍未
  实现，已在迭代 0 第 15 节「遗留项」显式标注，不得当作已生效的安全控制。
- **迭代 1c 已有两次提交，此前未记录，本次补齐**（本记录以 HEAD `747aee9` 的已提交状态为
  基准；1c 的其余部分正由其它 worker 并行实现，尚未提交，故未反映在下面的清单里）：
  - `3938db4`：manifest 领域模型（`internal/domain/appspec.go`、`host.go`、`runtime.go`）、
    `internal/adapters/manifest/`（严格解码与迁移链）、systemd 转义函数、`api/v1/errors.go`
    的 1c 错误码；
  - `747aee9`：`RuntimeAdapter` 端口（`internal/application/ports.go`）、共享合约套件
    （`internal/application/runtimecontract/`，11 项断言）、`proc` 假适配器
    （`internal/adapters/runtime/proc/`），转义函数迁至 `runtime/unitfile/`。
  实现范围、与规格的偏差（`readiness.type=exec` 收缩、实际路径偏移）与未实现清单见
  [1c 第 16 节](./2026-09-21-iteration-1c.md#16-实现记录-2026-09-23)，
  验证记录见 1c 第 17 节。
- **新增两项验证债务**：
  1. **`make verify-linux` 本轮未复核**：本机 docker daemon 不可达（OrbStack 未启动），
     40 项 Linux 容器断言没有在本机重跑；最近一次证据是 CI 干净 runner 上的 `linux-verify`
     job（`da70880` 与 HEAD 均 success）。该 job 执行的是 1b 时点的断言，**不含任何 systemd
     适配器断言**，因此不构成 1c 的证据。
  2. **`legacy` 档（systemd 219～239）仍无验证主机**，一律标注**未验证**（原因见 1c 第 15 节）。
  另：真实 Linux 主机的 reboot 后 unit 持久化、SELinux/AppArmor、sudoers/PAM 仍未验证
  （与迭代 0 的结论相同）。
- **1d 规格仍待编写**：`docs/plans/` 下不存在 1d 文档；1d（重试、指数退避、断点恢复、
  并发策略）系 2026-09-22 拍板新建，规格尚未撰写，因此迭代顺序 1c → 1d → 迭代 2 → 3 → 4 → 5
  目前停在 1c。
- **Phase A 剩余项**：**无（Phase A 全部落地，并已在 Linux 容器中跑通 89/0）**。
  下一步是**1d 规格与迭代 2**；仍缺的证据只有 `legacy` 档与真实 Linux 主机项，见补记二。

### 2026-09-23（补记）1c 的 A2–A6b 落地，只剩 A7 与三项验证缺口

- **变更原因**：同日上午的记录把 1c 的其余部分写成「未实现 / 在建」；这些工作已全部落地
  （在工作树里，HEAD 未变、**尚未提交**），本次按实际状态更新，并修正上一节里
  「A4/A5/A6 未做」的说法。
- **已落地**（逐条证据、`文件:行` 与测试名见 1c 第 16 节）：
  - **A2** migration `0004`（`application_specs`/`hosts`/`environments`）+ SQLite 仓储 +
    本机 Host 自举（`EnsureLocalHost`，名字冲突时降级 `local-<id>`）；
  - **A3** unit 渲染与 `strict`/`legacy` 双档（`TierFor`：≥240 strict、219–239 legacy、<219 报错）
    与 systemd 版本探测（注入式 `VersionRunner`，argv-only）；
  - **A4** systemd 适配器本体（`Prepare` 幂等、`Health`/`Status` 分离、
    不用 `systemctl show --value`（230 才有）、`kind:file` 凭据的跨用户可达性处理）；
  - **A6a** spec/hosts/environments/制品下载的 API+CLI+client；
  - **A6b** `runtime/*` 端点、`runtime.*` 走 Operation（无旁路；`start` = 幂等 `Prepare` + `Start`；
    `dryRun` 一律拒绝；适配器不可用则快速失败不建 Operation）、适配器装配（平台选择只有一处，
    不加配置开关）。
- **门禁（实现者实跑）**：`make fmt` / `make vet` / `make test`（16 个包） / `make test-race`
  全绿；linux amd64/arm64 交叉编译通过。**这仍是单元 / 集成 / e2e 证据，不是 Linux 容器证据。**
- **当时只剩 A7**（已落地，见下方补记二）：`test/linux/` 的 1c 容器断言（systemd 适配器、
  凭据路径跨用户可达性）。其中「`verify.sh` 的 `750` 断言与 A4 的 `0751` 冲突」这一处，
  按**保留 `Prepare` 之前的 `750` 断言、在 `Prepare` 之后新增 `0751` 断言**的时序处理，
  两条断言并不矛盾（见 1c 第 7 节）。
- **当时的验证缺口**：systemd 真实执行端到端、`legacy` 档（无验证主机）、真实 Linux 主机的
  reboot 持久化/SELinux/AppArmor/sudoers-PAM。其中 **systemd 真实执行已由容器取得证据**
  （见补记二），**`legacy` 档与真机项仍未验证**，均不得写成已验证。
- **1d 规格仍待编写**（同上一条记录）。

### 2026-09-23（补记二）A7 已落地并在容器中跑通 89/0，并据此修掉一个 root 形态下的 Prepare 缺陷

- **变更原因**：上一版补记写「A7 已写好、容器未运行、只有桩证据」。A7 随后**在容器里跑通了**，
  而且**跑出了一个产品缺陷**——这条比「断言通过」更值得记。
- **容器证据**：`make verify-linux` 在 macOS + OrbStack（容器内 Ubuntu 24.04 / systemd 255 /
  arm64）连跑 5 轮：第 1 轮 25 FAIL（共 65 项）→ 第 3/4 轮各 1 FAIL（87/88 项）→ 第 5 轮
  **exit 0：89 项通过 / 0 项失败**（其中新增的 `check_runtime` 占 49 项）。
  断言数的权威口径只有一个：脚本运行时打印的「`%d` 项通过，`%d` 项失败」= **89**；
  历史快照：1b 时点 40 项、A7 落地时的静态推导 87 项（偏低，勿引用）。
- **harness 结构变化**：新增**第二个以 root 运行**的 `opsd` 实例
  （`test/linux/opsd.root.verify.yaml`，独立 socket/DB/日志/制品目录）——适配器要
  `useradd`/`chown`/`systemctl`，必须 root；既有 40 项断言仍打在原来以 `frz-ops` 运行的实例上，
  前缀未动。另新增探针应用 `test/linux/probe/main.go`（只上报凭据长度与 sha256，不打印明文）。
- **跑出来的产品缺陷与修复**：`Prepare` 的凭据穿越链原先只承认「目录属主 == 自己 euid」，
  而生产上 `opsd` **必须以 root 运行**、`/etc/opsd` 的属主却是服务用户 →
  root 有权限修却拒绝修，`runtime prepare` 在**完全正常的部署**上直接失败。
  修法：新增纯函数 `canChmod(euid, dirUID)`（root 或属主才可收敛）+ 显式的「归我们管理」三级
  集合（凭据目录、`/etc/opsd/apps`、`/etc/opsd`；**`/` 与 `/etc` 只校验、绝不修改**）+
  表驱动 `TestCanChmod`（`internal/adapters/runtime/systemd/files.go`、
  `credentialpath_internal_test.go`）。修复后该断言转 PASS，其余 45 项 runtime 断言同时转 PASS。
  **教训：本地全绿的测试挡不住部署形态差异**（假 runner 下「进程身份 vs 目录属主」永远是测试
  自己造的那种关系）。
- **同时修正一条不稳定断言**：`Restart=on-failure` 下 unit 失败后 `ActiveState` 只是瞬时
  `failed`，自动重启期间是 `activating`（`SubState=auto-restart`）；「缺凭据必须启动失败」
  改为「12×0.5s 窗口内从未 `active`，且状态只能是 `activating`/`failed`」。**「停止」≠「失败」**。
- **污染检查**：容器内只有 `/sys/fs/cgroup` 一个挂载（外加 `--tmpfs /run /tmp`），无仓库
  bind mount；`output/` mtime 未变。
- **结论**：Phase A 的功能与容器断言**全部落地且有容器证据**；第 13 节的 12 条验收标准
  11 条达成、1 条部分达成（`legacy` 档）。**仍未验证**：`legacy` 档（容器是 255，只覆盖 strict）、
  真实 Linux 主机的 reboot 持久化/SELinux/AppArmor/sudoers-PAM。下一步：**1d 规格与迭代 2**。

### 2026-09-24 1c 结项（已提交、CI 全绿）；1d 规格编写完成、待冻结

- **变更原因**：1c 的实现此前一直停在工作树（HEAD 为 `747aee9`）。本次把它连同文档一起提交
  并推送到远端，CI 全绿；随后编写了 1d 的规格。上一节末尾写的「下一步：1d 规格与迭代 2」
  中的前半句已完成。
- **1c 提交**（三个提交，`origin/main` 已同步）：
  - `1168f29` 迭代 1c（三）：systemd 适配器、Host/Spec 持久化、runtime API/CLI 与容器断言
    （74 个文件，含 `AGENTS.md` 与四份迭代文档的同步，以及新增的 `CLAUDE.md`）；
  - `3c7376e` 修复 1c 的 e2e 用例在 Linux 上必然失败：`TestRuntimeWithoutAdapterThroughCLI`
    把「本机没有运行时适配器」写死为 macOS 的行为，而 Linux 上装配层会注入 systemd 适配器
    （`GOOS == linux`，见 `cmd/opsd/main.go`），该用例在 Linux CI 上必然失败。改为按 `GOOS`
    分断言——两个平台各钉各自成立的命题。**教训：e2e 此前只在 macOS 上跑过，平台耦合缺陷
    直到 CI 才暴露**；这与 A7 那次「本地全绿的测试挡不住部署形态差异」是同一类问题的两个面；
  - `e752e1e` 记录 1c 的容器断言首次在干净 runner 上跑全：89 通过 / 0 失败。
- **新增证据（不只是复现）**：1c 的容器断言此前只在本地 OrbStack（Apple Silicon，**arm64**）
  上跑过；`1168f29` 之后的 CI 运行是第一次在干净 runner 的 **amd64** 上执行，`linux-verify`
  跑全 **89 项通过 / 0 项失败**。该证据因此覆盖两种架构。
- **一处文档不一致闭合**：`opsd.example.yaml` 的 sudo 注释声称「实际行为由 1c 的 Linux 适配
  实现」，但 `SudoConfig.AllowedCommands` 从未被任何非测试代码读取（**零行为**）。这正是迭代 0
  文档记录的「示例注释与现状不符」，本次一并修正，并在 1c 文档的「不实现」清单里补上该条目。
- **1c 的最终判定不变**：第 13 节 12 条验收标准 **11 条达成、1 条部分达成**（第 8 条的
  `legacy` 档）。**仍未验证**：`legacy` 档（systemd 219–239；容器是 255，CentOS 7 镜像只有
  amd64 清单且 systemd 219 需要 cgroup v1 而 Docker Desktop 是 cgroup v2，验证主机未落实）、
  真实 Linux 主机的 reboot 后 unit 持久化 / SELinux / AppArmor / sudoers / PAM。
- **1d 规格已编写**：`docs/plans/2026-09-21-iteration-1d.md`（任务引擎的重试、退避与并发策略）。
  范围锁定为「失败之后的第二次机会」，明确**不做**断点续传、优先级队列与抢占。三条关键决策：
  重试载体是**新建 Operation**（状态机冻结，不许回到 `pending`）、自动重试**默认关闭**
  （执行器跑任意 argv，默认重试等于默认重复执行副作用）、退避**不得占着 worker**
  （未到 `notBefore` 不领取，绝不「领到手里再 sleep」）。
- **1d 待冻结**：规格第 12 节列了 5 条未决事项（退避默认值与上限、白名单可否由请求缩小、
  `runtime.*` 是否允许重试、策略是否挂到 `Schedule`/`Application`、重试链长度上限），
  每条都给了推荐值。**冻结之前 1d 不进入实现。**
- **影响迭代**：迭代 2 的入口条件是「1a + 1c 完成」，1c 已达成，因此迭代 2 **可以先于 1d
  开始**（与 2026-09-22 的归属决定一致）。但重试机制是备份场景的刚需，建议 1d 与迭代 2 的
  规格一起推进，避免备份先用一套将就的实现。
- **验证要求**：本次改动为文档、测试与一处配置注释，未触碰产品逻辑；证据为 `make ci` 全绿
  （fmt / vet / test / test-race / 交叉编译）与 CI 两个 job 全绿（`linux-verify` 89/0）。

### 2026-09-24（补记）1d 规格冻结；新增「提交后不等 CI 结果」的协作规范

- **变更原因**：1d 规格（`docs/plans/2026-09-21-iteration-1d.md`）第 12 节的 5 条未决事项
  全部按推荐值拍板，规格就此冻结、可以进入实现；同时新增一条关于 CI 的协作规范并落地为
  实际机制。
- **1d 的 5 条冻结决定**：
  1. 退避默认值：`base = 5s`、`maxDelay = 5m`、抖动 `±20%`、`maxAttempts` 上限 `10`；
  2. 可重试错误码白名单只允许由请求**缩小**，不允许扩大（`EXEC_CANCELLED` 之类不得加入）；
  3. `runtime.*` **允许**声明重试，但语义按 D6 收口——只有「就绪从未通过」才可重试，
     已就绪过再崩溃归 unit 的 `Restart=` 与健康检查；
  4. 重试策略**不**挂到 `Schedule`/`Application`，1d 只在 `CreateOperationRequest` 上支持；
  5. 不额外设重试链长度上限，由 `maxAttempts` 约束。
- **1d 不得偏离的三条硬约束**（规格第 3 节）：重试载体是新建 Operation（状态机冻结，不许回到
  `pending`）、自动重试默认关闭（执行器跑任意 argv，默认重试等于默认重复执行副作用）、
  退避不得占着 worker（未到 `notBefore` 不领取）。
- **新增协作规范**：**非大模块的提交不要等待 CI 结果**。文档、注释、单点修复、测试修正这类
  改动，本地 `make ci` 通过后即可提交推送并继续下一步；CI 的结果由**独立的定时任务或另一个
  会话**兜底。只有大模块提交（新增端口/适配器、动状态机或数据表、改错误码、跨多包重构）才
  值得等 CI。规范已写入 `AGENTS.md`，并已建立定时任务 `check-main-ci`（每小时的 :13 与 :43
  检查 `main`，对明确的小范围失败直接修复，其余只汇报）作为兜底机制。
- **影响迭代**：1d **可以进入实现**。迭代 2 的入口条件（「1a + 1c 完成」）已达成，因此迭代 2
  可以先于 1d 开始；但重试是备份场景的刚需，建议 1d 实现与迭代 2 规格一起推进。
- **验证要求**：本轮为文档改动，未触碰产品逻辑；证据为 `make ci` 全绿。按新规范，提交后
  **不等 CI 结果**，由 `check-main-ci` 兜底。

### 2026-09-24（补记二）迭代 2 规格编写完成，待冻结

- **变更原因**：按「先做迭代 2 的规格」的决定，编写
  `docs/plans/2026-09-21-iteration-2.md`（数据库与资源备份）。
- **建议拆分**：迭代 2 的内容量级接近迭代 1（后者被拆成 1a–1d），因此建议同样拆分。
  判据是「哪一层可以独立验证」：
  - **2a** 端口、`BackupPolicy` 模型、共享合约测试、文件/目录备份适配器、元数据持久化、
    `backup run|list|verify` 的 API+CLI（不需要数据库实例，容器里即可端到端验证）；
  - **2b** PostgreSQL 与 MySQL/MariaDB 适配器、隔离恢复（临时实例/临时目录）；
  - **2c** 传输编码：gzip 压缩、AES-256-GCM 加密、摘要与原子提交；
  - **2d** GFS 保留策略、`prune` 与完整验证链。
    （**2026-09-24 调整**：GFS 部分已移出 2d、停放到
    [2026-09-24-future-iterations.md](./2026-09-24-future-iterations.md) 第 2 节；
    2d 剩下的范围是只用 `keepLast` / `keepDays` 的 `prune` 与验证链。见补记八。）
  跨切面决策（端口形状、策略模型、编码与摘要的归属、元数据与保留模型、存储根隔离）
  由本规格一次定死，实现逐片交付。
- **识别出两处必须先解决的结构性问题**（不依赖待拍板事项）：
  1. **存储根必须隔离**。1a 的制品 GC 把「digest 不在 `artifacts` 表里」一律当作孤儿删除
     （`internal/application/artifact.go` 的 `Collect` 孤儿回收段）。备份若与制品共用一个
     存储根，`artifact gc` 会删掉**全部**备份内容，是静默的数据丢失。因此新增
     `backupStore.root`，备份只写自己的根，两条 GC 各自只扫自己的根。
  2. **调度器写死了操作类型**。`internal/application/scheduler.go` 创建 Operation 时硬编码
     `v1.KindExecutorCommand`，因此今天的 `Schedule` 只能触发 `executor.command`。迭代 2 要
     按计划跑备份，必须给 `Schedule` 加**可选**字段 `operationKind`（省略时默认
     `executor.command`，向后兼容、不提升 apiVersion），调度器改为按字段分派。
- **待冻结**：规格第 11 节列了 6 条（里程碑拆分是否采纳、备份与制品是否共享配额、
  加密默认开关、GFS 标签判定规则、`prune` 是否异步、恢复的权限模型）。每条都给了推荐值。
  其中**恢复的「独立权限」在当前架构下无法实现**——opsd 只有 Unix socket 文件模式一层
  访问控制，能连上就能恢复；建议把「独立权限」明确记为未实现、留给迭代 5，而不是假装有。
- **顺序建议**：路线图第 5 节把「失败可重试」列为迭代 2 的要求，而 1d 的重试已冻结但未实现。
  因此**建议先实现 1d，再做 2a**——这样迭代 2 的重试需求是复用而不是绕开。若坚持先做 2a，
  则**不得**把「可重试」写进 2a 的验收标准。
- **验证要求**：本轮为文档改动，未触碰产品逻辑；证据为 `make ci` 全绿。提交后不等 CI 结果。

### 2026-09-24（补记三）迭代 2 规格冻结

- **变更原因**：上一节记录的迭代 2 规格，第 11 节的 6 条未决事项已全部按推荐值拍板，
  规格就此冻结、可以进入实现。
- **6 条冻结决定**：采纳 2a–2d 的拆分（先做 2a）；备份与制品的配额**分列**、各自独立计数；
  加密**默认开启**且 `keySecret` 必填；GFS 标签按**时区感知**的日 / ISO 周 / 自然月判定；
  `prune` **同步**（与制品 GC 一致）；恢复的「独立权限」**记为未实现**（opsd 目前只有
  Unix socket 文件模式一层访问控制，能连上就能恢复——记为未实现比假装有一道防线更安全），
  只保留 `inPlace` 恢复的显式确认作为门槛。
- **实现顺序已定**：**先实现 1d，再做 2a**。迭代 2 要求「失败可重试」，复用已冻结的 1d
  比绕开它另做一套更省事。
- **验证要求**：本轮为文档改动；证据为 `make ci` 全绿。提交后不等 CI 结果。

### 2026-09-24（补记四）迭代 1d 实现完成

- **变更原因**：按已冻结的 1d 规格实现「重试 / 指数退避 / 断点恢复 / 并发策略」。
  实现记录与验证记录见 `docs/plans/2026-09-21-iteration-1d.md` 第 13、14 节。
- **交付**：`RetryPolicy` 值对象与默认白名单、migration `0005`（`attempt` / `not_before` /
  `retry_policy_json` 三列）、退避在**领取之前**过滤、重试与终态**同事务**落库、
  重启恢复只对声明了策略的操作重放、`CreateOperationRequest` / `RuntimeActionRequest`
  与 `Operation` 的 API 增量、`opsctl` 的 `--retry-*` 参数，以及新错误码
  `RETRY_POLICY_INVALID`（400 / 退出码 22）。
- **三条硬约束全部落地**：重试载体是**新建 Operation**（状态机不许回到 `pending`）、
  自动重试**默认关闭**、退避**不占 worker**。
- **两处由实现补齐的规格缺口**：`RuntimeActionRequest` 原先没有 retry 字段——
  决策 3（`runtime.*` 允许声明重试）当时**根本无法表达**；恢复期的重试原子性规格未指定。
- **一处由容器实跑纠正的规格缺陷**：D6 的第二条前提「`runtime.start` 只有在就绪从未通过时才
  失败」**不成立**——适配器的 `Start` 不等就绪，只做一次 `systemctl start` 就返回。
  结论（两层不叠加）不变、理由更正；容器断言按**真实语义**重写为三个夹具，
  覆盖面比原计划更完整（含一条「runtime.* 的重试确实被武装」的正面对照，
  否则「没有重试」的断言是空洞的）。规格 D6 处加了更正说明，不删原文。
- **验收**：第 10 节的 12 条验收标准**全部达成**（逐条判定见 1d 文档第 15 节）。
  容器断言由 89 项增至 **106 项**（新增 `check_retry` 17 项），本地实跑 **106 / 0**。
- **仍未验证**：真实 Linux 主机上的长退避（分钟级）与不同发行版 systemd 崩溃策略叠加后的
  表现；远程 / 多主机下的重试（迭代 5）。
- **下一步**：迭代 2 的规格已冻结，开始实现 **2a**（端口、策略模型、共享合约测试、
  文件备份适配器、元数据持久化、`backup run|list|verify`）。迭代 2 要求的「失败可重试」
  现在直接复用本迭代的机制。

### 2026-09-24（补记六）迭代 2a 实现完成；2c 并入 2a

- **变更原因**：按已冻结的迭代 2 规格实现 2a。实现过程中发现规格的**里程碑拆分与验收标准
  自相矛盾**，向用户提出两个选项并采纳了 A（见下）。实现记录与验证记录见
  `docs/plans/2026-09-21-iteration-2.md` 第 13、14 节。
- **2c 并入 2a（决定 A，用户拍板）**：规格原把传输编码（压缩 / AES-256-GCM 加密 / 摘要 /
  原子提交）放在 2c，但与 2a 的验收标准冲突——标准 #3 要求「内容寻址」，而摘要不可能不在 2a
  （`StorageBackend.Put` 本来就必须收 digest，`backups.storage_digest` 也是 schema 的一部分）；
  标准 #10 关于加密失败的要求在加密未实现时无意义；且**加密默认开启**，不做加密的 2a
  交付的东西在默认配置下根本跑不起来。因此 **2c 里程碑取消**。剩余里程碑为
  **2b（PostgreSQL/MySQL 适配器）与 2d（GFS 保留与 prune）**。
- **交付**（五个提交）：`BackupPolicy` 模型与解析、`BackupAdapter` 端口与共享合约测试、
  文件备份适配器、migration `0006`（`backup_policies` / `backups`）、传输编码
  （`internal/backupcodec`：先压缩后加密、AES-256-GCM 分块、分块序号进 AAD、零长度收尾块）、
  备份编排（`io.Pipe` 流水线、逻辑/落盘字节数分开记）、`backup.run|verify|restore` 走 Operation、
  API+CLI、以及 `Schedule.operationKind`（D6）。
- **独立存储根（D5）**：`backupStore.root` 与 `artifactStore.root` 分开、配额各自计数。
  这条是**防数据丢失**的结构性要求——1a 的制品 GC 把「digest 不在 artifacts 表里」一律当
  孤儿删除，共用根会让 `artifact gc` 删掉**全部**备份。e2e 与容器断言都直接钉了它。
- **对 1d 一条冻结决定的修订**：备份失败以 `BACKUP_*` 呈现，而 1d 的重试白名单里没有它们，
  因此备份实际上**永远不会自动重试**——而路线图要求备份「标记失败并可重试」。增补
  `BACKUP_PREFLIGHT_FAILED` 与 `BACKUP_VERIFY_FAILED`，**不加**配置/用户错误那两个。
  已在 1d 文档第 3 节 D3 的表格里就地注明，不删原文。
- **两处由测试抓出的真实缺陷**：`FinishBackup` 把备份 ID 填进了有外键约束的
  `audit_events.operation_id`；`prepareDirectories` 把未配置的 `backupStore.root`（空串）
  交给 `MkdirAll`，使「功能没启用」变成「守护进程起不来」。
- **一处规格表述需精确化**：加密流每块用独立随机 nonce（GCM 的正确用法，改不得），
  因此**同一份内容加密两次得到不同 digest**——「同内容同 digest」只在未加密时字面成立。
  加密开启时不做去重是所有带客户端加密的备份系统的共同取舍，已写进 2a 的判定表与实现记录。
- **验收**：第 10 节 12 条标准逐条判定见 2a 文档；其中第 9 条只实现了数据侧判据
  （`Usable()`），`prune` 命令属 2d、**未实现**。容器断言由 106 项增至 **118 项**
  （新增 `check_backup` 12 项），本地实跑 118/0。
- **仍未验证**：2b/2d 的全部内容（含真实数据库实例）、真实 Linux 主机上的长时间大容量备份、
  备份的长期可恢复性（需要真实的时间跨度）。
- **下一步**：**2b**（PostgreSQL 与 MySQL/MariaDB 适配器）。共享合约测试已就位，
  新适配器必须过同一套。


### 2026-09-24（补记五）时间列的「字符串序 ≠ 时间序」缺陷已修复

- **变更原因**：修一个**迭代 1d 期间发现、当时刻意没顺手改**的缺陷（当时的记录见
  `2026-09-21-iteration-1d.md` 第 13 节「发现但未修」）。所有时间列都用
  `time.RFC3339Nano` 写入，而它会裁掉末尾的零、并在小数部分为零时把小数点整段省略——
  于是同一秒内 `"…T00:00:00Z"` 按字典序**大于** `"…T00:00:00.5Z"`，但时间上更早。
  SQLite 比的是字符串，凡按时间列排序或比较处都会在同一秒内乱序：`ClaimNextPending`
  （worker 的领取顺序）、`RecoverRunning`、`not_before <= ?` 的退避过滤、各类
  `ORDER BY created_at` 的列表，以及 `schedule_runs` 的 `(schedule_id, scheduled_for)`
  字符串去重。当时把它排除在 1d 之外的理由是：修它要改写既有库里的数据，属于独立的数据
  迁移，塞进「重试」那个提交会让一次提交同时承担两种风险。
- **改法**：把 `internal/adapters/sqlite/store.go` 的 `timeLayout` 换成**定宽**的
  `2006-01-02T15:04:05.000000000Z`（写入侧统一）；读回改用 `time.RFC3339Nano`，它能接受
  任意小数位数（含没有小数部分），因此新旧值都能读——**宽读窄写**，两个方向都不需要分支。
  1d 里 `not_before` 专用的 `notBeforeLayout` / `formatNotBefore` 是同一处理的局部特例，
  本次推广后删除，所有时间列走同一个 `formatTime`。
  **`migrations/0005_operation_retry.sql` 里那段注释已被本记录取代**：它解释的是「为什么
  `not_before` 刻意用与仓库其它时间列**不同**的定宽格式」，而定宽现在是通则、不再是例外。
  0005 是历史迁移，按约定不改原文，因此这里显式标注它被取代——读到那段注释的人应该在
  这里找到后续，而不是以为仓库里同时存在两种时间格式。
- **既有数据的迁移**：新增 `migrations/0007_normalize_timestamp_width.sql`，用一段可重复
  执行的字符串规范化（秒级前缀 + 右补零到 9 位的小数部分 + `Z`）把 13 张表的所有时间列
  改写成定宽。它只挑「本工具写出的 UTC 形态」（第 20 位是 `Z` 或 `.`），带时区偏移或长度
  异常的值、以及 NULL 一律不动。**编号从 0005 直接跳到 0007**：`0006` 留给迭代 2a 的备份
  表，而 `migrate.go` 的 `migrationVersion` 只取文件名的数字前缀，两个 `0006_*.sql` 会撞
  `schema_migrations` 的主键；`Migrate` 按文件名排序执行，编号不连续是允许的。
- **影响面**：只影响**时间表示与排序**。状态机、资源锁语义与错误码一个字都没动；乱序本身
  从未影响过正确性，只是同一秒内的顺序。对外 API 不受影响：时间在 API 层是 `time.Time`，
  线上的序列化由 DTO 决定，不读库里的字符串。**一处必须知道的后果**：库里存的字符串变了，
  任何直接读库比对时间戳字符串的外部脚本都要跟着改（仓库内的 `test/linux/verify.sh` 只按
  `created_at` 排序取行、不比对字符串，无需改）。
- **验证要求**：证据是**单元 / 集成测试**（真实 SQLite 引擎，本地 macOS/arm64；CI 为
  amd64）。三条：①`TestFormatTimeKeepsChronologicalOrder` 与
  `TestClaimNextPendingOrdersSameSecondOperationsByTime` 在修复前失败、修复后通过（复现用例
  先行）；②`TestMigrateNormalizesLegacyTimestampWidth` 用「只应用到 0005 的库 + 旧格式写入」
  复现升级路径，断言 13 张表的每一列都与写入侧逐字符一致、NULL 仍为 NULL、带偏移的值不被
  改写、重放迁移体是恒等变换，并要求升级后的库按时间序领取；③既有用例全绿（`make ci`）。
  本地 `make ci` 全绿；分支上的 CI run `35950893265` 两个 job 全绿（`test` 与 `linux-verify`，
  后者在干净 runner 上打印 106 项通过 / 0 项失败——本次没有新增容器断言，数字不变）。
  **未验证**：没有在 Linux 容器或真实主机上跑过「带数据的旧库升级」——容器 harness 用的是
  全新库，迁移在空库上是恒等变换；也未在真实生产库规模上评估过迁移耗时。

### 2026-09-24（补记五）迭代 2b 实现并验证：数据库备份适配器

- **变更原因**：按第 5 节「数据库与资源备份」的范围实现 PostgreSQL 与 MySQL/MariaDB
  适配器，以及数据库的隔离恢复。设计与实现记录见
  `docs/plans/2026-09-21-iteration-2.md` 第 17–20 节。
- **交付**：`internal/adapters/backup/{postgres,mysql,dbtools}`、`test/dbbackup`、
  `test/linux/verify-db.sh`（新增 `make verify-db` 与 CI job `db-verify`）。
- **对既有结构的三处改动**（都在迭代 2 文档里记了理由与兼容性影响）：
  1. **`Executor` 端口新增 `RunStream`**。这不是新功能而是修一个正确性缺陷：
     `Run` 把 stdout 收进**有上限**的缓冲，超限部分被丢弃而命令照常退出 0——拿它跑
     `pg_dump` 会得到一份「记录为成功、内容却残缺」的备份。`api/v1` 不变，不提升
     apiVersion，不涉及数据表。
  2. **新增错误码 `BACKUP_RESTORE_FAILED`**（HTTP 409 / 退出码 28）。规格的错误码表里
     没有它，而恢复失败原本只能借 `EXEC_EXIT_NONZERO` 表达——那会把「这次恢复没成功」
     和「一条命令跑失败了」混成一个码。刻意**不**加入 1d 的重试白名单：恢复会覆盖真实
     数据，自动重试该由人看着做。
  3. **`resource.database` 增加字符集约束**（字母/数字/下划线/`$`，不以数字开头）。
     这是**安全边界**：库名会进命令行（mysql 的位置参数），一个以 `-` 开头的库名会被
     MySQL 客户端当成旗标解析。执行器只接受 argv、挡得住 shell 注入，挡不住旗标注入。
- **补上 2a 的两处遗留**：①`backup verify` 现在核对存储摘要与 `backups.storage_digest`
  （路线图的验收标准写的是「备份文件可通过 checksum 验证」，而 2a 只问了适配器
  「你读得动吗」）；②端口的 `Cleanup` 终于在产品路径上被调用——在此之前它只有合约测试
  在调，「被中断的恢复留下的临时资源」在生产上没有任何出口。
- **三处只有真实数据库实例才能发现的坑**（单测用假实现永远发现不了）：
  `mysqldump` 不认 `--connect-timeout`（它是 `mysql` 客户端独有的选项，给了直接退出 7）；
  MariaDB 的客户端包里可能**没有** `mysqldump` 这个名字（官方 `mariadb:11` 镜像里只有
  `mariadb-dump`），因此按优先级解析两个名字；MariaDB 的备份流开头是 `-- MariaDB dump`
  而不是 `-- MySQL dump`，只认后者会让每一份 MariaDB 备份都校验不通过。
- **验证要求**：证据是**单元测试**（两个适配器各 19 / 16 个用例，覆盖 argv 形状、密码不进
  argv、库名一致性、版本倒挂、临时库收尾、校验判据）、**集成测试**（篡改存储字节后校验必须
  失败、未注册 kind 在提交期与执行期都被拒、产品路径真的会调 `Cleanup`）、**Linux 容器**
  （`make verify-linux` 118/0 回归不变）与**Linux 容器承载的真实数据库实例**
  （`make verify-db` 13/0：PostgreSQL 16、MySQL 8.0、MariaDB 11 各跑一遍共享合约，
  测试账号是非超级用户 + 建库权限，并检查三个实例上都没有残留的临时库）。
  `make ci` 全绿；干净 runner 上的 CI run **`35963799621`** 三个 job 全绿
  （`test`、`linux-verify` 118/0、`db-verify` 13/0）。先后两次 `db-verify` 失败都是
  harness 的问题、不是产品缺陷：①就绪探测走了 socket，而容器入口脚本的临时实例只监听
  socket，于是建账号打在正在重启的实例上；②PATH 里的 `/usr/bin` 让测试解析到 runner
  自带的 mysql 客户端，且失败输出被自己的 `grep` 过滤掉了正文。两处都已在脚本里就地注释。
- **未验证**：真实 Linux 主机上的数据库备份（本地 socket、版本组合差异、生产账号的真实权限
  边界、长时间大库与磁盘将满）、GTID 开启的 MySQL 8（适配器刻意不传 `--set-gtid-purged=OFF`，
  因为 MariaDB 的 mysqldump 不认它）、`prune`（属 2d，未实现）。

### 2026-09-24（补记六）第一类「Linux 主机」证据：真机上的 runtime 与数据库备份

- **变更原因**：用户确认 `root@192.168.11.101` 可作为测试机，并在被明确问到时授权
  「允许在主机上安装」（建专用系统用户、写 unit、装 opsd，不动既有业务文件）与
  「新建独立库与专用账号」。这台机在第 15 节（1c）曾被**只读探测后排除**——它是生产机、
  且 systemd 257 证明不了 `legacy` 档。本轮用途不同：不是去验证 `legacy` 档，而是拿
  **真实主机**这一档的证据。**排除的那两条理由依然成立**：`legacy` 档仍未落实、
  生产业务未被触碰。
- **交付**：`test/host/{run.sh,verify.sh,opsd.host.verify.yaml}` 与 `make verify-host`。
  工作站侧交叉编译并上传，断言脚本经 stdin 送进主机以 root 执行，**结束时删干净自己创建的
  一切**（用户/组、unit、目录、临时库）并逐项报告。
- **证据**：环境 **Rocky Linux 10.2 / 内核 6.12 / systemd 257 / SELinux Enforcing / x86_64**；
  `make verify-host` 实跑 **93 项通过 / 0 项失败**（1c 的 runtime 与 unit 生命周期 71 项 +
  2b 的真实 MySQL 8.4 备份闭环 22 项）。数据库那一轮用**加密开启**的策略跑完了
  「备份 → 校验（含存储摘要核对）→ 隔离恢复 → 删表 → 原地恢复 → 内容指纹逐字一致」。
  详细断言表见 `2026-09-21-iteration-1c.md` 第 18 节与 `2026-09-21-iteration-2.md` 第 21 节。
- **三条真机才暴露得出的事实**：
  1. **本工具不提供 SELinux 加固**：opsd 与托管进程都落在 `unconfined_service_t`，
     等于 SELinux 对托管应用的约束**未生效**。这是**未实现的能力**，不得写成「已支持」。
     第 14 节那条「SELinux 未验证」据此改写为「enforcing 下的行为已观测」。
  2. 以 root 运行的 opsd 建出的 socket 是 `root:root 0660`，非 root 用户用不了 `opsctl`，
     而配置里没有 socket 属组项——真机暴露的部署缺口，记为待定。
  3. MySQL 的隔离恢复对备份账号的权限要求**不只是 CREATEDB**：MySQL 里建库的人不会自动
     获得该库的权限，因此还需要临时库上的权限；只给全局 `CREATE` 会让每次隔离恢复都在
     对端留下一个临时库。最小权限配方已写进 `iteration-2.md` 第 21 节（`CREATE ON *.*` +
     `ALL ON \`frz\_restore\_%\`.*`），不需要 `*.*` 全权。
- **一处 harness 自身的缺陷（更该记住的一条）**：`test/dbbackup` 里给 MySQL/MariaDB 写的
  「内容指纹」用了 `||`——而 MySQL 里 `||` 是**逻辑或**，那个表达式永远返回 1，于是合约里
  最核心的往返断言退化成「表里有至少一行」并一路绿着通过。真实主机上才发现。已改用
  `CONCAT`，并新增元断言 `TestFingerprintIsContentSensitive`（改一行的值指纹必须变），
  让「断言本身没有分辨力」这种事再也骗不过去。修好后 `make verify-db` 三家实例仍 **12/0**：
  **产品没问题，是测试在空转**。
- **仍未验证**：`legacy` 档（systemd 219–239）、**reboot 后的 unit 持久化**（本轮没有重启
  那台生产机）、sudoers/PAM 实际策略、真实生产库与大库的备份、GTID 开启的 MySQL 8、
  `prune`（2d 未实现）。

### 2026-09-24（补记七）重启验证：unit 持久化与 self-healing

- **变更原因**：补记六里记着「reboot 后的 unit 持久化」仍未验证（需要重启那台生产机）。
  用户随后单独授权重启，本轮补上。
- **做法**：`test/host/verify.sh` 增加三个阶段 `prepare` / `check` / `cleanup`
  （`FRZ_HOST_PHASE`）。分段是必需的——跨重启存活的那个状态正是要留下的东西，
  一次跑完的 `full` 模式会把它删掉。
- **重启事实**：发起 14:57:56，SSH 恢复 14:58:40（停机约 **44 秒**）；`boot_id`
  `3ac249b2…` → `4146571f…`——**两个值不同，才是「真的重启过」的硬证据**。
  9 个业务容器（全部 `RestartPolicy=always`）在恢复连接时已经全部回来。
- **证据**：重启后 **21 项通过 / 0 项失败**。要点：opsd 与托管应用的 unit 都 enabled 且
  **自动**回到 active；**`/run/opsd` 由 systemd 重新创建**（`RuntimeDirectory=opsd`，
  `/run` 是 tmpfs 这件事在容器里被 `install -d` 掩盖）；磁盘上的模式/属主/凭据副本一字未变；
  **重启前创建的 Operation 重启后仍可查且终态仍是 succeeded**（任务引擎状态是持久的）；
  **重启后凭据仍逐字节到达进程**（探针开机重跑）。
- **仍未验证**：`legacy` 档（systemd 219–239，本机 systemd 257 证明不了）、
  sudoers/PAM 实际策略、AppArmor、**重启后再跑一次数据库备份闭环**
  （重启后跑的是 runtime 那一组断言）、真实生产库与大库的备份、GTID 开启的 MySQL 8、
  `prune`（2d 未实现）。

### 2026-09-24（补记八）GFS 保留策略移出迭代 2d，停放为未来迭代目标

- **变更原因**：用户指示「先不用考虑 GFS，新建一份未来迭代目标，把 GFS 保存起来」。
- **做法**：新建 `docs/plans/2026-09-24-future-iterations.md` 作为**停放区**（不是迭代规格），
  GFS 整条停放在它的第 2 节。文档里写全了「已经冻结、不用再定」的部分（D3/D9/决定 4/决定 5、
  现有的 `labels_json` 列与三个索引、`pruned` 状态与审计事件词汇），以及**开工前必须定死的
  六个语义问题**并各给推荐值：同一周期留哪一份（推荐该周期**最后**一份；已核对 restic 文档的原文措辞）、
  周期按「有备份的那些天」数而不是自然日、
  `keepLast` 与 GFS 取**并集**、`keepDays` 是**硬下限**、共享 digest 删 blob 前先查引用、
  正在被恢复的备份**跳过并计数**而不是整批失败、以及**声明了 `gfs` 而本版本不实现时怎么办**
  （推荐提交期拒绝，并记下它对既有策略的兼容性影响）。
- **为什么现在不做**：更急的那一半是 `keepLast`/`keepDays`——今天**什么都不 prune**，备份会
  无限增长，而 GFS 解决的是「很久以前也要有几份」，那是备份攒多了之后才痛的问题。
- **范围推断（需用户确认）**：GFS 移出后 2d 剩「只用 `keepLast`/`keepDays` 的 `prune` +
  验证链」。依据是「先不用考虑 GFS」≠「先不做 prune」。已写进迭代 2 文档第 22 节。
- **同一份文档的第 3 节**汇总了迭代 0–2 沉淀下来的其它待定项（`legacy` 档验证主机、
  socket 属组缺失、`runtime status` 未暴露、SELinux 加固未实现、备份账号权限预检、
  GTID 开启的 MySQL 8、大库与长时间备份、sudoers/PAM 与 AppArmor、真机上的遗留物），
  每条一句话 + 指向已有记录，此前它们散落在各迭代文档的末尾。
- **验证要求**：本轮为文档改动，未触碰产品逻辑；证据为 `make ci` 全绿。按约定提交后不等 CI 结果。
