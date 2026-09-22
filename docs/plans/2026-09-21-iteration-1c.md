# Linux 迭代 1c：Linux 适配（RuntimeAdapter 与 systemd）

> 文档日期：2026-09-22
> 文档状态：Proposed / 待确认设计，未实现
> 对应路线图：[2026-09-21-linux-ops-tool-roadmap.md](./2026-09-21-linux-ops-tool-roadmap.md)（第 4 节「Linux 适配」）
> 前置迭代：[2026-09-21-iteration-1a.md](./2026-09-21-iteration-1a.md)（已实现并提交）

## 1. 定位与前置

1c 把「一个应用规格」落成目标机上的真实进程管理：专用用户、目录、环境文件、systemd unit，
并提供校验、准备、启停与健康检查。它是迭代 3（发布流程）与迭代 4（蓝绿切流）的基础——
**1c 只提供可靠的原语，不做发布编排**。

验证策略沿用已确认的三层（见 1a 第 1 节第 2 条）：

1. 端口 + 假适配器 + 共享合约测试，可在 macOS 与 CI 上跑。
2. Linux 容器：真实 systemd（`--cgroupns=host` 配方已验证并记录在 `AGENTS.md`）。
3. 真实 Linux 主机：reboot 后 unit 持久化、SELinux/AppArmor、sudoers/PAM —— **挂起并标注未验证**。

## 2. 范围

### 1c 实现

- `RuntimeAdapter` 端口与共享合约测试。
- systemd 适配器：unit 生成、安装、启用、启停、状态与健康检查。
- `proc` 假适配器：在 macOS 与 CI 上跑同一套合约测试。
- Application manifest（`ApplicationSpec`）的字段冻结、严格解码与校验。
- `Host`、`Environment` 模型与持久化。
- 制品下载端点（1a 推迟到这里的缺口）。
- 用户、目录、环境文件、凭据目录的管理。
- migration `0004`，API 端点与 CLI。

### 1c 不实现

- 发布流程、release 目录、失败清理、健康通过才接流（迭代 3）。
- slot 与蓝绿、Nginx、切流与回滚（迭代 4）。
- 远程主机、SSH、mTLS、批量执行（迭代 5）。
- systemd timer：**已决定不生成**（2026-09-22）。调度权威归 1b 的 `opsd` 进程内调度循环，
  1c 只生成 `.service`，避免同一条计划被两套机制各触发一次。详见 1b 第 13 节第 2 条。

## 3. RuntimeAdapter 端口

```go
// RuntimeAdapter 把「应用规格」映射到具体运行时。实现方必须通过共享合约测试。
type RuntimeAdapter interface {
	// Validate 只检查规格能否被本适配器执行，不产生任何副作用。
	Validate(ctx context.Context, spec *domain.ApplicationSpec) error
	// Prepare 创建用户、目录、环境文件与 unit 文件；幂等，可重复调用。
	Prepare(ctx context.Context, spec *domain.ApplicationSpec) error
	Start(ctx context.Context, spec *domain.ApplicationSpec) error
	Stop(ctx context.Context, spec *domain.ApplicationSpec) error
	// Health 返回是否就绪；Status 返回底层状态，二者不合并，
	// 因为「进程活着」与「已就绪」是两件事。
	Health(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeHealth, error)
	Status(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeStatus, error)
}
```

`domain.RuntimeStatus`：`active`、`inactive`、`failed`、`activating`、`deactivating`、`unknown`。
`domain.RuntimeHealth`：`Ready bool`、`CheckedAt time.Time`、`Detail string`。

合约测试套件放 `internal/application/runtimecontract`，断言至少覆盖：
`Validate` 无副作用、`Prepare` 幂等、`Start` 后 `Status=active`、`Stop` 后 `Status=inactive`、
对未 `Validate` 通过的规格执行 `Start` 必须被拒绝、`Health` 在未启动时不就绪。

## 4. Application manifest（冻结）

YAML，严格解码（`KnownFields(true)`），`apiVersion: ops.frz.io/v1alpha1`、
`kind: ApplicationSpec`：

