# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 本仓库的协作约定以 [AGENTS.md](AGENTS.md) 为准，本文是给 Claude Code 的操作摘要。
> 两者冲突时以 AGENTS.md 为准；约定有变更请改 AGENTS.md，不要只改这里。
> 仓库地址 <https://github.com/freezeChen/frz-tools>，module path `github.com/freezeChen/frz-tools`。

## 语言约定（强制）

**所有产出都用中文**：对话回复、`docs/` 下的文档、代码注释（Go、SQL、YAML、Makefile、
CI 配置）、Git 提交信息。以下技术标识保持英文原样，不要翻译：标识符、API 字段名、错误码、
命令名、旗标名、日志字段的 key、状态值（`pending`、`running` 等）、`--json` 输出字段名。

CLI 的两个二进制（`opsd`、`opsctl`）面向用户的文案一律中文——命令 Short/Long 帮助、旗标说明、
报错与提示；但命令名、旗标名与可复制粘贴的 shell 片段保持英文。本地化在 `internal/cliutil`，
`opsctl` 还要额外改写 cobra 内置的 `help` 与 `completion` 命令文案（见 `cmd/opsctl/main.go`
的 `localizeBuiltinCommands`）。

## 常用命令

```bash
make fmt           # gofmt -l 检查，有未格式化文件即失败
make vet           # go vet ./...
make test          # go test ./...（含 test/e2e，会真起 opsd 进程）
make test-race     # go test -race ./...
make build         # 构建 opsctl/opsd 到 output/
make cross         # 交叉编译 linux/amd64 与 linux/arm64
make verify-linux  # Linux 容器验证（需要 docker，不纳入 ci）
make verify-db     # 真实 PostgreSQL/MySQL/MariaDB 实例上的备份适配器验证（需要 docker）
make verify-host   # **真实 Linux 主机**上的验证（需要一个能 ssh 的主机，不进 CI）
make ci            # fmt + vet + test + test-race + cross，提交前必须通过
```

跑单个测试：

```bash
go test ./internal/application -run TestArtifactPutIsContentAddressed -v
```

本地起一对进程：

```bash
go run ./cmd/opsd --config opsd.example.yaml
go run ./cmd/opsctl --socket /run/opsd/opsd.sock health
```

`opsctl` 的 socket 默认 `/run/opsd/opsd.sock`，可用 `--socket` 或环境变量 `OPSD_SOCKET` 覆盖。
`opsd` 默认配置 `/etc/opsd/config.yaml`。

**提交后不等 CI 结果**（约定见 AGENTS.md）：文档、注释、单点修复、测试修正这类**非大模块**
改动，本地 `make ci` 通过后即可提交推送并继续下一步，不要在推送后轮询流水线；CI 由独立的
定时任务 `check-main-ci` 兜底（每小时的 :13 与 :43 检查 `main`，小范围失败直接修，其余只汇报）。
只有**大模块**提交（新增端口/适配器、动状态机或数据表、改错误码、跨多包重构）才值得等 CI。
无论等与不等，引用 CI 结论都必须给出真实运行号与结果。

## 架构

依赖方向单向：`cmd` → `internal/adapters` → `internal/application` → `internal/domain`。

- `api/v1`：对外协议类型、错误码与退出码映射，各层共享的叶子包。
- `internal/domain`：领域模型、状态机、错误码载体、脱敏，纯逻辑无 IO。
- `internal/application`：在 `ports.go` 定义端口并编排用例。**非测试代码不 import 任何具体适配器**。
- `internal/adapters`：sqlite、executor、httpapi、client、config、blob、secret、manifest、runtime。
- `internal/fleet`：**客户端侧**的批量发布编排（5b），只经 `HostClient` 接口与远端说话，
  刻意不持有任何状态（没有库、没有批次表）。
- `internal/pki`：证书/私钥文件 → `crypto/tls` 配置，外加私钥文件的模式与属主检查。**opsd 与
  opsctl 共用**，两边对「什么样的私钥才算合格」的判据只有一份。

