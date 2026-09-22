# Linux 运维工具迭代 1a：资源模型、制品管理与配置版本化

> 文档日期：2026-09-21
> 文档状态：Proposed / 待确认设计，未实现
> 对应路线图：[2026-09-21-linux-ops-tool-roadmap.md](./2026-09-21-linux-ops-tool-roadmap.md)（第 4 节）
> 前置迭代：[2026-09-21-iteration-0.md](./2026-09-21-iteration-0.md)（已实现并提交）

## 1. 本迭代的定位

路线图第 4 节的迭代 1 范围过大，一次性冻结会让设计审查失效，因此拆成三个里程碑：

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| **1a** | 资源模型、制品管理、配置版本化 | 本文档冻结 |
| 1b | 调度器（cron/interval 计划、错过执行策略、运行历史） | 另开规格 |
| 1c | Linux 适配（`RuntimeAdapter` 端口、systemd service/timer、用户与目录管理） | 另开规格 |

拆分依据：1a 产出的 `Artifact`/`Release`/`SecretRef` 是 1b 和 1c 的共同词汇，必须先冻结；
而 1b 的调度语义和 1c 的 systemd 细节彼此独立，可以分别设计。

已确认的三个决定：

1. **交付粒度**：拆里程碑，本文件只冻结 1a。
2. **Linux 验证策略**：分三层。
   - 自动化测试层：端口 + fake 适配器 + 合约测试，可在 macOS 与 CI 上跑。
   - Linux 容器层：systemd、文件模式、属组与 Unix Socket ACL 用 Linux 容器真实验证。
     harness 已落地于 `test/linux/`，用 `make verify-linux` 运行（配方与已知坑见 `AGENTS.md`）。
   - 真实主机层：reboot 后的 unit 持久化、SELinux/AppArmor、sudoers/PAM 实际策略，
     仍挂起并显式标注「未验证」。
   1a 中依赖文件模式语义的验收项由 Linux 容器层覆盖，见第 11、12 节。
3. **Go module path**：继续使用 `frz-tools`，正式远程仓库确定后一次性全局替换 import 前缀。

## 2. 范围

### 1a 实现

- 资源模型：`Artifact`、`Application`、`Release`、`SecretRef` 的领域类型、不变量与持久化。
- 制品管理：`StorageBackend` 端口、内容寻址的本地实现、原子落盘、SHA-256 校验、留用清理。
- 配置版本化：版本识别与迁移链机制、新增制品存储配置、`SecretRef` 解析。
- 按值脱敏：解析出的 Secret 值不得进入日志、审计和错误信息。
- 对应 API 端点、CLI 子命令、migration `0002`、测试。

### 1a 不实现

- 调度器、cron/interval、运行历史（1b）。
- systemd、用户与目录管理、`RuntimeAdapter` 的真实实现（1c）。
- 部署、切流、回滚（迭代 3、4）；`Release` 在 1a 只是一条记录，没有任何部署行为。
- `Host`、`Environment` 模型：推迟到 1c，因为在此之前没有主机信息可承载，建表即为臆测。
- S3/MinIO 适配器：只冻结 `StorageBackend` 端口形状，不实现。
- 通知、metrics、Web UI（迭代 1 后半段与迭代 5）。

## 3. 资源模型

### 3.1 Artifact（不可变）

| 字段 | 说明 |
| --- | --- |
| `id` | 不透明 ID，前缀 `art_` |
| `digest` | `sha256:<hex>`，唯一，内容寻址的唯一键 |
| `size` | 字节数 |
| `mediaType` | 例如 `application/gzip`、`application/java-archive` |
| `name` | 调用方提供的展示名，仅作元数据，**不参与路径构造** |
| `createdAt` / `createdBy` | 创建信息 |
| `deletedAt` | 软删除标记，为空表示可用 |

不变量：

