# Linux 运维工具总路线图

> 文档日期：2026-09-21  
> 文档状态：Proposed / 供后续实现与交叉验证使用  
> 项目状态：空目录，尚未初始化 Go 工程和 Git 仓库

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