`RuntimeAdapter`（`internal/application/ports.go`）把应用规格映射到具体运行时，有两个实现：
`runtime/systemd`（真实）与 `runtime/proc`（假适配器，让 macOS/CI 能跑同一套合约）。两者
**共用** `runtime/unitfile`（unit 渲染与转义）与 `runtime/readiness`（tcp/http 就绪探测与
连续成功计数），不允许各自实现一份。任何新实现都必须通过
`internal/application/runtimecontract` 的共享合约测试——`proc` 行为若与 `systemd` 漂移，
本地测试通过就毫无意义。

适配器的**平台选择只有一处**：`cmd/opsd/main.go` 里 `GOOS == "linux"` 才注入 systemd。
刻意**不提供** `runtime.adapter` 之类的配置开关，避免生产误选 `proc` 假适配器；非 Linux 上
`runtime.*` 返回 `RUNTIME_UNSUPPORTED`。`UnitDecision` → `RuntimeDecision` 的翻译在装配层
（`cmd/opsd/runtime.go`），不进端口——它进不了共享合约测试，塞进端口只会逼每个实现各编一份。

依赖装配集中在 `application.NewRuntime`（`internal/application/runtime.go`）：它把操作服务、
制品/目录/规格/主机服务、调度器与 worker 池组合起来共享同一个取消注册表与唤醒通道。
适配器实例只建一次，systemd 的版本探测与就绪计数是实例态，建两份会让同一次 `runtime.start`
里的探测结果互相不可见。

### 关于 `internal/application` 的测试

"不 import 具体适配器"只约束非测试代码。现状的例外：外部测试包
`artifact_test.go`（`package application_test`）import `adapters/sqlite` 与 `adapters/blob`；
同包内测试（`scheduler_test.go`、`service_test.go`、`runtimeops_test.go`）只 import `sqlite`。
不成环的原因是 `sqlite` 只依赖 `domain`，而 `blob` 依赖 `application`——`blob` 一旦出现在
**同包内**测试里就会形成 import 环，所以它只能出现在外部测试包。新增测试优先用本地假实现
（`proc` 适配器、内存端口）；确实需要真适配器的按集成测试对待，并在提交信息里说明为什么
不能用假实现。

## 关键不变量

- Operation 状态机只允许 `pending → {running, cancelled}` 与
  `running → {succeeded, failed, cancelled}`（`internal/domain/operation.go`）。
- 同一 `resource` 任意时刻最多一个未完成 Operation，否则提交返回 `LOCK_BUSY`。
- 长任务执行期间**不持有数据库连接**：worker 在短事务中领取并提交，之后才启动进程。
- 执行器只接受 argv，**永不经过 shell**；systemd 适配器与版本探测同样遵循。
- `Health`（能否接流量）与 `Status`（进程本身状态）刻意不合并——进程活着不等于已就绪。
- `runtime.start` = 幂等 `Prepare` + `Start`（合约要求未 Prepare 的 Start 必须被拒绝）；
  `runtime.stop` 只 `Stop`。`runtime.validate`/`prepare`/`health` 是同步用例、不进 Operation。
  **`runtime.*` 一律拒绝 `dryRun`**（适配器端口没有 dry-run 语义），只做校验请用 `runtime validate`。
- `runtime.start`/`stop` 与 `POST /api/v1/operations` 走同一条 `Service.Create`：锁、幂等键、
  请求摘要、审计、日志、取消全部复用，因此 `operation get/logs/cancel/retry` 对它们同样适用。
  `runtime.*` 读的是应用的**当前**规格，不是创建时的快照。
- **批量发布（迭代 5b）在客户端侧**：`internal/fleet` 编排、`opsctl app deploy/rollback
  --hosts` 驱动，**批次状态不落库**——每台机上的 `opsd` 是它那次部署的唯一事实来源，
  批次只是一个视图，批次号就是每台机上的幂等键（`<batchID>:<action>:<application>`），
  「继续」= 同名重跑、「重来」= 换一个批次号。三条不变量：**部署目标是声明的意图而不是
  观测状态**（探测不通的机器必须仍出现在批次里，标成 `skipped`）；**汇总恒等式**
  成功+失败+跳过+未执行=总数；**准备阶段先行**（任何一台部署之前完成全员检查），默认
  不过就一台都不动（`BATCH_PREFLIGHT_FAILED` 36），跑完但有主机没成功是 `BATCH_FAILED` 37
  ——两个码分开「什么都没发生」与「动了一部分」。`--hosts` 模式要求 manifest 按
  `artifact.digest` 引用制品（`artifact.id` 只在某一台机上有意义）。
