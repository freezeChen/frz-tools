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
    DB_PASSWORD:                # kind: env → 环境变量里就是凭据值，必须是单行
      kind: env
      name: billing_db_password
    TLS_KEY:                    # kind: file → 多行内容落到 0600 文件，变量里是该文件路径
      kind: file
      name: /etc/opsd/secrets/billing-tls.key
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
  `kind` 决定交付方式（见第 6 节）：`env` 的值必须单行，`file` 的路径由 opsd 生成、
  环境变量里传该路径。
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
EnvironmentFile=/etc/opsd/apps/<application>.secrets.env
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

两个 `EnvironmentFile` 的 `-` 前缀是**有意区别对待**的：非敏感环境文件缺失可以继续（应用
可能不需要额外环境变量），**敏感环境文件缺失必须让 unit 启动失败**，否则应用会在缺凭据的
状态下起来。已决定不用 `LoadCredential=`，全部走环境文件（理由见第 15 节）。

### 环境文件的值转义（已实测，不能自己发明）

systemd 会处理环境文件里的反斜杠转义。实测（systemd 255）确认：**双引号内的 `\\` 收敛为一个
`\`，`\"` 收敛为 `"`，其余 `\x` 原样保留；`$` 不做展开；单引号不特殊。** 因此 `opsd` 写值时必须：

> 整体用双引号包裹，把 `\` 替换为 `\\`、`"` 替换为 `\"`，其余字节原样输出。