- `digest` 唯一；相同内容重复上传复用同一条记录，不产生新版本。
- 制品内容不可变：不存在「覆盖」语义，只有新增或软删除。
- 路径永远由 `digest` 派生，用户提供的 `name` 绝不拼进文件路径。

### 3.2 Application

| 字段 | 说明 |
| --- | --- |
| `id` | 前缀 `app_` |
| `name` | 唯一，例如 `billing-api` |
| `labels` | 键值对，用于后续分组 |
| `createdAt` / `updatedAt` | 时间戳 |

### 3.3 Release

| 字段 | 说明 |
| --- | --- |
| `id` | 前缀 `rel_` |
| `applicationId` | 引用 `Application` |
| `artifactId` | 引用 `Artifact`，必须已存在且未删除 |
| `version` | 应用内的版本号，`(applicationId, version)` 唯一 |
| `labels` | 键值对 |
| `createdAt` / `createdBy` | 创建信息 |

1a 的 `Release` 是纯记录：没有 slot、没有 active 标记、没有部署状态。这些字段要等迭代 3/4
有了实际语义再加，避免现在造出无法验证的字段。

### 3.4 SecretRef

配置和 manifest 中只出现引用，不出现值：

```yaml
secretRef:
  kind: env        # env | file
  name: PAYMENT_TOKEN
```

- `env`：从 `opsd` 进程环境读取。
- `file`：从配置允许的目录读取，文件权限必须是 `0600`，否则拒绝。
- 解析发生在**使用时刻**（构造 `CommandSpec` 时），解析结果不写库、不写日志。
- 迭代 1a 只登记 `env` 和 `file`；`vault` 等后端留到后续，解析器要做成可扩展的注册表。

## 4. 制品管理与存储

### 4.1 StorageBackend 端口

```go
type StorageBackend interface {
    Put(ctx context.Context, digest string, r io.Reader, size int64) (Stored, error)
    Open(ctx context.Context, digest string) (io.ReadCloser, error)
    Stat(ctx context.Context, digest string) (Stored, error)
    Delete(ctx context.Context, digest string) error
    List(ctx context.Context) ([]Stored, error)
}
```

`Stored` 至少含 `Digest`、`Size`、`ModifiedAt`。端口只认 `digest`，不认调用方文件名——
这是路径穿越防护的结构性保证，而不是靠字符串校验。

### 4.2 本地实现（内容寻址）

```text
<artifactRoot>/
  tmp/                          # 上传中的临时文件，必须与 blobs/ 同一文件系统
  blobs/sha256/ab/cd/<hex>      # 两级 fanout，按 digest 派生
```

- 目录 `0750`，blob 文件 `0640`，临时文件 `0600`。
- **原子落盘**：先写 `tmp/`，`fsync` 后 `rename` 到最终路径；同一 digest 已存在时丢弃临时文件
  并视为成功（幂等）。
- 写入过程中同步计算 SHA-256；与调用方声明的摘要不一致时删除临时文件并返回
  `ARTIFACT_CHECKSUM_MISMATCH`。
- 任何失败路径都必须清理临时文件；`opsd` 启动时扫描并清理 `tmp/` 中的残留。

### 4.3 上传流程

1. `opsctl artifact put <path>` 流式 `POST /api/v1/artifacts`。
2. `opsd` 先校验声明大小是否超过 `maxUploadBytes`，再校验是否超出配额。
3. 边写边算摘要 → 比对声明值（若提供）→ 原子 rename → 写 `Artifact` 行 → 写审计事件。
4. 新建返回 `201`；命中已有 digest 返回 `200` 并复用原记录。

### 4.4 留用与清理

- 清理只针对**未被任何 `Release` 引用**且超过保留策略的制品。
- 显式删除被引用的制品返回 `ARTIFACT_IN_USE`，不做级联删除。
- `opsctl artifact gc --keep <n> --older-than <dur> --dry-run` 手动触发；定时触发属于 1b 的
  调度器职责，1a 不引入后台定时任务。