- **远程（迭代 5a）默认是关的**：没有 `remote.listen` 就完全不听 TCP。开了之后是 mTLS
  （最低 TLS 1.2，为兼容 legacy 档 CentOS 7 的 openssl 1.0.2），**握手只验 CA、授权在
  HTTP 层**（默认拒绝的 CN 白名单 + `read`/`write` + 可选应用白名单）——这样
  「证书没签对」与「身份没被授权」是两种不同的错误。**`executor.command` 对远程全禁**。
- **每条路由在注册处声明访问档位**（`httpapi/server.go` 的 `routes()`，唯一来源；漏写启动
  即崩），**「谁做的」只在 `application.Service.Create` 一处决定**（远程用证书 CN，
  本机用 `createdBy`）。这两条都是为了让「新增端点 / 新增写入口」不可能静默越权——
  5a 实现时正是两处「授权代码失效却不报错」被自己的断言抓出来的（见迭代 5 文档 12.3）。
- **`releases` 子树内部归 `ReleaseAdapter`**（`domain.InReleaseTree`）：根由物化建、release 目录
  由解包建、`current` 由切换建。运行时适配器的 `Prepare` **不得**把 `current`（或任何子树内的
  路径）建成实体目录——那样符号链接切换会因为改名目标是目录而失败，而报出来的是「改名失败」。
  `runtime.*` 的 `Stop`/`Status`/`Health` 只走 `validateSpec`（不查 `argv[0]`）：解释器或可执行
  文件从盘上消失时，服务仍然必须能停下来、状态仍然必须问得出来。
- **部署失败时的错误码只说一件事**：`DEPLOY_ROLLED_BACK` = 「改动过线上并且把它撤销了」。
  什么都没被动过（失败发生在本版本被应用之前）时返回**原因码**；`current` 切过或旧版本被停过
  都必须撤销，两者是两个独立的标记（`switched` / `stoppedPrevious`）。

### 版本化信封与迁移

配置（`adapters/config`）与 manifest（`adapters/manifest`）用同一套解码流程：先非严格解码读出
`apiVersion` 信封，按版本走迁移链，再用 `KnownFields(true)` 严格解码。同一 `apiVersion` 内新增
可选字段是兼容的；**删除字段、改名、改变语义或必填性，必须提升 `apiVersion` 并登记一段迁移**。
不要修改历史版本的语义或直接删历史决策。

SQL 迁移在仓库根 `migrations/`，由 `migrations` 包的 `go:embed` 导出，
`internal/adapters/sqlite` 按文件名排序执行并记录到 `schema_migrations`。

## 验证纪律

- 证据类型必须显式区分：静态检查 / 单元测试 / 集成测试 / e2e / **Linux 容器** / **Linux 主机**。
  「Linux 容器」与「Linux 主机」**不得混用，也不得互相替代**。