不这样做，含反斜杠或双引号的凭据会被**静默改写**——密码里的 `\` 会消失，排查成本极高。
该函数必须有逐字节往返的单元测试（见第 12 节）。

### 环境文件装不下换行（已实测，决定了凭据交付方式）

实测：`ESCAPED="line1\nline2"` 得到的值是**字面量 `\n`**（反斜杠加 n），不是换行；
用行尾反斜杠续行则会把换行**直接吃掉**（`first\` + 换行 + `second` → `firstsecond`）。
**结论：`EnvironmentFile` 无法承载含真实换行的值。**

因此 `exec.secretEnvironment` 按 `SecretRef.kind` 分流，语义在 manifest 里显式声明、不隐藏：

| kind | 交付方式 | 环境变量里的内容 |
| --- | --- | --- |
| `env` | 写入 `<application>.secrets.env` | 凭据值本身（**必须是单行**） |
| `file` | 内容落地为 `/etc/opsd/apps/<application>.secrets/<name>`（`0600`，属主 `runUser`） | **该文件的路径** |

- `kind: env` 的值若含换行，在服务启动时以 `SECRET_UNRESOLVED` 失败，错误信息指向具体的
  manifest 字段。多行凭据（PEM 私钥、JSON 服务账号）必须声明为 `kind: file`。
- `kind: file` 传路径而不是内容，是与 1a 执行器的一处**有意差异**：执行器跑短命令，内容进
  环境变量没问题；长驻服务的凭据可能多行，只能走文件。manifest 里已经写了 `kind`，
  作者在声明时就知道拿到的会是路径。

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
| `ProtectSystem=strict` 之外的加固指令 | — | 见上，`legacy` 档须整体降级 |

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
- **环境文件（非敏感）**：`/etc/opsd/apps/<application>.env`，`0600`，属主为 `runUser`，
  只写 `exec.environment` 里的**非敏感**值。
- **环境文件（敏感）**：`/etc/opsd/apps/<application>.secrets.env`，`0600`，属主为 `runUser`，
  写 `kind: env` 的 `secretEnvironment` 项。它与非敏感文件分开，是因为两者的处理规则不同：
  非敏感文件可以安全地展示与比对，敏感文件必须永不落日志、永不进审计、永不回显。
- **凭据目录（`kind: file`）**：`/etc/opsd/apps/<application>.secrets/`，`0700`，属主为 `runUser`；
  内部每个 secret 一个 `0600` 文件，内容为 `SecretRef` 解析出的值；环境变量里传**路径**（见第 6 节）。

**已确认的设计取舍（2026-09-22）**：为了让托管在 systemd 下的进程拿到凭据，明文必须以某种
形式到达进程，**已允许写到磁盘**。选择写 `0600` 文件 + `EnvironmentFile=`，而不是写进 unit 的
`Environment=`——unit 的权限更难管、更容易被 `systemctl show` 或日志带出。它不违反 1a 的
「不落库、不进日志、不进审计」，但**确实把明文落到了磁盘**，因此：

- 敏感环境文件与凭据文件必须 `0600`、凭据目录 `0700`，属主为运行用户；
- `opsd` 自身与日志、审计、错误信息中仍按值脱敏；写文件与渲染 unit 的过程不得打印值；
- 必须在 Linux 容器中验证权限强制效果，并在第 14 节标注真实主机未验证。

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

`SECRET_UNRESOLVED`（1a 已有，400 / 9）在 1c 中新增一种触发场景：`kind: env` 的凭据解析出
含换行的值，无法通过环境文件交付（见第 6 节）。不新增错误码。

## 11. 兼容性与迁移影响

- migration `0004` 只 `CREATE TABLE`，不动既有表。
- 新增端点；既有端点不变。
- Operation 状态机不变；`runtime.*` 只是新增 `kind`，复用同一套状态与恢复语义。
- 未提交 manifest 的应用，1c 的 `runtime/*` 端点返回 `SPEC_NOT_FOUND`，迭代 0/1a 行为不变。

## 12. 测试计划

### 单元

- manifest 严格解码与每条校验规则。
- `ExecStart` 转义规则的边界用例（空参数、引号、反斜杠、中括号、等号）。
- **环境文件值转义**：逐字节往返——含 `\`、`"`、字面 `\n`、制表符、`$`、单引号、首尾空格的值
  必须原样还原。并有一个反向断言：不转义时 systemd 会改写的那些字节，恰好是被转义的那些。
- **换行拒绝**：`kind: env` 的 secret 解析出含换行的值时，必须报 `SECRET_UNRESOLVED` 且
  错误信息指向具体 manifest 字段；`kind: file` 的多行值必须能正常落盘。
- unit 模板渲染：两个档位各渲染一次；`strict` 档字段齐全、`ReadWritePaths` 包含全部声明路径；
  `legacy` 档**不含** `ProtectSystem=strict`、`ReadWritePaths=`、`StandardOutput=append:`，
  且含 `ReadWriteDirectories=`。
- 环境文件与凭据文件内容生成、按值脱敏不写入日志。
- `mediaType` → 解包策略映射与 `MANIFEST_CONFLICT`。

### 合约测试

- `runtimecontract` 在 `proc` 适配器上全绿（macOS / CI）。
- 同一套件在 systemd 适配器上于 Linux 容器内全绿。

### Linux 容器（扩展 `make verify-linux`）

- systemd 适配器 `Prepare` 后：用户存在、目录 `0750`、非敏感环境文件 `0600`、
  敏感环境文件 `0600`、凭据目录 `0700`/文件 `0600`、unit 文件 `0644`。
- `Start` → `Status=active` → `Health.Ready` → `Stop` → `Status=inactive`。
- 凭据确实到达进程：用一个只打印「变量是否存在」与「长度是否为预期值」的探针 unit 验证，
  **不打印值本身**；并验证含反斜杠与双引号的值被原样收到（这是转义正确性的端到端证据）。
- `kind: file` 的凭据文件可被运行用户读到、且内容与源一致。
- `ProtectSystem=strict` 下进程写入未声明路径必须失败。
- **敏感环境文件缺失时 unit 必须启动失败**（对应 `EnvironmentFile` 不加 `-` 的设计）。

### 回归

- 迭代 0 与 1a 的全部测试与验收标准继续通过。

## 13. 验收标准

1. manifest 非法在提交时即失败，错误码 `MANIFEST_INVALID`，且无任何副作用。
2. `Prepare` 可重复执行，结果一致（幂等）。
3. `Start` → `Health.Ready` → `Stop` → `Status=inactive` 全链路通过。
4. 用户、目录、两个环境文件、凭据、unit 的文件与属组全部符合第 6、7 节，
   证据类型为 **Linux 容器**。
5. `ExecStart` 转义在边界用例下正确。
6. 环境文件的值转义逐字节往返正确：含 `\`、`"`、字面 `\n`、制表符、`$`、单引号、首尾空格的
   凭据，服务进程收到的字节与源完全一致（**Linux 容器**端到端验证，不是只有单测）。
7. `kind: env` 的多行凭据被拒绝且错误信息指向具体字段；`kind: file` 的多行凭据可正常交付。
8. 两个 unit 档位各自渲染正确；`legacy` 档在拿到老版本主机验证之前标注**未验证**。
9. 制品下载端点可用，且下载内容与记录的摘要一致。
10. `runtime.*` 操作可在 `opsctl operation get/logs/cancel/retry` 中被查询与操作。
11. `make ci` 与 `make verify-linux` 全绿。
12. 迭代 0、1a、1b 的回归用例全部继续通过。

## 14. 未验证内容

- 真实 Linux 主机上的 **reboot 后 unit 持久化**、`WantedBy=multi-user.target` 实际生效。
- **SELinux/AppArmor** 对 `ProtectSystem=strict` 与路径写入的实际约束。
- **sudoers/PAM** 实际策略与 `useradd --system` 在目标发行版上的细节差异。
- `tzdata` 缺失对时区解析的影响（与 1b 共用，容器镜像需显式安装并断言）。
- **老发行版（systemd 219～246）上的实际行为**：已承诺支持 systemd ≥ 219，但现有
  `test/linux/` harness 用的是 Ubuntu 24.04（systemd 255），**证明不了 219 上的任何事**。
  容器路已验证走不通：CentOS 7 镜像只有 amd64 清单，且 systemd 219 需要 cgroup v1，
  而当前 Docker Desktop 是 cgroup v2。用户曾提供一台主机，经探测为 Rocky Linux 10.2 /
  systemd 257 的生产机，已排除（详见第 15 节）。**`legacy` 档的验证主机尚未落实**；
  在拿到之前，该档必须标注为**未验证**，其降级路径只有单元测试与静态检查支撑。
- 远程主机、mTLS、批量（迭代 5）。

## 15. 未决事项（进入实现前必须冻结）

1. **凭据是否允许落盘（已决定，2026-09-22）**：**允许**。凭据以 `0600` 文件形式落到
   `/etc/opsd/apps/<application>.secrets.env`（`kind: env`）与
   `/etc/opsd/apps/<application>.secrets/`（`kind: file`，目录 `0700`），属主为运行用户。
   靠权限与属主约束，以及 `opsd` 自身在日志/审计/错误信息中按值脱敏来控制风险；
   权限强制效果在 Linux 容器中验证，真实主机标注未验证。
2. **调度器与 systemd timer 的关系（已决定，2026-09-22）**：`opsd` 的进程内调度循环是唯一
   权威，1c 不生成 `.timer`。若将来需要 systemd 集成，增加显式的「导出计划为 timer」命令，
   不做双向自动同步。详见 1b 第 13 节第 2 条。
3. `application_specs` 是否在迭代 3 改为按 release 版本化（第 8 节说明了影响）。
4. 制品解包的目标目录布局与权限（本文件只声明了 `stripComponents` 与目录，未定义目录结构）。
5. `Host`/`Environment` 是否需要与应用建立关联（当前刻意不关联）。
6. **目标 systemd 版本范围（已决定，2026-09-22）**：支持 **systemd ≥ 219**
   （覆盖 CentOS 7 / Ubuntu 18.04 起的发行版）。这带来必须落实的兼容工作，见第 6 节。

### 已收敛：决定 1 与决定 6 的冲突（2026-09-22）

`LoadCredential=` 需要 **systemd ≥ 247**，与「支持 ≥ 219」不相容。**已决定采用收敛方式一：
所有版本统一走 `EnvironmentFile=`，完全不使用 `LoadCredential=`。**

- 一套 unit 模板（除加固指令档位外）、一个应用契约：应用只认环境变量。
- 代价：凭据以环境变量形式出现，同用户与 root 可读 `/proc/<pid>/environ`。可接受——
  root 本来就能读 `0600` 凭据文件，而绕过 opsd 直接读进程内存不在本工具的威胁模型内。
- 收益：不必让应用契约随宿主机变化。方式二（≥ 247 走 `LoadCredential`、旧版走环境变量）
  会把复杂度推到应用侧，且只在旧主机上暴露，是本迭代最难排查的一种故障。
- 由此确定：凭据不必再分成「每个 secret 一个文件」，而是收敛为
  `/etc/opsd/apps/<application>.secrets.env`（`0600`）与 `kind: file` 的落盘目录
  `/etc/opsd/apps/<application>.secrets/`（`0700`）。见第 6 节与第 7 节。

### 老发行版的验证安排（2026-09-22）

`legacy` 档（systemd 219～239）在当前环境无法验证，原因已查明且不可回避：

- CentOS 7 的镜像**只有 amd64 清单**，Apple Silicon 上只能靠 qemu 模拟，容器内的 systemd 不稳；
- 更关键的是 **systemd 219 需要 cgroup v1**（对 cgroup v2 统一层级的支持到 226 才开始、
  233 才实用），而当前的 Docker Desktop 用的是 cgroup v2，systemd 219 起不来。

**用户曾提供一台主机（`root@192.168.11.101`）用于验证，经只读探测后排除，不能用于本用途：**

- 该机是 **Rocky Linux 10.2 / systemd 257**（`systemctl`、`/usr/lib/systemd/systemd`、`rpm`
  三种方式一致确认），**比容器里的 255 还新**，属于 `strict` 档，证明不了 `legacy` 档；
- 该机是**生产环境**，上面跑着 MES、MySQL、Redis、TDengine、EMQX、OpenResty 等实际业务
  （由 1Panel 管理），用户明确要求只能隔离部署，不碰 `/etc`、不建系统用户、不写 systemd unit；
- 该机 cgroup 为 v2，systemd 219 在其上无法启动。
- 探测阶段为**只读操作**，未对该机做任何改动。

因此 `legacy` 档的验证主机**目前仍未落实**。安排为：

1. 1c 先实现两档 unit 模板，`legacy` 档的单元测试与静态检查必须齐全；
2. `legacy` 档在拿到主机前，一律标注为**未验证**，不得声称「已支持」；
3. 拿到主机后，用第 12 节列出的 Linux 容器断言清单在本机重跑，证据类型记作
   **Linux 主机**（不是 Linux 容器），并单独记录 systemd 版本与发行版。
