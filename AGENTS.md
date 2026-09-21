# AGENTS.md

本文件是本仓库的协作约定，供人工开发者和 AI CLI 共同遵守。

## 语言约定（强制）

**所有产出都用中文**，包括：对话回复、`docs/` 下的文档、代码注释（Go、SQL、YAML、
Makefile、CI 配置），以及 Git 提交信息。

以下技术标识保持英文原样，不要翻译：标识符、API 字段名、错误码、命令名、日志字段
的 key、状态值（如 `pending`、`running`）。

## 项目概览

`frz-tools` 是一个 Go 实现的 Linux 运维工具，当前处于迭代 0（工程基础与设计冻结）。

- `opsd`：目标主机上的守护进程，负责执行需要权限的操作。
- `opsctl`：用户 CLI，负责发起操作、查询状态与日志。
- 两者通过 Unix Socket 上的 HTTP/JSON `api/v1` 协议通信。

设计与验收标准见：

- `docs/plans/2026-09-21-linux-ops-tool-roadmap.md`：总路线图
- `docs/plans/2026-09-21-iteration-0.md`：迭代 0 设计、实现记录与验证记录

## 代码规范

- 变量命名要有描述性；复杂条件抽取为有含义的布尔变量。
- 遵循仓库既有模式，不引入新的分层或框架。
- 注释只写必要的：解释「为什么」以及非显而易见的约束，不要复述代码本身。
- 依赖保持精简，目前只引入三个第三方依赖：`modernc.org/sqlite`、`github.com/spf13/cobra`、
  `gopkg.in/yaml.v3`。
- SQL 迁移文件放在仓库根 `migrations/`，由 `migrations` 包的 `go:embed` 导出。

## 架构说明

依赖方向：`cmd` → `internal/adapters` → `internal/application` → `internal/domain`。

- `api/v1`：对外协议类型与错误码，各层共享的叶子包。
- `internal/domain`：领域模型、状态机、错误码载体、脱敏等纯逻辑。
- `internal/application`：定义端口（`ports.go`）并编排用例，不 import 任何具体适配器。
- `internal/adapters`：SQLite、执行器、HTTP API、客户端、配置、日志等实现。

关键不变量：

- Operation 状态机只允许 `pending → {running, cancelled}` 与
  `running → {succeeded, failed, cancelled}`。
- 同一 `resource` 任意时刻最多存在一个未完成 Operation，否则提交返回 `LOCK_BUSY`。
- 长任务执行期间不持有数据库连接：worker 在短事务中领取后提交，再启动进程。
- 执行器只接受 argv，永不经过 shell。

## 常用工作流

```bash
make fmt          # 格式检查
make vet          # 静态检查
make test         # 单元 + 集成 + e2e
make test-race    # 竞态检测
make build        # 构建两个二进制到 bin/
make cross        # 交叉编译 linux/amd64 与 linux/arm64
make ci           # 以上全部，提交前必须通过
```

本地运行：

```bash
go run ./cmd/opsd --config opsd.example.yaml
go run ./cmd/opsctl --socket /run/opsd/opsd.sock health
```

## 验证约定

- 每个迭代的验收标准必须给出「命令 / 结果 / 证据类型」。
- 必须区分证据类型：静态检查、单元测试、集成测试、Linux 主机、真实服务。
- 源码检查不能替代真实 Linux 主机、systemd、Nginx、数据库实例的验证；未验证内容要显式
  标注为「未验证」。
- 若修改 API、状态机、数据表或错误码，先更新对应迭代文档并记录兼容性影响。