```yaml
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go                     # go | java
artifact:
  id: art_XXXX                  # 或 digest: sha256:...
  unpack:                       # 1a 预留的 mediaType 解包约定，1c 冻结
    strategy: none               # none | tar | tar-gz | zip
    stripComponents: 1
exec:
  argv: [/opt/billing-api/bin/billing-api]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
  environment:
    GOMEMLIMIT: 40MiB
  secretEnvironment:
    DB_PASSWORD:
      kind: file
      name: /etc/opsd/apps/billing-api/db_password
  ports: [8080]
health:
  readiness:
    type: tcp                    # tcp | http | exec
    target: "127.0.0.1:8080"
    consecutiveSuccesses: 2
  startTimeoutSeconds: 60
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/billing-api
systemd:
  unitName: billing-api.service
  restartPolicy: on-failure      # always | on-failure | no
```

校验要求（创建时就失败，不到触发时才失败）：

- `application`、`runtime`、`exec.argv`、`exec.runUser`、`exec.workingDirectory` 必填。
- `argv[0]`、`workingDirectory`、`logs.directory` 必须是绝对路径。
- `runtime` 取值限定 `go`/`java`；`unpack.strategy` 限定 `none`/`tar`/`tar-gz`/`zip`。
- `health.readiness.type=http` 时 `target` 必须能解析为 URL；`tcp` 时能解析为 host:port。
- `secretEnvironment` 复用 1a 的 `SecretRef` 校验，且不得与 `environment` 撞键。
- `unitName` 必须以 `.service` 结尾，且只允许 `[A-Za-z0-9_.@-]`。

`mediaType → 解包方式` 约定：`application/x-tar`→`tar`、`application/gzip`
与 `application/x-gzip`→`tar-gz`、`application/zip`→`zip`；其余默认 `none`（当作裸二进制）。
若 manifest 显式声明的策略与 mediaType 冲突，返回 `MANIFEST_CONFLICT`。

## 5. Host 与 Environment

1c 只建身份与标签，不承载连接语义（远程属迭代 5）：

| 模型 | 字段 |
| --- | --- |
| `Host` | `id`、`name`（唯一）、`address`（本机为空）、`labels`、时间戳 |
| `Environment` | `id`、`name`（唯一）、`labels`、时间戳 |

启动时自动确保一条 `address` 为空的本机 `Host` 记录存在，方便后续把应用挂到具体主机上。
**不**在此迭代做「应用绑定主机」的校验，留给迭代 3/5。

## 6. systemd unit 模板

生成到 `/etc/systemd/system/<unitName>`，`0644`，随后 `daemon-reload`：

```ini
[Unit]
Description=<application>
After=network.target
Wants=network.target

[Service]
Type=simple
User=<runUser>
Group=<runUser>
WorkingDirectory=<exec.workingDirectory>
EnvironmentFile=-/etc/opsd/apps/<application>.env
LoadCredential=<name>:/etc/opsd/apps/<application>.credentials/<name>
ExecStart=<argv，按 systemd 规则转义>
Restart=<restartPolicy>
RestartSec=5
TimeoutStartSec=<health.startTimeoutSeconds>
TimeoutStopSec=<health.stopTimeoutSeconds>
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=<exec.workingDirectory> <logs.directory> <artifact 解包目录>
StandardOutput=append:<logs.directory>/current.log
StandardError=append:<logs.directory>/current.log

[Install]
WantedBy=multi-user.target
```

### systemd 版本兼容档（已承诺支持 ≥ 219）

上面的模板是「新档」。已决定支持 systemd ≥ 219（CentOS 7 起），而模板里有多条指令在
219 上要么不存在、要么取值不被接受（取值错误会让 unit **加载失败**，不是忽略）：

| 指令 | 最低 systemd | 219 上的后果 |
| --- | --- | --- |
| `EnvironmentFile=` | 205 | 可用 |
| `PrivateTmp=` | 209 | 可用 |
| `ProtectHome=` | 214 | 可用（`tmpfs` 取值需 232，不得使用） |
| `NoNewPrivileges=` | 159 | 可用 |
| `Restart=` / `RestartSec=` | 186 起 | 可用 |
| `ProtectSystem=strict` | 232 | **取值不被接受，unit 加载失败**；219 只能用 `yes`/`full` |
| `ReadWritePaths=` | 231 | 旧档须退回 `ReadWriteDirectories=`，否则**指令被忽略**（与 `ProtectSystem=yes` 组合会导致应用写不了自己的工作目录） |
| `StandardOutput=append:` | 240 | **取值不被接受**；旧档只能用 `file:`（每次启动截断）或 `journal` |
| `LoadCredential=` | 247 | **指令未知**；见第 15 节的凭据收敛方式 |

