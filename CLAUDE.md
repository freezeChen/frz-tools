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

### 版本化信封与迁移

配置（`adapters/config`）与 manifest（`adapters/manifest`）用同一套解码流程：先非严格解码读出
`apiVersion` 信封，按版本走迁移链，再用 `KnownFields(true)` 严格解码。同一 `apiVersion` 内新增
可选字段是兼容的；**删除字段、改名、改变语义或必填性，必须提升 `apiVersion` 并登记一段迁移**。
不要修改历史版本的语义或直接删历史决策。

SQL 迁移在仓库根 `migrations/`（`0001`…`0005`），由 `migrations` 包的 `go:embed` 导出，
`internal/adapters/sqlite` 按文件名排序执行并记录到 `schema_migrations`。

## 验证纪律

- 证据类型必须显式区分：静态检查 / 单元测试 / 集成测试 / e2e / **Linux 容器** / **Linux 主机**。
  「Linux 容器」与「Linux 主机」**不得混用，也不得互相替代**。
- 未验证的内容必须显式标注「未验证」，不得写成已验证或"已支持"。**代码落地 ≠ 验证通过**。
- 修改 API、状态机、数据表或错误码，**先更新对应迭代文档**并记录兼容性影响。
- 设计文档在 `docs/plans/`：`2026-09-21-linux-ops-tool-roadmap.md` 是总路线图（追加式变更记录，
  不删历史决策），`2026-09-21-iteration-{0,1a,1b,1c,1d,2}.md` 是各迭代规格与验证记录。验收标准
  必须给出「命令 / 结果 / 证据类型」。**1d（重试与并发策略）已于 2026-09-24 实现并提交**；
  **迭代 2（数据库与资源备份）的规格也已冻结**（拆成 2a–2d，先做 2a），实现待做。
- `make verify-linux` 的断言清单与断言数以 `test/linux/verify.sh` 为准，权威数字是脚本运行时打印的
  「`%d` 项通过，`%d` 项失败」；不要引用静态推导值或历史快照当结论。
- **不得把未验证项写成已验证**。当前明确未验证：`legacy` unit 档（systemd 219–239）、真实主机的
  reboot 后 unit 持久化、SELinux/AppArmor、sudoers/PAM 实际策略、`SudoConfig`（只有模型、零行为）。

## 已知的坑

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