- 未验证的内容必须显式标注「未验证」，不得写成已验证或"已支持"。**代码落地 ≠ 验证通过**。
- 修改 API、状态机、数据表或错误码，**先更新对应迭代文档**并记录兼容性影响。
- 设计文档在 `docs/plans/`：`2026-09-21-linux-ops-tool-roadmap.md` 是总路线图（追加式变更记录，
  不删历史决策），`2026-09-21-iteration-{0,1a,1b,1c,1d,2}.md` 与 `2026-09-24-iteration-3.md`
  是各迭代规格与验证记录，
  `2026-09-25-iteration-4.md` 是 Nginx 蓝绿的规格与验证记录（4a/4b/4c 均已实现并在
  **Linux 容器**里验证，见第 13–20 节；**legacy 档真机**上一整轮也跑通了，见第 21 节），
  `2026-09-27-iteration-5.md` 是远程/多主机的规格与验证记录
  （**5a 与 5b 已实现**：5a = 远程连接与主机注册，5b = 部署目标与批量发布；5c–5e 未开始。
  **5b 的「多主机上都真的换了版本」未验证**，理由见该文档 §16.3），
  `2026-09-24-future-iterations.md` 是**未来迭代目标的停放区**（GFS 保留策略 +
  迭代 0–2 沉淀下来的待定项；**不是规格**，开工前要先升级成迭代文档）。验收标准
  必须给出「命令 / 结果 / 证据类型」。**1d（重试与并发策略）已于 2026-09-24 实现并提交**；
  **迭代 2a（备份）与 2b（PostgreSQL/MySQL/MariaDB 适配器、数据库隔离恢复）均已实现并验证**；
  **2d（保留策略与 `prune`）也已实现并验证**，范围是只用 `keepLast`/`keepDays`；
  **GFS 已移出**并停放在停放区第 2 节（未实现）。
  **迭代 3 的 3a / 3b / 3c 都已实现并验证**（制品解包成 release 目录、部署与回滚、资源限制与
  Java 运行时的解释器预检），含「部署出来的 release 跨重启存活」的真机证据。
  **迭代 5 的 5a（远程连接与主机注册）已实现并验证**：`opsd` 可选的 mTLS 监听、证书身份、
  默认拒绝的 CN 白名单（档位 + 应用白名单）、审计绑定认证身份、`GET /api/v1/identity`，
  以及 `opsctl` 的 `--host` / `--remote` / `identity` / `host check` / `host list --check`。
- `make verify-linux` 的断言清单与断言数以 `test/linux/verify.sh` 为准，权威数字是脚本运行时打印的
  「`%d` 项通过，`%d` 项失败」（迭代 4c 落地后为 **232 项**，5a 的回归复跑同为 **232/0**）；
  不要引用静态推导值或历史快照当结论。
- **不得把未验证项写成已验证**。迭代 5b 新增的未验证项：**批量发布「多台主机上都真的
换了版本」**（容器里造不出来——systemd 的 unit 名字空间是机器级的，同一台机上两个 `opsd`
实例部署同名应用写的是同一个 unit 文件，那里的「一台成功、一台失败」是测试装置的产物；
e2e 只证明编排与协议接线那一半。补齐需要**两台真实主机**）、大规模并发与波次（没压过）、
部分失败现场的人工处置流程。迭代 5a 新增的未验证项：**跨物理主机**（全部远程断言都在
  同一台机的两个 opsd 实例之间，真机上也**没加**远程段）、**证书的签发与轮换**（不做，
  运维用 openssl 手工来，CRL/OCSP 都没有）、**SELinux/防火墙对监听端口的影响**、
  **RBAC/审批/操作签名/审计导出**（5c）、**批量发布**（5b）。
  原有明确未验证：`legacy` 档的 **232～239 那一段**
  （219 已在真实主机上验证，见 `2026-09-21-iteration-1c.md` 第 19 节）、
  sudoers/PAM 实际策略、
  `SudoConfig`（只有模型、零行为）、GFS 保留、store-wide 的备份孤儿回收、
  真实生产库与大库的备份、
  GTID 开启的 MySQL 8、大容量长时间备份、**strict 档上修复后的 3c 复跑**（那台主机不可达）、
  **蓝绿只在 legacy 档真机上验过**（2026-09-27，183 项通过 / 0 项失败）：**strict 档**上的
  蓝绿、以及**蓝绿跨机器重启存活**仍未验证（容器里有真 Nginx 与真 systemd，但那仍是
  「Linux 容器」证据）；另有两条设计边界见 `2026-09-25-iteration-4.md` §18.6
  （受管文件 ≠ nginx 已加载的配置、对账只发生在启动时）。
  SELinux 的措辞要精确：**enforcing 下的行为已观测**（进程落在 `unconfined_service_t`），
  本工具**不提供** SELinux 加固——这是「未实现的能力」，不得写成「已支持」。