## 5. 配置版本化

### 5.1 版本识别与迁移链

现状：`internal/adapters/config` 用 `yaml.Decoder.KnownFields(true)` 严格解码，并把
`apiVersion` 与 `ops.frz.io/v1alpha1` 做等值校验。问题是一旦字段发生破坏性变化，旧配置会直接
报错，没有任何迁移余地。

冻结的机制：

1. 先用**非严格**解码读出版本信封 `{apiVersion, kind}`。
2. 在迁移注册表 `map[string]func(*Config) error` 中按顺序应用从该版本到当前版本的迁移函数。
3. 再用**严格**解码（`KnownFields(true)`）读入当前结构体，最后跑语义校验。

兼容规则：

- **同一 `apiVersion` 内新增可选字段**是向后兼容的，不需要提升版本。
- **删除、改名、改变语义或改变必填性**必须提升 `apiVersion`，并提供一段迁移函数。
- 迁移函数只做数据搬运，不做 I/O；失败返回 `CONFIG_INVALID` 并指出出错的版本。
- 未登记的 `apiVersion` 明确报错，不做「尽力解析」。

### 5.2 新增配置项

```yaml
artifactStore:
  root: /var/lib/opsd/artifacts
  fileMode: "0640"
  maxUploadBytes: 2147483648
  quotaBytes: 21474836480
  retention:
    keepLast: 10
    maxAgeDays: 90

secrets:
  allowedFileDirectories:
    - /etc/opsd/secrets
```

语义校验补充：`root` 必须是绝对路径且不能是符号链接；`allowedFileDirectories` 中的每一项都
必须是绝对路径；`maxUploadBytes` 不得大于 `quotaBytes`。

## 6. 持久化设计（migration `0002`）

```sql
CREATE TABLE artifacts (
    id           TEXT PRIMARY KEY,
    digest       TEXT NOT NULL UNIQUE,
    size         INTEGER NOT NULL,
    media_type   TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    created_by   TEXT,
    deleted_at   TEXT
);

CREATE TABLE applications (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE releases (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES applications (id),
    artifact_id    TEXT NOT NULL REFERENCES artifacts (id),
    version        TEXT NOT NULL,
    labels_json    TEXT,
    created_at     TEXT NOT NULL,
    created_by     TEXT
);

CREATE UNIQUE INDEX ux_releases_application_version
    ON releases (application_id, version);
CREATE INDEX ix_releases_application ON releases (application_id, created_at);
```

设计取舍：

- 不建 `resources` 万能表。`Artifact`、`Application`、`Release` 的字段和约束差异太大，合并成
  一张宽表只会让约束失效。
- `deleted_at` 用软删除，因为 `Release` 可能引用它；硬删除需要先确认无引用，留到清理流程里做。
- 迭代 0 的 `operations` 表**不加外键**指向 `releases`：1a 的 Operation 仍然只表达
  `executor.command`，把 Operation 与 Release 关联是 1c/迭代 3 的事。

## 7. API 与 CLI

### 7.1 新增端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/artifacts` | 流式上传，`application/octet-stream`，元数据走请求头 |
| `GET` | `/api/v1/artifacts` | 列表，支持 `cursor`、`limit` |
| `GET` | `/api/v1/artifacts/{id}` | 详情 |
| `POST` | `/api/v1/artifacts/{id}/verify` | 从磁盘重算摘要并与记录比对 |
| `DELETE` | `/api/v1/artifacts/{id}` | 软删除，被引用时返回 `ARTIFACT_IN_USE` |
| `POST` | `/api/v1/applications` | 创建应用 |
| `GET` | `/api/v1/applications` / `{id}` | 列表与详情 |
| `POST` | `/api/v1/releases` | 登记发布记录 |
| `GET` | `/api/v1/releases/{id}` | 详情 |
| `GET` | `/api/v1/applications/{id}/releases` | 应用下的发布列表 |
| `POST` | `/api/v1/artifacts/gc` | 触发清理，请求体含 `dryRun` |