因此适配器必须：

1. 运行时探测版本（`systemctl --version`），据此选择 unit 档位；
2. 至少维护两档——`strict`（≥ 240）与 `legacy`（219～239）；
3. 把探测到的版本与所选档位写进 unit 的注释与审计，避免「同一份 manifest 在不同主机上
   生成的 unit 不同」却无迹可查。

需要显式记录的语义损失（旧档无法等价实现，只能降级）：

- `logs.directory` 的**追加**语义。`append:` 只在新档可用；旧档用 `file:` 会在每次启动时
  截断日志，与 1b「按 Operation 查日志」的预期不一致。备选是旧档统一改用 `journal`，
  但这要求 `opsd` 从 journald 读日志，而当前设计是直接读文件。
- 凭据的隔离级别（见第 15 节）。

### ExecStart 转义规则（必须按 systemd 的语义，不能自己发明）

- 参数按空白切分，除非整体用双引号包裹。
- 空参数写作 `""`。
- 含空白、`"`、`'`、`\` 时，整体用双引号包裹，并在双引号内把 `"` 与 `\` 前加反斜杠。
- 除此之外原样输出。
- 该函数必须有覆盖这些情况的单元测试——转义错误会直接变成服务起不来或参数错乱。

`strict` 档中 `ProtectSystem=strict` 配合显式 `ReadWritePaths` 是有意的硬约束：unit 只能写
工作目录、日志目录和解包目录，写不进去就说明规格里漏声明了路径，属于需要暴露的错误；
`legacy` 档做不到同等强度（见上表），降级结论必须写进审计。

## 7. 用户、目录、环境文件与凭据

- **用户**：`useradd --system --no-create-home --home-dir <workingDirectory> --shell /usr/sbin/nologin <runUser>`。
  已存在时只校验，不改已有用户的属性。
- **目录**：`workingDirectory`、`logs.directory`、解包目录全部 `0750`，属主为 `runUser:runUser`。
- **环境文件**：`/etc/opsd/apps/<application>.env`，`0600`，只写 `exec.environment` 里的**非敏感**值。
- **凭据文件**：`/etc/opsd/apps/<application>.credentials/<name>`，`0700` 目录、`0600` 文件，
  属主为 `runUser`，内容是 `SecretRef` 解析出的值。**如何交给进程由 systemd 版本决定**：

  - `strict` 档（≥ 247）：`LoadCredential=<name>:<path>`，进程内读 `$CREDENTIALS_DIRECTORY/<name>`；
  - `legacy` 档（219～246）：没有 `LoadCredential=`，只能走 `EnvironmentFile=` 或
    `Environment=`，此时凭据以**普通环境变量**出现在进程环境里。

  这个分档意味着「同一份应用在不同主机上读凭据的方式不同」，因此第 15 节把
  「是否统一走 `EnvironmentFile=`」列为进入实现前必须收敛的取舍。

**已确认的设计取舍（2026-09-22）**：为了让托管在 systemd 下的进程拿到凭据，明文必须以某种
形式到达进程，**已允许写到磁盘**。这里选择写入 `0600` 凭据文件而非写进 unit 的
`Environment=`——unit 的权限更难管、更容易被 `systemctl show` 或日志带出。它不违反 1a 的
「不落库、不进日志、不进审计」，但**确实把明文落到了磁盘**，因此：

- 凭据目录必须 `0700`，文件 `0600`，属主为运行用户；
- `opsd` 自身与日志、审计、错误信息中仍按值脱敏；
- 必须在 Linux 容器中验证权限强制效果，并在第 15 节标注真实主机未验证。

## 8. 持久化（migration `0004`）

```sql
CREATE TABLE application_specs (
    application_id TEXT PRIMARY KEY REFERENCES applications (id),
    spec_json      TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    updated_by     TEXT
);

CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    address     TEXT NOT NULL DEFAULT '',
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE environments (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
```

`application_specs` 以 `application_id` 为主键，即**每个应用一份当前规格**。
迭代 3 若要让规格随 release 版本化，需改为 `(release_id, spec_json)` —— 这属于破坏性变更，
必须提升 `apiVersion` 或新增表，记在第 16 节。