- **真实 Linux 主机**上的验证用 `make verify-host`（`test/host/`，需要一台能 ssh 的主机，
  因此不进 CI）。两台主机、两个档位都已跑过：
  - **strict 档**（Rocky Linux 10.2 / systemd 257 / SELinux enforcing，`192.168.11.101`）：
    `full` **95/0**（1c 的 73 + 2b 的 22），重启验证 **21/0**（停机 44 秒）；迭代 3c 那一轮
    **111/1**——唯一失败项是一个真实缺陷（已修复），**修复后没在这台上复跑**（那台机后来
    整段网段不可达）。
  - **legacy 档**（CentOS 7 / systemd 219 / cgroup v1，`43.142.95.141`）：最近一轮 `full`
    **183/0**（2026-09-27，迭代 4 之前那一轮是 118/0），含迭代 3c 整段（真 JAR、真 JVM、
    `MemoryLimit=` 落到 cgroup v1、解释器预检）与**迭代 4 的蓝绿段**（EPEL 的 nginx 1.20.1：
    两次部署 + 真切流 + 回滚 + `app slot list/history` + 重启 opsd 服务之后的对账）；
    重启验证
    `prepare` **116/0** + `check` **28/0**——`boot_id` 前后不同、`/run/opsd` 由
    `RuntimeDirectory=` 重建、**部署出来的 release 自己回来且 `current` 未变**。
    跑法：`FRZ_HOST=root@43.142.95.141 FRZ_HOST_JAVA_HOME=/opt/jdk-17.0.20.1+1 make verify-host`。
  - 重启验证用 `FRZ_HOST_PHASE=prepare` → 重启 → `FRZ_HOST_PHASE=check`；`check` 阶段
    **不重新上传**（否则会把跨重启的记录冲掉），自动重启时必须**等它先下线再上线**，
    否则 `check` 跑在同一代 boot 上——那条 `boot_id` 断言会（正确地）拒绝。
  - **在 `192.168.11.101` 上禁止 `pkill` / `killall` 这类宽匹配的杀进程方式**：上面跑着
    不在 systemd 下的业务 JVM（`/home/data/ems/ems-server`），一次 `pkill -x java` 把它一起杀了。
- 备份适配器的真实实例验证用 `make verify-db`（`test/linux/verify-db.sh` + `test/dbbackup`，
  由 `FRZ_TEST_*_DSN` 控制，未设置时跳过）。它跑的是 `internal/application/backupcontract`
  的共享合约——**新增任何 `BackupAdapter` 实现都必须过同一套**。

## 已知的坑

- **MySQL/MariaDB 里 `||` 是逻辑或，不是字符串拼接**（PostgreSQL 里才是拼接）。测试夹具
  里写「内容指纹」时用了 `||`，指纹就**永远是 1**，于是「备份 → 清空 → 恢复 → 内容一致」
  这条断言退化成「表里有至少一行」，还会一路绿着通过。用 `CONCAT(...)`。`test/dbbackup`
  里有一条元断言（`TestFingerprintIsContentSensitive`）专门钉这件事：**断言测不出东西
  比断言失败更危险**。

Linux 容器 harness 的固定配方与两个坑见 AGENTS.md「Linux 容器验证的固定配方」。补充两条：

- 不得把 macOS 目录 bind mount 进容器做权限测试（virtiofs 不保真属主与模式）。
- `docker cp` 不要写进容器的 `/tmp`（`--tmpfs /tmp` 会遮住根文件系统里的同名目录）。
- `test/linux/verify.sh` 里变量名后紧跟中文全角字符时必须写 `${var}`：macOS 自带的 bash 3.2
  会把多字节字符的前几个字节算进变量名并报 `unbound variable`，CI 上的 bash 5 不会暴露这个问题。
- `/etc/opsd` 的 `750` 与 `0751` 两条断言不是矛盾而是时序：前者在 `Prepare` 之前（安装脚本状态），
  后者在 `Prepare` 之后（`kind: file` 凭据要能被运行用户穿越）。改动权限、属组、socket 或 systemd
  相关逻辑后必须单独跑 `make verify-linux`。

## 提交约定

一个里程碑一个提交，提交信息用中文：简短摘要行 + 逐文件的要点正文。