上传接口的请求头：`X-Artifact-Name`、`X-Artifact-Media-Type`、`X-Artifact-SHA256`（可选，用于
校验）。沿用既有约定：所有响应带 `apiVersion`，错误用 `ErrorResponse` 信封，请求体大小受限。

### 7.2 新增 CLI

```text
opsctl artifact put <path> [--media-type <m>] [--sha256 <hex>] [--name <n>]
opsctl artifact list [--limit <n>] [--cursor <n>]
opsctl artifact inspect <artifact-id>
opsctl artifact verify <artifact-id>
opsctl artifact delete <artifact-id>
opsctl artifact gc [--keep <n>] [--older-than <dur>] [--dry-run]

opsctl app create <name> [--label k=v]
opsctl app list
opsctl app inspect <name-or-id>

opsctl release add --app <name> --version <v> --artifact <id> [--label k=v]
opsctl release list --app <name>
opsctl release inspect <release-id>
```

沿用迭代 0 的全局旗标 `--socket`、`--timeout`、`--json`，以及「默认人类可读、`--json` 输出原始
JSON」的约定。

## 8. 错误码与退出码增量

迭代 0 已占用 `1/2/3/4/10/11/12/13/20`。新增：

| 错误码 | HTTP | CLI 退出码 | 含义 |
| --- | ---: | ---: | --- |
| `ARTIFACT_NOT_FOUND` | 404 | 2 | 制品不存在或已删除 |
| `ARTIFACT_CHECKSUM_MISMATCH` | 409 | 5 | 上传内容与声明摘要不一致 |
| `ARTIFACT_IN_USE` | 409 | 6 | 制品仍被 Release 引用 |
| `UPLOAD_TOO_LARGE` | 413 | 7 | 超过 `maxUploadBytes` |
| `STORAGE_QUOTA_EXCEEDED` | 507 | 8 | 超过 `quotaBytes` |
| `SECRET_UNRESOLVED` | 400 | 9 | SecretRef 无法解析（缺失、越权、权限不对） |
| `APPLICATION_NOT_FOUND` | 404 | 2 | 应用不存在 |
| `RELEASE_NOT_FOUND` | 404 | 2 | 发布记录不存在 |
| `RELEASE_CONFLICT` | 409 | 14 | 同一应用下版本号重复 |

## 9. 兼容性与迁移影响

- **API**：只新增端点和字段，迭代 0 的现有请求与响应形状不变。回归测试必须覆盖迭代 0 的
  全部端点行为。
- **数据表**：migration `0002` 只做 `CREATE TABLE`，不修改迭代 0 的 `operations`、
  `operation_logs`、`resource_locks`、`audit_events`。迁移版本单调递增，可重复执行。
- **错误码**：只新增，不修改既有码的含义和退出码。
- **配置**：新增段落为可选；不配置时制品相关端点返回 `CONFIG_INVALID` 并说明缺失项，而不是
  启动失败——这样迭代 0 的既有部署升级后不会被强制要求配置制品存储。
- **状态机**：不修改。`Operation` 的转换规则与 1a 无关。

## 10. 测试计划

### 单元测试

- 制品摘要计算、声明摘要比对、超限拒绝。
- 原子落盘：模拟写入中途失败，断言 `tmp/` 无残留、blobs 无半成品。
- 路径穿越：含 `../`、绝对路径、超长名称的 `name` 不影响 blob 路径。
- 引用计数与 GC：被引用制品不被清理，`--dry-run` 不产生删除。
- `SecretRef`：`env` 与 `file` 解析；`file` 权限非 `0600` 被拒绝；越权目录被拒绝。
- **按值脱敏**：Secret 值不得出现在日志、审计、错误信息中（对输出做子串断言）。
- 配置迁移链：旧 `apiVersion` 升级成功、未登记版本明确失败、未知字段仍报错。