## 9. API 与 CLI

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/artifacts/{id}/content` | **下载制品**（1a 推迟的缺口，1c 落地） |
| `PUT` | `/api/v1/applications/{id}/spec` | 提交 manifest，严格校验 |
| `GET` | `/api/v1/applications/{id}/spec` | 查询当前 manifest |
| `POST` | `/api/v1/applications/{id}/runtime/validate` | 只校验，无副作用 |
| `POST` | `/api/v1/applications/{id}/runtime/prepare` | 创建用户/目录/环境文件/unit |
| `POST` | `/api/v1/applications/{id}/runtime/start` | 启动 |
| `POST` | `/api/v1/applications/{id}/runtime/stop` | 停止 |
| `GET` | `/api/v1/applications/{id}/runtime/health` | 就绪状态 |
| `GET` | `/api/v1/hosts`、`/api/v1/environments` | 列表与创建 |

```text
opsctl artifact download <id-or-digest> --output <path>
opsctl spec put --app <name> --file <manifest.yaml>
opsctl spec show --app <name>
opsctl runtime validate|prepare|start|stop|health --app <name>
opsctl host list
opsctl env list
```

`runtime/start` 与 `stop` 直接走 `Operation`（`kind=runtime.start` / `runtime.stop`），
复用既有的锁、状态机、审计与取消——**不新增旁路**，这样 1a 的 `executor.command`
与 1c 的 `runtime.*` 在查询、日志、取消、重试上完全一致。

## 10. 错误码与退出码增量

| 错误码 | HTTP | CLI 退出码 | 含义 |
| --- | ---: | ---: | --- |
| `SPEC_NOT_FOUND` | 404 | 2 | 应用尚未登记 manifest |
| `MANIFEST_INVALID` | 400 | 17 | manifest 字段或取值非法 |
| `MANIFEST_CONFLICT` | 409 | 18 | unpack 策略与 artifact mediaType 冲突 |
| `RUNTIME_NOT_READY` | 409 | 19 | 健康检查未就绪 |
| `RUNTIME_UNSUPPORTED` | 409 | 21 | 当前适配器不支持该操作 |

退出码 `17`/`18`/`19`/`21` 与迭代 0、1a 已占用的 `1~14`、`20` 不冲突。

## 11. 兼容性与迁移影响

- migration `0004` 只 `CREATE TABLE`，不动既有表。
- 新增端点；既有端点不变。
- Operation 状态机不变；`runtime.*` 只是新增 `kind`，复用同一套状态与恢复语义。
- 未提交 manifest 的应用，1c 的 `runtime/*` 端点返回 `SPEC_NOT_FOUND`，迭代 0/1a 行为不变。

## 12. 测试计划

### 单元

- manifest 严格解码与每条校验规则。
- `ExecStart` 转义规则的边界用例（空参数、引号、反斜杠、中括号、等号）。
- unit 模板渲染：字段齐全、`ReadWritePaths` 包含全部声明路径。
- 环境文件与凭据文件内容生成、按值脱敏不写入日志。
- `mediaType` → 解包策略映射与 `MANIFEST_CONFLICT`。

### 合约测试

- `runtimecontract` 在 `proc` 适配器上全绿（macOS / CI）。
- 同一套件在 systemd 适配器上于 Linux 容器内全绿。

### Linux 容器（扩展 `make verify-linux`）

- systemd 适配器 `Prepare` 后：用户存在、目录 `0750`、环境文件 `0600`、
  凭据目录 `0700`/文件 `0600`、unit 文件 `0644`。
- `Start` → `Status=active` → `Health.Ready` → `Stop` → `Status=inactive`。
- 凭据通过 `LoadCredential` 可被服务进程读到（用一个打印 `$CREDENTIALS_DIRECTORY` 内容
  是否存在的探针 unit 验证，不打印内容本身）。
- `ProtectSystem=strict` 下进程写入未声明路径必须失败。

### 回归

- 迭代 0 与 1a 的全部测试与验收标准继续通过。

## 13. 验收标准

1. manifest 非法在提交时即失败，错误码 `MANIFEST_INVALID`，且无任何副作用。
2. `Prepare` 可重复执行，结果一致（幂等）。
3. `Start` → `Health.Ready` → `Stop` → `Status=inactive` 全链路通过。
4. 用户、目录、环境文件、凭据、unit 的文件与属组全部符合第 6、7 节，
   证据类型为 **Linux 容器**。
5. `ExecStart` 转义在边界用例下正确。
6. 制品下载端点可用，且下载内容与记录的摘要一致。
7. `runtime.*` 操作可在 `opsctl operation get/logs/cancel/retry` 中被查询与操作。
8. `make ci` 与 `make verify-linux` 全绿。
9. 迭代 0、1a 的回归用例全部继续通过。

## 14. 未验证内容

- 真实 Linux 主机上的 **reboot 后 unit 持久化**、`WantedBy=multi-user.target` 实际生效。
- **SELinux/AppArmor** 对 `ProtectSystem=strict` 与路径写入的实际约束。
- **sudoers/PAM** 实际策略与 `useradd --system` 在目标发行版上的细节差异。
- `tzdata` 缺失对时区解析的影响（与 1b 共用，容器镜像需显式安装并断言）。
- **老发行版（systemd 219～246）上的实际行为**：已承诺支持 systemd ≥ 219，但现有
  `test/linux/` harness 用的是 Ubuntu 24.04（systemd 255），**证明不了 219 上的任何事**。
  要给出该范围的证据，需要第二个 harness 镜像（CentOS 7，systemd 219；注意 CentOS 7
  已于 2024-06 停止维护）、或一台 Linux 主机。在此之前，「支持 systemd ≥ 219」
  必须标注为**未验证**，且 unit 模板的降级路径只有静态检查支撑。
- 远程主机、mTLS、批量（迭代 5）。

## 15. 未决事项（进入实现前必须冻结）

1. **凭据是否允许落盘（已决定，2026-09-22）**：**允许**。采用第 7 节的方案：凭据以
   `0600` 文件形式落到 `/etc/opsd/apps/<application>.credentials/<name>`，属主为运行用户。
   靠目录 `0700`、文件 `0600`、属主约束，以及 `opsd` 自身在日志/审计/错误信息中按值脱敏
   来控制风险；权限强制效果在 Linux 容器中验证，真实主机标注未验证。
2. **调度器与 systemd timer 的关系（已决定，2026-09-22）**：`opsd` 的进程内调度循环是唯一
   权威，1c 不生成 `.timer`。若将来需要 systemd 集成，增加显式的「导出计划为 timer」命令，
   不做双向自动同步。详见 1b 第 13 节第 2 条。
3. `application_specs` 是否在迭代 3 改为按 release 版本化（第 8 节说明了影响）。
4. 制品解包的目标目录布局与权限（本文件只声明了 `stripComponents` 与目录，未定义目录结构）。
5. `Host`/`Environment` 是否需要与应用建立关联（当前刻意不关联）。
6. **目标 systemd 版本范围（已决定，2026-09-22）**：支持 **systemd ≥ 219**
   （覆盖 CentOS 7 / Ubuntu 18.04 起的发行版）。这带来三项必须落实的兼容工作，见第 4 节。

### 待确认：决定 1 与决定 6 之间存在冲突

`LoadCredential=` 需要 **systemd ≥ 247**，而 219 不支持。因此「用 `LoadCredential=` 交付凭据」
无法覆盖已承诺的版本范围，必须二选一收敛（**需要用户拍板**）：

- **收敛方式一：统一走 `EnvironmentFile=`**。所有发行版都用 `0600` 环境文件，
  由 systemd 在启动时读入进程环境。一套路径、一个应用契约（应用只认环境变量）。
  代价：放弃 `LoadCredential` 的额外隔离，凭据以环境变量形式出现在
  `/proc/<pid>/environ`（仅同用户与 root 可读）。
- **收敛方式二：双路径 + 应用自行兼容**。≥ 247 用 `LoadCredential=`（应用读
  `$CREDENTIALS_DIRECTORY/<NAME>`），< 247 用 `EnvironmentFile=`（应用读 `$<NAME>`）。
  代价：**同一份应用二进制在不同主机上的读取方式不同**，应用必须同时处理两种，
  且这种差异只在老发行版上才暴露出来。

倾向收敛方式一：本迭代已经为了版本范围放弃了 `LoadCredential`，再让应用契约随宿主机变化，
是把复杂度转移到了最难排查的地方（应用侧、且只在旧主机上出现）。