### 合约测试

新建共享合约套件，任何 `StorageBackend` 实现都必须通过：`Put`/`Open`/`Stat`/`Delete`/`List`
的幂等性、缺失 digest 的错误、并发 `Put` 同一 digest、大小与摘要一致性。本地实现先过，
后续 S3/MinIO 直接复用同一套件。

### 集成与 e2e

- 上传 → 落盘 → 列表 → 校验 → 删除的完整链路，走真实二进制与真实 Unix Socket。
- 中断上传（客户端断开）后无残留、无 Artifact 行。
- 同一文件重复上传返回同一条记录。
- 被 Release 引用的制品删除返回 `ARTIFACT_IN_USE`。
- 迭代 0 端点的回归用例继续全绿。

## 11. 1a 验收标准

每条都要给出「命令 / 结果 / 证据类型」并写入本文档的验证记录。证据类型必须区分
静态 / 单元 / 集成 / **Linux 容器** / Linux 主机。

1. 上传制品并原子落盘；写入中断不留半成品、不产生记录。
2. 摘要校验：声明值与实际内容不一致时拒绝且不落库。
3. 制品不可变：相同内容重复上传复用同一条记录。
4. 内容寻址与路径穿越防护通过恶意输入测试。
5. `Name` 等用户输入不参与任何文件路径构造。
6. GC 只清理未被引用的制品，`--dry-run` 不删除任何东西。
7. `SecretRef` 解析出的值不落库、不进日志、不进审计。
8. 配置迁移链可用；未知字段仍被拒绝；缺省配置下迭代 0 行为不变。
9. `StorageBackend` 合约套件在本地实现上全绿。
10. `make ci` 全绿（格式、vet、单元、集成、e2e、`-race`、linux amd64/arm64 交叉编译）。

**证据类型限制（重要）**：第 1、2、6、7 条中涉及文件模式（blob `0640`、目录 `0750`、SecretRef
文件必须 `0600`）和属组语义的部分，**在 macOS 上无法证明**——macOS 与容器 bind mount 都不保真
Linux 权限语义。这些条目必须通过 `make verify-linux` 在 Linux 容器内验证，证据类型标注为
「Linux 容器」，不得用 macOS 上的测试结果替代。

## 12. 未验证内容

以下内容在 1a 完成后仍未验证，不得用源码检查替代：

- 制品存储的文件模式（`0640`/`0750`）与 `SecretRef` 文件 `0600` 的实际强制效果：由
  `make verify-linux` 覆盖（Linux 容器），但**真实 Linux 主机上的 umask 与挂载选项差异**
  仍未验证。
- 真实 S3/MinIO 后端行为（1a 只冻结端口）。
- systemd 的真实控制逻辑（属于 1c）；即使 1c 完成，reboot 持久化与 SELinux/AppArmor 仍需
  真实 Linux 主机。
- 备份、部署、切流相关的一切（属于后续迭代）。

## 13. 未决事项

进入 1b / 1c 前必须冻结：

- 制品的 `mediaType` 取值集合与解包方式约定（1c 需要）。
- `Application` manifest 的完整字段（1c 需要，路线图第 6 节已给出草稿）。
- `RuntimeAdapter` 端口的精确签名（1c）。
- `Host`、`Environment` 模型的字段（1c）。
- 调度器的时区与错过执行策略（1b）。

以下事项不阻塞 1a，但需要持续跟踪：

- 正式远程仓库对应的 module path（当前 `frz-tools` 是临时的）。
- 认证与多用户隔离：1a 的 API 仍然无鉴权，与迭代 0 一致，多人环境不可用。
- **Linux 容器验证 harness 的搭建时机**：已完成，位于 `test/linux/`（`make verify-linux`），
  先用迭代 0 的代码验证通过（22 项断言全绿），并已接入 CI 独立 job。1a 的制品存储与
  `SecretRef` 权限断言直接复用它。
