# Linux 迭代 1c：Linux 适配（RuntimeAdapter 与 systemd）

> 文档日期：2026-09-22（2026-09-23 两次回填实现与验证记录）
> 文档状态：**实现完成、验证进行中**——A2–A7 已在工作树落地（HEAD 仍为 `747aee9`，改动未提交），
> 并已在 **Linux 容器**中实跑通过（`make verify-linux` 第 5 轮 **89 通过 / 0 失败**）；
> 仍缺 `legacy` 档与真实 Linux 主机的证据，见第 16 节末尾与第 17 节
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
- sudo 白名单（`sudo.allowedCommands`）：配置模型已在迭代 0 冻结，但 1c **未实现任何行为**
  ——读取该字段的代码不存在。在真实 Linux 主机上验证 sudoers 之前，不得当作已生效的
  安全控制（迭代 0 遗留项，见 `2026-09-21-iteration-0.md` 第 395 行）。
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

> **实现偏差（2026-09-23 补注）**：上面 yaml 里的 `type: tcp  # tcp | http | exec` 是冻结时的
> 设想，**实现只支持 `tcp` 与 `http`**；`exec` 未实现，理由与影响见第 6 节末尾
> 「实现比规格更严的三处」第 1 条。

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

### 实现比规格更严的三处（2026-09-23 补注）

以下三处是**实现刻意比本节规格更严格**，不是遗漏；理由都写在代码注释里，并有测试钉住：

1. **`readiness.type` 只有 `tcp` 与 `http`**：本节 yaml 里的 `tcp | http | exec` 是冻结时的设想，
   `exec` 的 `target` 与参数语义从未定义，凭空实现一套未冻结的语义比明确不支持更危险
   （`internal/domain/appspec.go` 第 39–44 行的注释；实际取值只有 `ReadinessTCP`/`ReadinessHTTP`，
   测试钉住了对 `exec` 的拒绝）。**需要 `exec` 时必须先补一份冻结决定再实现**，
   不能按本节现在的文字当作已支持。
2. **渲染层拒绝含换行、空白、引号或反斜杠的路径**（`internal/adapters/runtime/unitfile/unit.go`
   第 253 行的 `checkRenderablePath`）：manifest 只校验「必须是绝对路径」，而这些字节写进 unit
   会被 systemd 重新解释——换行会直接变成一条新指令（等于把 unit 配置的书写权交给 manifest
   作者）、空白会切开 `ReadWritePaths=` 的路径列表、引号与反斜杠会按 C 转义把路径悄悄改写。
   这些指令的反斜杠/引号规则没有在真实主机上验证过，因此宁可显式拒绝。
3. **健康检查超时必须是整秒**（同文件第 271 行的 `wholeSeconds`）：`health.startTimeoutSeconds`
   与 `stopTimeoutSeconds` 出现亚秒值时直接报错，而不是截断——渲染层做截断会让「manifest 里写的」
   与「unit 里生效的」不再一致，而 unit 里出现的数字事后无从解释。

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

### `/etc/opsd` 的权限例外：过路目录补 `0751`（2026-09-23 补注）

本节上面的目录/文件模式描述与迭代 0 §9 的安装约定（`/etc/opsd` 建成 `0750`、属主是 `opsd`）
对 `kind: file` 的凭据**不成立**，必须显式记下这个例外：

- **为什么必须**：`kind: file` 的凭据是**应用以 `runUser` 身份按路径自己去打开的**（环境变量里
  传的是路径）。`0750` 且属主是 `opsd` 时，`runUser` 既不是属主也不在主组，命中的是 other 位
  `---`，**没有 `+x` 就穿不过目录，应用打不开自己的凭据文件**——而这一点在开发机上不可见
  （本地跑测试时「runUser」就是测试进程本身，走的是属主分支）。`kind: env` 的凭据由 systemd
  以 root 读环境文件，不受影响。
- **只加穿越位，不放开读位**：`Prepare` 对**属于 `opsd` 自己**的过路层级
  （`/etc/opsd`、`/etc/opsd/apps`）执行 `mode | 0111`，即 `0750 → 0751`（更严格时
  `0700 → 0711`），**只补 `+x`**——`opsd` 自己的 `config.yaml`（`0600`）与 `secrets/`（`0700`）
  的保护不变，other 依然不能读与列目录；不属于我们的层级（例如 root 拥有的 `/etc`）**不修改**。
- **不属于我们的层级改为校验并快速失败**：整条链（从 `/etc` 到凭据目录）逐级校验 `runUser`
  能否穿越，不能就让 `Prepare` 以 `PERMISSION_DENIED` 失败，并给出确切的修复命令
  （`chmod ...`，只放开穿越位）。宁可 `Prepare` 失败，也不要让应用带着一个打不开的凭据路径启动。
- **作用域尽量小**：没有 `kind: file` 凭据时，`Prepare` 不改任何共享目录的权限位。

落点：`internal/adapters/runtime/systemd/files.go` 第 186 行 `ensureCredentialReachable`、
第 196 行的「归我们管理」集合、第 236 行 `credentialPathLevels`、第 258 行 `canChmod`、
第 265 行 `traversable`、第 110 行 `ensureAncestors`（新目录按 `0751` 建并显式 `Chmod`，
不受 umask 影响）。

**「归我们管理」只有三级，且 `canChmod` 必须认 root**（这一条是 Linux 容器实跑逼出来的）：

- 可收敛的目录**只有三级**：凭据目录 `/etc/opsd/apps/<app>.secrets`、`/etc/opsd/apps`、
  `/etc/opsd`（`files.go` 第 196–200 行的 `managed` 映射）。**更高层（`/`、`/etc`）只校验、
  绝不修改**——一次 `Prepare` 顺手把系统目录权限改宽，属于「本地没人发现、真机上被安全扫描
  发现」的那类改动。
- `canChmod(euid, dirUID)`（第 258 行）判定的是**本进程能不能收敛这个目录**：
  `euid == 0 || dirUID == euid`。**root 这一支是必需的**——适配器要 `useradd`/`chown`/`systemctl`，
  生产上必须以 root 运行，而安装约定把 `/etc/opsd` 的属主给了 opsd 的**服务用户**
  （`test/linux/verify.sh` 里是 `frz-ops`），于是「目录属主 == 自己的 euid」这一条会把
  root 本来修得好的目录判成不可修，让 `Prepare` 在**完全正常的部署**上失败。
  第一版实现漏了这一支，由 `make verify-linux` 的 `check_runtime` 实跑发现（详见第 17 节）。
- 单元测试：`TestCanChmod`（`credentialpath_internal_test.go` 第 43 行）用表驱动钉住
  「root 可以、属主可以、其他人不可以」；`TestTraversable`（第 14 行）、
  `TestCredentialPathLevels`（第 65 行）覆盖判定与路径分层。
- 容器断言：`check_runtime` 里 `runuser -u <runUser> -- cat` 成功、`frz-other` 被拒，
  以及 `Prepare` 后 `/etc/opsd` 为 `0751`（other 有 `+x` 无 `+r`）——**已实跑通过**。
- 同进程内的单元测试（跑在假 runner / 临时根前缀上）：`TestCredentialPathIsReachableByRunUser`
  （`systemd_test.go` 第 740 行）、`TestPrepareRepairsInstallerOwnedEtcOpsd`（第 780 行，
  断言 `0750 → 0751`、`0700 → 0711` 且 other 不得有读位）、
  `TestPrepareWithoutFileSecretsLeavesSharedDirsAlone`（第 811 行）。

**时序关系（避免误读为自相矛盾）**：`check_filesystem` 第 247 行的「`/etc/opsd` 模式 = `750`」
发生在 `Prepare` **之前**（那是安装脚本留下的状态，断言保留不变），`check_runtime` 的 `751`
发生在 `Prepare` **之后**——安装时 `0750`、首次 `Prepare` 后 `0751`，两者都成立。

**仍未验证**：以上都是 **Linux 容器**（Ubuntu 24.04 / systemd 255 / arm64）里的结论；
真实 Linux 主机上的 umask、挂载选项差异与 SELinux/AppArmor 约束仍未验证（见第 14 节）。

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
必须提升 `apiVersion` 或新增表，决定见第 15 节第 3 条。

> **落地情况（2026-09-23）**：本节的三张表已由 `migrations/0004_hosts_specs.sql` 落地，
> 内容与本节的 SQL 一致（只 `CREATE TABLE`，且文件头注释重申了「刻意不加永远为 NULL 的
> `release_id`」）；仓储在 `internal/adapters/sqlite/host.go` 与 `spec.go`，
> 本机 Host 自举在 `internal/application/host.go`。见第 16 节的 A2。

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

> **落地情况（2026-09-23）**：本节的端点与 CLI 全部落地（见第 16 节 A6a/A6b）。
> 两处如实记录：CLI 除这里列的 `host list`、`env list` 之外，还实现了
> `host create|inspect` 与 `env create|inspect`（API 本就有 `POST` 与详情端点，属自然补全）；
> `/runtime/health` 未就绪时返回 `200` + `ready:false`（供轮询），只有确定性失败才 `409`。

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

> **落地现状（2026-09-23）**：`proc` 与 `systemd` **都已经**在本地跑这套合约（systemd 那一份跑在
> 「假 `systemctl` + 根前缀」上，`internal/adapters/runtime/systemd/contract_test.go`）。
> 容器内更进了一步：`check_runtime` 用**真实 systemd** 走通了适配器的端到端路径
> （`runtime prepare/start/health/stop` + `systemctl is-active`），这是比合约套件更强的证据；
> 但**「Go 合约套件本身在容器里被执行过」这一点没有核实**，所以本小节第二条不完全按
> 「合约套件在容器内全绿」记，而是记作「容器内真实 systemd 上的适配器行为已通过」。

### Linux 容器（扩展 `make verify-linux`）

- systemd 适配器 `Prepare` 后：用户存在、目录 `0750`、非敏感环境文件 `0600`、
  敏感环境文件 `0600`、凭据目录 `0700`/文件 `0600`、unit 文件 `0644`。
- `Start` → `Status=active` → `Health.Ready` → `Stop` → `Status=inactive`。
- 凭据确实到达进程：用一个只打印「变量是否存在」与「长度是否为预期值」的探针 unit 验证，
  **不打印值本身**；并验证含反斜杠与双引号的值被原样收到（这是转义正确性的端到端证据）。
- `kind: file` 的凭据文件可被运行用户读到、且内容与源一致。
- `ProtectSystem=strict` 下进程写入未声明路径必须失败。
- **敏感环境文件缺失时 unit 必须启动失败**（对应 `EnvironmentFile` 不加 `-` 的设计）。

> **落地现状（2026-09-23）**：以上断言**已全部写进** `test/linux/verify.sh` 的 `check_runtime`，
> 并且**已在容器中实跑通过**：第 5 轮 `make verify-linux` 退出码 0，**89 项通过 / 0 项失败**
> （其中 `check_runtime` 49 项；证据类型 **Linux 容器**：Ubuntu 24.04 / systemd 255 / arm64）。
> 前 4 轮暴露了 3 处问题（第 1 轮 25 FAIL）——其中一处是**产品缺陷**（`canChmod` 不认 root），
> 完整过程见第 17 节 A7。
>
> `check_runtime` 打在一个**以 root 运行**的第二个 `opsd` 实例上（配置
> `test/linux/opsd.root.verify.yaml`；适配器要 `useradd`/`chown`/`systemctl`，必须 root），
> 其余各组的断言仍打在原来以 `frz-ops` 运行的实例上、前缀未动。
> **另有一条语义修正**：`Restart=on-failure` 下 unit 失败后 `ActiveState` 只是瞬时 `failed`，
> 等待自动重启期间是 `activating`（`SubState=auto-restart`），所以「缺凭据必须启动失败」被
> 断言成「12×0.5s 窗口内从未 `active`，且观察到的状态只能是 `activating`/`failed`」——
> **不要把「停止」与「失败」混为一谈**（`verify.sh` 第 649–678 行）。

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

### 逐条判定（2026-09-23）

证据类型严格区分：**静态 / 单元 / 集成 / e2e / Linux 容器 / Linux 主机**。

| # | 判定 | 证据与类型 |
| --- | --- | --- |
| 1 | **达成** | 单元：`internal/adapters/manifest/manifest_test.go:250,271`（断言错误码 `MANIFEST_INVALID`）、`internal/adapters/httpapi/spec_test.go`；集成：同名端点用例；e2e：`TestSpecPutRejectsInvalidManifestThroughCLI`（退出码 17） |
| 2 | **达成** | 单元：`TestPrepareCommandSequenceIsIdempotent`:137、`TestPrepareDoesNotRewriteUnchangedFiles`:707（mtime 不变）；**Linux 容器**：`check_runtime` 的「第二次 Prepare 幂等且档位不变」（`verify.sh` 第 569–574 行，断言在第 574 行） |
| 3 | **达成** | **Linux 容器**：`check_runtime` 走完 `runtime start`（Operation 终态 `succeeded`）→ `runtime health` 就绪 → `systemctl is-active` = `active` → `runtime stop` 终态 `succeeded` → 停止后 `inactive`；本地侧另有 `proc` 的合约断言（单元） |
| 4 | **达成** | **Linux 容器**：`check_runtime` 断言用户存在、目录 `0750` 属主 `runUser`、两个环境文件 `0600`、凭据目录 `0700`、凭据文件 `0600` 且内容与源逐字节一致、unit `0644`、`/etc/opsd` 补 `0751` 后可跨用户读 |
| 5 | **达成** | 单元：`escape_test.go`（`TestEscapeArg`/`TestEscapeArgs`/`TestEscapeEnvValue*`）+ `unit_test.go` 的 `TestRenderUnitEscapesExecStartArguments`:213（含引号、反斜杠、空参数、含空格的参数） |
| 6 | **达成** | **Linux 容器**：`check_runtime` 用探针应用比对两类凭据的**长度与 sha256**（值含 `\`、`"`、字面 `\n`、`$`、单引号与首尾空格），与源逐字节一致；单元侧有 `escape_test.go` 的逐字节往返 |
| 7 | **达成** | 单元：`TestPrepareRejectsMultilineEnvSecret`:254（`SECRET_UNRESOLVED` 且指向字段）、`TestPrepareDeliversMultilineFileSecret`:277（多行文件凭据可交付） |
| 8 | **部分达成** | `strict` 档渲染：单元（`strictUnit` 黄金文本逐字节比对，`:130`）+ **Linux 容器**（`check_runtime` 从 `runtime prepare` 的返回里核对 `tier`/`systemdVersion`，并在 unit 头注释里核对）；`legacy` 档渲染：只有单元（`legacyUnit` 黄金文本，`:153`）——**`legacy` 档的实际加载/运行本次容器是 systemd 255，零 legacy 证据**，按本条要求仍标注**未验证** |
| 9 | **达成** | 集成：`httpapi/artifact_test.go` 的下载端点用例；e2e：`TestArtifactDownloadThroughCLI`（按 ID 与摘要各下载一次、字节一致、摘要一致，损坏 blob 时退出码 5 且不留目标文件） |
| 10 | **达成** | 单元/集成：`internal/application/runtimeops_test.go`（14 用例，含无旁路复用与取消）；**Linux 容器**：`check_runtime` 里 `runtime start/stop` 都是真实 Operation 且以 `operation get` 轮询到终态 `succeeded`；`test/e2e/runtime_test.go`（本机无适配器时的退出码 21 属回归保护） |
| 11 | **达成** | `make fmt`/`make vet`/`make test`（16 个包）/`make test-race`/交叉编译 exit 0（实现者实跑）；**Linux 容器**：`make verify-linux` 第 5 轮退出码 0、**89 项通过 / 0 项失败** |
| 12 | **达成** | 单元/集成/e2e：16 个包全绿（实现者实跑）；**Linux 容器**：迭代 0 的既有断言语义仍打在原 `frz-ops` 实例上、全部通过 |

**仍未达成/未验证的只剩一处**：第 8 条的 `legacy` 档（无任何容器或真机证据），
以及第 14 节的真实 Linux 主机项（reboot 持久化、SELinux/AppArmor、sudoers/PAM）。

## 14. 未验证内容

> **现状（2026-09-23）**：1c 已经取得 **Linux 容器**证据（`make verify-linux` 第 5 轮 89/0，
> Ubuntu 24.04 / systemd 255 / arm64）。下面各项**仍然未验证**，且**都不能**由容器证据替代；
> 逐条现状见第 16 节末尾与第 17 节。

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
3. **`application_specs` 是否在迭代 3 改为按 release 版本化（已决定，2026-09-22）**：
   语义上应当版本化——否则回滚只回二进制不回配置，语义不完整。但**在迭代 3 做**：
   1c 只建应用级的表，**刻意不加一个永远为 NULL 的 `release_id`**，因为那种列会让人误以为
   已经支持版本化。迭代 3 用 `ALTER TABLE` 迁移，成本很低。
4. **制品解包的目标目录（已决定，2026-09-22）**：`/opt/opsd/apps/<application>/releases/<release-id>/`，
   `0750`，属主 `runUser:runUser`。1c 只固定这个约定；release 目录的切换、保留与清理属于迭代 3。
   工作目录与日志目录仍由 manifest 显式声明，不从该约定推导。
5. **`Host`/`Environment` 是否与应用建立关联（已决定，2026-09-22）**：**1c 保持独立**。
   当前只有本机，建了关联也没有第二台主机可验证；关联留到迭代 5 与远程/多主机一起做迁移。
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

## 16. 实现记录 2026-09-23

本节记录 1c 的两批落地：2026-09-22 已提交的两个提交（下文「已提交」部分），以及
2026-09-23 落地在**工作树、尚未提交**的 A2–A7（下文第二部分）。上一版本节曾把 A2–A6b 写成
「在建工作」快照；它们现已全部落地，本节据此重写并删除那个小节。

> **核对基准**：下文第一部分以已提交状态（HEAD `747aee9`）为基准，第二部分以 **2026-09-23 的
> 工作树**为基准（HEAD 未变，A2–A7 的改动都在工作树里）。每条都给出 `文件:行`，可在仓库里
> 逐条核对；未提交这一点在每处都已标明，避免后来者误以为 HEAD 已有这些代码。
> **代码落地 ≠ 验证通过**：本节把「实现」与「证据」分开写；`legacy` 档与真实 Linux 主机项
> 至今没有证据，见第 16 节末尾与第 17 节。

### 已实现范围（已提交：`3938db4`、`747aee9`）

| 提交 | 标题 | 落地内容 |
| --- | --- | --- |
| `3938db4` | 迭代 1c（一）：manifest 领域模型与 systemd 转义 | 领域模型与应用规格、manifest 严格解码与迁移链、转义函数、1c 错误码 |
| `747aee9` | 迭代 1c（二）：RuntimeAdapter 端口、共享合约测试与 proc 假适配器 | `RuntimeAdapter` 端口、共享合约套件、`proc` 假适配器、转义函数迁到 `unitfile` |

逐项对应：

- **`internal/domain/appspec.go`（418 行）**：`ApplicationSpec` 与 `artifact`/`unpack`/`exec`/
  `health`/`logs`/`systemd` 各段模型、`Validate`（第 128 行）、`applyDefaults`（第 296 行，
  未声明字段填默认值：`unitName` 由应用名派生、`restartPolicy=on-failure`、
  健康检查超时 60s/30s、`consecutiveSuccesses=1`）、`MediaTypeUnpackStrategy`（第 358 行）
  与 `CheckUnpackMediaType`（第 375 行），以及集中一处的路径约定函数 `EnvFilePath` /
  `SecretsEnvFilePath` / `SecretsDir` / `ReleaseDir` / `UnitPath` / `SecretFileVarValue`
  （第 391–414 行，与第 7 节的路径一致）。`kind=env` / `kind=file` 的凭据分流按第 6 节的表实现。
- **`internal/domain/runtime.go`（33 行）**：`RuntimeStatus`（`active`/`inactive`/`failed`/
  `activating`/`deactivating`/`unknown`，与第 3 节一致）、`RuntimeHealth`、`RuntimeInfo`。
- **`internal/domain/host.go`（42 行）**：`Host`、`Environment` 模型（第 5 节的字段形状），
  只有身份与标签，`address` 为空表示本机；无持久化、无自举。
- **`internal/adapters/manifest/manifest.go`（241 行）+ `manifest_test.go`（344 行）**：
  与配置版本化同一套流程——先非严格解码读 `apiVersion` 信封、按迁移链升级、再
  `KnownFields(true)` 严格解码，因此删除/改名/改语义仍需提升版本。
- **`internal/adapters/runtime/unitfile/escape.go` + `escape_test.go`**：`EscapeArg`（第 23 行）、
  `EscapeArgs`（第 43 行）、`EscapeEnvValue`（第 57 行）、`RenderEnvFile`（第 73 行，
  按键排序输出以保证字节稳定，否则每次 `Prepare` 都会重写文件）、`HasNewline`（第 92 行）。
  这些函数在 `3938db4` 中位于 `internal/adapters/runtime/systemd/`，`747aee9` 迁到
  `unitfile/`，由 `proc` 与将来的 `systemd` 共用。
- **`api/v1/errors.go`**：第 32–38 行新增错误码常量、第 61–67 行 HTTP 状态、第 104–110 行
  退出码——`SPEC_NOT_FOUND`（404/2）、`MANIFEST_INVALID`（400/17）、`MANIFEST_CONFLICT`
  （409/18）、`RUNTIME_NOT_READY`（409/19）、`RUNTIME_UNSUPPORTED`（409/21）、
  `HOST_NOT_FOUND`（404/2）、`ENVIRONMENT_NOT_FOUND`（404/2，常量名 `CodeEnvNotFound`），
  与第 10 节一致（第 10 节未列后两个，因为第 5 节当时还没确定要不要建表）。这七个错误码在
  A6a/A6b 之后都有实际消费方，不再只是定义。
- **`internal/application/ports.go` 第 94–111 行**：`RuntimeAdapter` 六方法，签名与第 3 节完全相同；
  端口注释明确「实现方不得假设调用方已经校验过规格，`Prepare`/`Start`/`Health`/`Status`
  内部都必须先校验」。（行号是 A2 之后的当前值；A2 在该文件里新增了仓储方法与
  `RuntimePrepareReporter`，把这段从原来的第 78 行起往下推了。）
- **`internal/application/runtimecontract/contract.go`（279 行）**：共享合约套件，11 项断言
  （第 42、60、71、80、99、118、143、166、181、205、231 行的 `t.Run`）。第 3 节列了 6 项，
  实现多出 `Prepare` 幂等（第 80 行）、`Start`/`Stop` 幂等（第 181/166 行）与就绪目标可达/不可达
  （第 205/231 行）。价值不在「测出 bug」，而在钉住 `proc` 与 `systemd` 的语义一致。
- **`internal/adapters/runtime/proc/proc.go`（当前 399 行）+ `proc_test.go`（216 行）**：假适配器。
  把所有绝对路径映射到沙箱根之下（`SandboxPath`，第 285 行），使第 12 节的合约测试能在无特权
  环境跑通目录、环境文件与权限断言；子进程必须 `Wait` 回收，否则僵尸在 `kill(pid,0)` 下仍算
  「存在」。就绪探测与「连续成功次数」计数在 A4 里被抽成共享包
  `internal/adapters/runtime/readiness`（`Check`、`Tracker`），`proc` 与 `systemd` 共用
  （`proc.go` 第 256 行调用 `readiness.Check`），见下文 A4。
- **提交内修复（测试直接暴露）**：凭据目录权限建成 `0750` 而不是第 7 节要求的 `0700`——
  `MkdirAll` 对已存在目录不改权限，目录先被 `0750` 的循环建出来后按 `0700` 创建即无效，
  同组用户可穿过凭据目录。已改为单独创建并显式 `Chmod` 收敛权限。

### 已实现范围（A2–A7：2026-09-23 工作树，尚未提交）

> HEAD 仍是 `747aee9`；以下文件都在工作树里，**尚未提交**。行号为本次核对时的当前值。

**A2：migration `0004` + 仓储 + 本机 Host 自举**

- `migrations/0004_hosts_specs.sql`：`application_specs`（`application_id` 为主键）、`hosts`、
  `environments`，只 `CREATE TABLE`；文件头注释重申「迭代 3 才版本化、刻意不加永远为 NULL 的
  `release_id`」。
- 仓储：`internal/adapters/sqlite/host.go`（180 行）、`internal/adapters/sqlite/spec.go`（54 行）；
  `internal/adapters/sqlite/migrate_test.go` 覆盖两条迁移路径——
  `TestMigrateApplies0004OnFreshDatabase`（第 98 行，全新库）与
  `TestMigrateUpgradesFrom0003`（第 111 行，从 0003 升级）。
- 用例层：`internal/application/specstore.go`（一个应用一份当前规格，`put` 即覆盖）、
  `internal/application/host.go` 的 `HostService.EnsureLocalHost`（第 40 行）——**先查后建、
  不改动已有记录**（名字与标签可能已被运维调整过，覆盖它们等于每次重启悄悄回滚）；
  若 `local` 这个名字已被一台远程主机占用，降级为 `local-<id>`（第 66–72 行），
  不让守护进程因为一个标签冲突起不来。
- 接线：`cmd/opsd/main.go` 第 138 行在启动时调用 `EnsureLocalHost`，失败即退出。
- 测试：`internal/application/host_test.go`（`TestEnsureLocalHostIsIdempotent`:25、
  `TestEnsureLocalHostKeepsExistingRecord`:54、`TestEnsureLocalHostSurvivesNameCollision`:81）、
  `specstore_test.go`、`test/e2e/host_test.go`（`TestDaemonBootstrapsLocalHost`:13：真实 `opsd`
  启动后 `hosts` 表恰好一行、`name=local`、`address` 为空，**重启后仍是一行**）。
  注意该用例断言的是「行数 + 名字 + 空地址」，没有单独比对 `hostId` 字符串。

**A3：unit 渲染 + `strict`/`legacy` 双档 + systemd 版本探测**

- `internal/adapters/runtime/unitfile/unit.go`（280 行）：`Tier`（第 17 行）、
  `TierStrict`/`TierLegacy`（第 20–23 行）、`MinSupportedSystemdVersion = 219`（第 28 行）、
  `TierStrictMinVersion = 240`（第 31 行）、`TierFor`（第 47 行：`≥ 240` → `strict`、
  `219–239` → `legacy`、`< 219` 报错）、`Degradations`（第 60 行）、`Rendered`（第 81 行）、
  `RenderUnit`（第 92 行）、`RenderForVersion`（第 201 行）、`RenderForHost`（第 210 行）、
  `writeHeader`（第 220 行）、`ReleaseRootDir`（第 241 行）、`checkRenderablePath`（第 253 行）、
  `wholeSeconds`（第 271 行）。
- `internal/adapters/runtime/unitfile/probe.go`（92 行）：`VersionRunner`（第 20 行）、
  `SystemctlArgv`（第 24 行）、`Prober`（第 27 行）、`Detect`（第 42 行）、
  `ParseSystemctlVersion`（第 69 行）、`CommandRunner`（第 87 行，`exec.CommandContext` 执行 argv）。
  探测走注入式接口，「执行器只接受 argv、不经过 shell」的既有不变量不变。
- 渲染结果：`strict` 档 = `ProtectSystem=strict` + `ReadWritePaths=`（`writable` 覆盖
  `workingDirectory`、`logs.directory` 与 `ReleaseRootDir`）+ `StandardOutput=append:`；
  `legacy` 档 = `ProtectSystem=yes` + `ReadWriteDirectories=` + `StandardOutput=journal`
  （两档的加固指令块在第 159–183 行）。降级语义同时写进 unit 头部注释（第 226–231 行）
  与 `Rendered.Degradations`。
- `legacy` 档未在真实主机/容器验证的事实被固化为常量 `tierLegacyUnverifiedNote`（第 42 行）并
  写进 unit 头部；第 40 行的注释明确「不得写成『已支持 219』」。
- 测试：`unit_test.go`（398 行）用两份**完整黄金文本** `strictUnit`/`legacyUnit` 逐字节比对
  （`TestRenderUnitStrictTier`:130、`TestRenderUnitLegacyTier`:153 都是 `got.Content != <golden>`），
  另有档位与版本必须自洽（`:276`）、路径字节拒绝（`:295`）、亚秒超时拒绝（`:323`）、
  `TierFor` 边界（`:331`）、头部记录版本与档位（`:187`）；`probe_test.go`（195 行）覆盖版本解析、
  探测、注入与 argv-only。

**A4：systemd 适配器本体**

- `internal/adapters/runtime/systemd/{systemd.go,runner.go,files.go}`（526 / 79 / 411 行）
  + 测试 `systemd_test.go`（901 行）、`fakes_test.go`（250 行）、`contract_test.go`（35 行）、
  `credentialpath_internal_test.go`（81 行）。
- 注入点：`WithRunner`:79、`WithGOOS`:85、`WithOwnerResolver`:90、`WithClock`:95、
  `WithProbeTimeout`:100、`WithEUID`:110、`WithLogger`:117；`RootPath`:172、`UnitDecision`:177。
- **`unitStatus`（第 372 行）刻意不用 `systemctl show --value`**：第 368–370 行的注释记录已比对
  v219/v228/v229/v230 的 `systemctl.c`——`--value` 是 systemd 230 才加的，而本适配器承诺支持到
  219，用它等于把 219–229 上的 `Status`/`Start`/`Health` 一起废掉；改为容忍并切分
  `ActiveState=` 前缀（`parseActiveState`，第 406 行）。
- **`Prepare`（第 212 行）幂等**：`writeFileIfChanged`（`files.go` 第 348 行）内容未变不重写，
  也就不会白白触发 `daemon-reload`（`TestPrepareDoesNotRewriteUnchangedFiles`:707 用 mtime 断言）。
- **`Health`（第 321 行）与 `Status`（第 306 行）分离**：未就绪返回快照；只有确定性失败
  （unit `failed`、启动超时 `startDeadlineExceeded`，第 419 行）才返回 `RUNTIME_NOT_READY`。
- **就绪探测抽成共享包**：`internal/adapters/runtime/readiness/readiness.go`（94 行）的
  `Check`（第 26 行）与 `Tracker`（第 63 行）由 `proc`（`proc.go`:256）与 `systemd`
  （`systemd.go`:345）共用，避免两个适配器各写一份 `consecutiveSuccesses` 语义
  （A4 之前这份逻辑在 `proc` 内部）。
- **凭据交付**：`resolveSecrets`（`files.go` 第 133 行）先全部解析、再统一落盘，使
  `SECRET_UNRESOLVED` 出现在「还没有写任何文件」的时候；`kind:env` 含换行时报
  `SECRET_UNRESOLVED` 并指向字段。`kind:file` 的跨用户可读性由 `ensureCredentialReachable`
  保证——**这是本迭代最容易被漏掉的一处**，单独记在第 7 节的权限例外小节。
- **合约**：`TestSystemdAdapterContract`（`contract_test.go`:17）在**假 `systemctl` + 根前缀**下
  跑同一套 `runtimecontract`（11 项子测试）。它证明「适配器与合约的语义一致」；真实 systemd 上的
  验证由 A7 的容器断言完成。

**A4 的后续修复（由 A7 的 Linux 容器实跑发现，2026-09-23）**

> 这是本迭代最有价值的一条记录：**本地全绿的测试挡不住部署形态差异**。

- **缺陷**：`ensureCredentialReachable` 原先只把「目录属主 == 本进程 euid」当作可收敛
  （等价于把 `opsd` 当成以服务用户运行）。但适配器要 `useradd`/`chown`/`systemctl`，
  **必须以 root 运行**；而安装约定把 `/etc/opsd` 的属主给了**服务用户**（harness 里是
  `frz-ops`）。于是在容器里 root 明明有权限修那个目录，代码却判成「不属于我们」→ 直接
  走 `PERMISSION_DENIED`，`runtime prepare` 在**完全正常的部署**上失败。
- **修法**（`files.go`）：新增纯函数 `canChmod(euid, dirUID)`（第 258 行，`euid == 0 || dirUID == euid`），
  并把可收敛范围显式收敛为「归我们管理」的**三级集合**（第 196–200 行：凭据目录、
  `/etc/opsd/apps`、`/etc/opsd`）；**`/` 与 `/etc` 只校验、绝不修改**。
- **测试**：新增表驱动 `TestCanChmod`（`credentialpath_internal_test.go` 第 43 行）。
- **效果**：修复后容器里该断言由 FAIL 转 PASS，其余 45 项 runtime 断言同时转 PASS
  （它们是同一个失败原因的下游）；第 5 轮 `make verify-linux` 89/0。
- **教训（写进 §12 与 §17 的同一句话）**：这类差异只在「以 root 运行 + 目录属主是别人」这种
  真实部署形态下暴露，**用假的 runner / 临时根前缀永远测不出来**——harness 的第二个 root 实例
  就是为它存在的。

**A6a：spec / hosts / environments / 制品下载的 API + CLI + client**

- 协议类型：`api/v1/spec.go`（`ApplicationSpec` 与各段共 10 个类型）、`api/v1/host.go`
  （Host/Environment 的请求与响应）。路由在 `internal/adapters/httpapi/server.go`
  第 68、76–91 行：`GET /api/v1/artifacts/{id}/content`、`PUT|GET /api/v1/applications/{id}/spec`、
  `GET|POST /api/v1/hosts`、`GET /api/v1/hosts/{id}`、`GET|POST /api/v1/environments`、
  `GET /api/v1/environments/{id}`。
- **制品下载**：`ArtifactService.Download`（`internal/application/artifact.go` 第 156 行）
  **先 `Verify` 从磁盘重算摘要并与记录比对，再开流**（不是先开流再验）；
  `handleDownloadArtifact` 设置 `Content-Type`/`Content-Length`/`ETag`
  （`internal/adapters/httpapi/artifact.go` 第 146–149 行，ETag 就是内容摘要）。
- client：`internal/adapters/client/spec.go`、`host.go`（含 `CreateHost`/`CreateEnvironment`）。
- CLI：`opsctl spec put|show`、`opsctl host list|inspect|create`、`opsctl env list|inspect|create`、
  `opsctl artifact download <id|digest> --output <path>`（`cmd/opsctl/artifact.go` 第 149 行）。
  第 9 节只要求 `host list` 与 `env list`；多出的 `create`/`inspect` 是对 API 里已有的 `POST`
  与详情端点的自然补全（如实记录，不是新增能力）。
- 错误码：`SPEC_NOT_FOUND`/`HOST_NOT_FOUND`/`ENVIRONMENT_NOT_FOUND`/`MANIFEST_INVALID`/
  `MANIFEST_CONFLICT`/`ARTIFACT_CHECKSUM_MISMATCH` 现在都有实际消费方。
- 测试：`internal/adapters/httpapi/spec_test.go`、`host_test.go`、
  `internal/adapters/client/client_test.go`、`test/e2e/spec_test.go`
  （`TestSpecPutShowRoundTripThroughCLI`:69、`TestSpecPutRejectsInvalidManifestThroughCLI`:143、
  `TestArtifactDownloadThroughCLI`:220、`TestHostAndEnvironmentCommandsThroughCLI`:267）。

**A6b：`runtime/*` 端点 + `runtime.*` 走 Operation + 适配器装配**

- 协议：`api/v1/runtime.go`（`RuntimeDecision`、`RuntimeActionRequest` 与三个响应类型）；
  `api/v1/types.go` 第 15–16 行新增 `KindRuntimeStart`/`KindRuntimeStop`。
- 路由：`server.go` 第 78–82 行——`POST .../runtime/{validate,prepare,start,stop}`、
  `GET .../runtime/health`。`validate`/`prepare`/`health` 是**同步用例、不进 Operation**
  （「这份规格能不能执行」「现在有没有就绪」都是查询，排队没有意义）。
- **无旁路**：`start`/`stop` 走与 `POST /api/v1/operations` 相同的 `Service.Create`
  （`internal/adapters/httpapi/runtime.go` 第 65 行起），因此锁、幂等键、请求摘要、审计、日志与
  取消全部复用；执行在 `internal/application/worker.go` 的 `executeRuntime`（第 215 行起）。
- 三条设计判断（都在代码注释里）：
  - **(a) 适配器不可用时快速失败、不建 Operation**：适配器可用性是部署属性，可在创建侧同步判定
    （`internal/application/service.go` 第 91 行 `s.runtime == nil` → `RUNTIME_UNSUPPORTED`）；
    执行期另有一道兜底（`worker.go` 第 218 行）。
  - **(b) `runtime.start` = 幂等 `Prepare` + `Start`**：共享合约要求「未 `Prepare` 直接 `Start`
    必须被拒绝」，所以「把应用跑起来」这一个操作必须自带准备步骤，否则在从未 prepare 的主机上
    start 永远失败；`runtime.stop` 只 `Stop`（systemd 对未运行的 unit stop 同样成功，天然幂等）。
    见 `worker.go` 第 208–211 行的注释。
  - **(c) `dryRun` 对 `runtime.*` 一律拒绝**：适配器端口没有 dry-run 语义，接受它会变成
    「以为只是预演、其实真的启停进程」，返回 `INVALID_REQUEST` 并提示改用 `runtime validate`
    （`service.go` 第 85–90 行）。
- 执行的规格来源：读「应用的**当前**规格」而不是创建时的快照（`op.Spec` 留空），
  因此 `spec put` 之后重试 `runtime.start` 启动的是最新 manifest；资源规范成应用 ID，
  同一应用用名字或 ID 提交命中同一把锁（`service.go` 第 94–102 行）。
- **决策可追溯**：`recordRuntimeDecision`（`worker.go` 第 271 行起）把
  `tier`/`systemdVersion`/`unitPath`/`degradations` 写进 `phase=prepare` 的 Operation 日志，
  并追加审计 `runtime.prepared`（`internal/domain/operation.go` 第 21–23 行、
  `internal/domain/audit.go` 第 25–27 行）。适配器没给出决策时什么都不写，而不是编一个空档位。
- **装配**：`cmd/opsd/main.go` 第 100–116 行——**平台选择只有这一处**：`GOOS == "linux"` 才装配
  systemd 适配器，其它平台刻意不注入（`runtime.*` 于是返回 `RUNTIME_UNSUPPORTED`），
  **刻意不加 `runtime.adapter` 之类的配置开关**，避免生产误选 `proc` 假适配器。
  `cmd/opsd/runtime.go` 的 `systemdReporter` 把适配器私有的 `UnitDecision` 翻译成
  `application.RuntimeDecision`：这层翻译只能放在装配层，否则 `systemd`（已 import
  `application`）与 `application` 会成环。
- **health 语义**：未就绪 → `200` + `ready:false`（供轮询）；确定性失败 → `409` +
  `RUNTIME_NOT_READY`（`internal/application/runtimeops.go` 第 75–76 行的注释、
  `internal/adapters/httpapi/runtime.go` 第 101–124 行）；`opsctl runtime health` 未就绪时
  退出码 **19**（`cmd/opsctl/runtime.go` 第 121、143 行）。
- 测试：`internal/application/runtimeops_test.go`（14 个用例：dry-run 拒绝、适配器缺失、
  manifest 缺失、幂等、取消、决策写入日志与审计等）、`internal/adapters/httpapi/runtime_test.go`、
  `internal/adapters/client/client_test.go`、`cmd/opsctl/runtime_test.go`、
  `test/e2e/runtime_test.go`（`TestRuntimeWithoutAdapterThroughCLI`:35——本机没有适配器时退出码
  21，且未登记 manifest 时 `SPEC_NOT_FOUND`（退出码 2）优先）。

**A7：`test/linux/` 的 1c 容器断言（已在容器中实跑通过）**

- `test/linux/verify.sh` 新增 `check_runtime`（第 431 行起），在 `main()` 里**排在最后**：
  它的最后一条断言会删掉敏感环境文件、让探针 unit 进入失败/重启循环。
  当前调用顺序是 `check_filesystem → check_socket_acl → check_executor →
  check_artifacts_and_secrets → check_schedules → check_systemd → check_runtime`。
- **两个 opsd 实例**（这是本轮的结构性变化）：原来的实例仍以服务用户 `frz-ops` 运行，
  **迭代 0 的既有断言（1b 时点共 40 项，历史快照）全部仍打在它上面、前缀未动**；新增一个**以 root 运行**的实例
  （配置 `test/linux/opsd.root.verify.yaml`，独立 socket `/run/opsd-root/opsd.sock`、
  独立数据库/工作目录/日志/制品目录），`check_runtime` 只打它——因为适配器要
  `useradd`/`chown`/`systemctl`，必须 root。`wait_for_socket`/`wait_for_status` 做了参数化，
  客户端助手是 `runtimectl` / `rq`（`verify.sh:132-133`）。
- 新增探针应用 `test/linux/probe/main.go`（107 行）：把凭据的**长度与 sha256** 写进报告文件后
  再监听就绪端口，**绝不打印明文**；同时采集 `ProtectSystem` 允许/拒绝写入的实测结果。
- 覆盖内容：`Prepare` 产物（用户存在、目录 `0750` 属主 `runUser`、两个环境文件 `0600`、
  凭据目录 `0700`、凭据文件 `0600` 且内容与源逐字节一致、unit `0644`、头注释含版本与档位、
  **第二次 `Prepare` 幂等且档位不变**）；`/etc/opsd` 在 `Prepare` 后为 `0751` 且**真实跨用户读取**
  （`runuser -u <runUser> -- cat` 成功、`frz-other` 被拒）；两类凭据经 `EnvironmentFile` 到达进程后
  长度与 sha256 与源一致（含 `\`、`"`、字面 `\n`、`$`、单引号与首尾空格）；`runtime start` 走
  `Operation` 且终态 `succeeded`、`runtime health` 就绪、`systemctl is-active` = `active`、
  `runtime stop` 终态 `succeeded`、停止后 `inactive`；删掉 `<app>.secrets.env` 后
  `systemctl start` 必须失败；unit 声明路径可写、未声明路径被 `ProtectSystem=strict` 拒绝。
- **一次不稳定断言的修正**：`Restart=on-failure` 下 unit 失败后 `ActiveState` 只是瞬时 `failed`，
  等待自动重启期间是 `activating`（`SubState=auto-restart`），且 `RestartSec=5` 永远凑不满默认
  start limit，所以「缺凭据必须启动失败」不能断言瞬时状态。现改为「12×0.5s 窗口内从未 `active`，
  且观察到的状态只能属于 `activating`/`failed`」，并把观察到的状态集合打进输出
  （`verify.sh` 第 649–678 行）。**不要把「停止」与「失败」混为一谈。**
- **断言数（只认一个权威口径）**：权威值 = 脚本运行时打印的「`%d` 项通过，`%d` 项失败」
  （`verify.sh` 第 721 行），本次实跑为 **89 项通过 / 0 项失败**，其中 `check_runtime` 占 49 项。
  历史快照（仅供对照，不要引用为结论）：1b 时点 40 项；A7 落地时的静态计数推导值 87 项
  （**该推导偏小**，实跑为 89）。
- **`/etc/opsd` 的两条断言不矛盾，是时序不同**：`check_filesystem` 第 247 行的
  「`/etc/opsd` 目录模式 = `750`」发生在 `Prepare` **之前**（安装脚本的状态，保留不变）；
  `check_runtime` 的 `751`（第 558、560 行）发生在 `Prepare` **之后**。安装时 `0750`、
  首次 `Prepare` 后 `0751`。
- **污染检查**：容器内只有 `/sys/fs/cgroup` 一个挂载（外加 `--tmpfs /run /tmp`），
  **没有仓库 bind mount**，因此容器内的操作结构上不可能写进仓库；`output/` 的 mtime 早于本次
  运行，未被触碰。
- **过程本身是 harness 不是空转的证据**：同一份 harness 连跑 5 轮，前 4 轮分别 25 FAIL / 1 FAIL /
  1 FAIL，第 5 轮 0 FAIL——失败会真的挂住；细节与那处**产品缺陷**（`canChmod` 不认 root）见第 17 节与
  A4 的「后续修复」。

### 对规格的补充与偏离

1. **`readiness.type` 收缩为 `tcp`/`http`**（`exec` 未实现）：这是实现刻意比规格更严的一处，
   完整说明与理由见第 6 节末尾「实现比规格更严的三处」第 1 条（`internal/domain/appspec.go`
   第 39–44 行是判断的落点），此处不再重复。**第 4 节的 yaml 因此是过时的**：`exec` 不在
   已实现范围内，若确实需要，必须先冻结语义再实现。
2. **实际路径与第 3/12 节的目录建议有偏移**（目录选择见提交说明，本文档此前未指定）：
   - 转义函数从 `internal/adapters/runtime/systemd/` 迁到
     `internal/adapters/runtime/unitfile/`；理由是 `proc` 与 `systemd` 必须共用同一份转义，
     各写一份等于把「本地能跑、真机不能跑」的故障固化。
   - 假适配器在 `internal/adapters/runtime/proc/`；合约套件在
     `internal/application/runtimecontract/`（这一处与第 3 节一致）。
   - manifest 严格解码放在 `internal/adapters/manifest/`（第 4 节未指定位置）。
   - 真实 systemd 适配器最终落在 `internal/adapters/runtime/systemd/`（见 A4）；就绪探测与
     unit 渲染分别抽到 `internal/adapters/runtime/readiness` 与 `runtime/unitfile`，
     由 `proc` 与 `systemd` 共用。
3. **新增 `SpecUnpack.StrategyExplicit`**（第 84–88 行）：严格解码后无法区分「显式写了
   `none`」与「压根没写」，而只有显式声明才可能与制品 `mediaType` 冲突
   （`MANIFEST_CONFLICT`）。这是第 4 节校验要求的一处必要补充。
4. **`SecretRef` 校验失败改报 `MANIFEST_INVALID`**（第 230–234 行）：调用方提交的是 manifest，
   透传 1a 的 `INVALID_REQUEST` 会让错误类型与提交内容不对应。
5. **`Host`/`Environment` 先有模型、后有持久化与自举**：第 5 节的模型与校验在
   `internal/domain/host.go`（提交 `3938db4`），表、仓储与「本机 Host 自举」随 A2 落地。
   第 5 节「不在此迭代做『应用绑定主机』的校验」的结论不变。
6. **`Host`/`Environment` 的错误码**：第 10 节的表里没写它们，实现新增了
   `HOST_NOT_FOUND`（404/2）与 `ENVIRONMENT_NOT_FOUND`（404/2，常量名 `CodeEnvNotFound`）；
   两个新端点的创建重复走 `INVALID_REQUEST`，不再新增错误码（与 1a/1b 处理重名资源一致）。
7. **CLI 表面比第 9 节多两个子命令组**：除 `host list`、`env list` 外还有
   `host create|inspect`、`env create|inspect`，理由见 A6a。
8. **`runtime.start` 的语义补白**：第 9 节只写了「走 `Operation`」，没说它自带 `Prepare`；
   实现是「幂等 `Prepare` + `Start`」，理由是共享合约要求未 `Prepare` 不得 `Start`（见 A6b）。
9. **`dryRun` 对 `runtime.*` 一律拒绝**：第 9 节未提，实现返回 `INVALID_REQUEST` 并提示改用
   `runtime validate`（见 A6b）。

### 未验证清单

第 2 节「1c 实现」全部落地，第 13 节的 12 条验收标准**11 条已达成、1 条部分达成**
（第 8 条的 `legacy` 档）。剩下的证据缺口只有两类，都**不能**由 Linux 容器证据替代：

| 条目 | 状态 | 说明 |
| --- | --- | --- |
| `legacy` 档（systemd 219–239）的实际行为 | **未验证** | 容器是 Ubuntu 24.04 / systemd 255，只覆盖 `strict` 档（≥240），**零 `legacy` 证据**；该档目前只有单元测试（两份黄金文本）+ 静态检查支撑，不得声称「已支持 219」。第 15 节记录了为什么本地容器路走不通。 |
| 真实 Linux 主机项：reboot 后 unit 持久化、`WantedBy=multi-user.target` 实际生效、SELinux/AppArmor、sudoers/PAM | **未验证** | 容器只验证内核级语义（文件模式、属组、Unix Socket ACL、systemd 生命周期），**不是 Linux 主机**；这几项仍需真实主机。 |

已不再属于缺口的两项（记录一下变化）：**systemd 真实执行端到端**已由容器内的
`check_runtime` 走通（真实 `systemctl` 的 `Prepare`→`Start`→`is-active`→`Health`→`Stop`）；
**1c 的 Linux 容器证据**已取得（`make verify-linux` 第 5 轮 89/0）。

第 2 节的其余条目（systemd 适配器、版本分档、`Host`/`Environment` 模型与持久化、制品下载端点、
manifest 端点、`runtime/*` 端点、hosts/environments 端点、CLI、`runtime.*` 走 Operation、
适配器装配、容器断言）均已落地，见上文两段「已实现范围」。

## 17. 验证记录 2026-09-23

本节分三段：**已提交基线**（HEAD `747aee9`，A2–A6b 落地之前）、**A2–A6b 落地之后**
（当前工作树）、以及 **A7 容器断言落地并在容器中跑通**（第 5 轮 89/0）。

### 已提交基线（HEAD `747aee9`）

- 执行者：Command Code agent（只读审计核实 + 文档回填；本次**未修改任何代码**）
- 变更范围：`3938db4`、`747aee9` 两个提交
- 证据来源：下表的命令结果为 HEAD `747aee9` 已核实存在的既有证据（只读审计逐项核对）；
  本次为写文档只重跑了静态计数类检查（`grep -c '^func Test'`、路径存在性），**未重跑测试套件**。

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `gofmt -l .` | PASS（无输出） | 静态 |
| `go vet ./...` | PASS（无输出） | 静态 |
| `go test ./...` | PASS（12 个包 ok） | 单元 + 集成 + e2e |
| `go test -race -count=1 ./...` | PASS（全 ok，e2e 44.6s） | 单元 + 集成 + e2e |
| `go test -v -count=1 ./...` | PASS（181 个顶层用例 0 FAIL 0 SKIP；含子测试共 313 PASS） | 单元 + 集成 + e2e |
| `GOOS=linux GOARCH=amd64` 与 `arm64` 交叉编译 | PASS | 交叉编译 |
| GitHub Actions `test` job（HEAD `747aee9`） | success | CI（干净 runner） |
| GitHub Actions `linux-verify` job（HEAD `747aee9`） | success | **Linux 容器**（CI runner） |
| `make verify-linux`（本机） | **NOT RUN** | **Linux 容器**（本机 docker daemon 不可达） |

远端 CI 记录：共 3 次 run 全部 success，HEAD 的 `test` 与 `linux-verify` 两个 job 均 success
（2026-09-22T06:28:30Z–06:31:29Z）。

### A2–A6b 实现落地（2026-09-23 工作树，尚未提交）

- 执行者：实现者实跑；本节的表由文档看守者据其报告 + 静态核对写入，**文档看守者未重跑任何门禁**
  （只做了文件存在性、行号与测试名的 `grep` 核对）
- 变更范围：A2 / A3 / A4 / A6a / A6b（工作树，HEAD 未变）
- 环境：macOS (darwin/arm64)，Go 1.27.1；**没有 systemd、没有 Linux**

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `make fmt` | PASS | 静态 |
| `make vet` | PASS | 静态 |
| `make test` | PASS（**16 个包 ok**） | 单元 + 集成 + e2e |
| `make test-race` | PASS（`-count=1` 强制重跑） | 单元 + 集成 + e2e |
| `GOOS=linux GOARCH=amd64` 与 `arm64` 交叉编译 | PASS | 交叉编译 |
| `make verify-linux` | 当时未执行（后续在 A7 中实跑：第 5 轮 89/0，见下） | **Linux 容器** |

「16 个包」可由静态清点核对：仓库里含 `func Test` 的包目录共 16 个（`cmd/opsctl`、`blob`、
`client`、`config`、`executor`、`httpapi`、`manifest`、`runtime/proc`、`runtime/readiness`、
`runtime/systemd`、`runtime/unitfile`、`secret`、`sqlite`、`application`、`domain`、`test/e2e`）。

**证据类型必须分开读**：

- 上表的**单元 / 集成 / e2e** 三列是真实的：A2–A6b 各自都有对应测试（见第 16 节逐条列出的
  测试名），`test/e2e/*` 是真实二进制 + 真实 Unix Socket 的端到端用例。
- **systemd 真实执行已在容器中验证**：A4 的合约测试跑在「假 `systemctl` + 根前缀」上，
  但 A7 的 `check_runtime` 进一步在**真实 systemd 255** 上走通了
  `runtime prepare/start/health/stop` 与 `systemctl is-active`（见下一条）。
- **Linux 容器证据已取得**（见下一条）：`make verify-linux` 第 5 轮退出码 0、89/0。
- **跨用户凭据读取已由容器验证**：第 7 节那条 `/etc/opsd` 补 `0751` 的行为，
  在容器里由 `runuser -u <runUser> -- cat` 成功、`frz-other` 被拒两条断言证明。
- `legacy` 档、真机 reboot 持久化、SELinux/AppArmor、sudoers/PAM 仍未验证（见第 16 节末尾清单）。

### A7 容器断言落地并在容器中跑通（2026-09-23）

- 执行者：A7 实现者（实跑 + 修复）；文档看守者据其报告与静态核对写入，**本人未重跑容器**
- 变更范围：`test/linux/verify.sh`（新增 `check_runtime`、第二个 root 实例、参数化等待助手）、
  `test/linux/probe/main.go`（新增探针）、`test/linux/opsd.root.verify.yaml`（新增 root 实例配置）、
  `internal/adapters/runtime/systemd/files.go`（`canChmod` 修复）+ `credentialpath_internal_test.go`
  （新增 `TestCanChmod`）；`.github/workflows/ci.yml` **未改**
- 环境：macOS (darwin/arm64) + OrbStack；容器内 **Ubuntu 24.04 / systemd 255 / arm64**

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `make verify-linux`（第 1 轮） | **FAIL：25 项失败 / 共 65 项** | **Linux 容器** |
| `make verify-linux`（第 3、4 轮） | **FAIL：各 1 项失败 / 共 87、88 项** | **Linux 容器** |
| `make verify-linux`（第 5 轮） | **exit 0：89 项通过 / 0 项失败**（`check_runtime` 占 49 项） | **Linux 容器** |
| 修复后门禁：`make fmt` / `make vet` / `make test` / `make test-race` / `bash -n test/linux/verify.sh` | 全部 exit 0（实现者实跑） | 静态 + 单元 + 集成 + e2e |
| 污染检查：容器挂载只有 `/sys/fs/cgroup`（+ `--tmpfs /run /tmp`），无仓库 bind mount；`output/` mtime 未变 | PASS | 静态 |

**断言数的唯一权威口径**：脚本运行时打印的「`%d` 项通过，`%d` 项失败」（`verify.sh` 第 721 行）
= **89**。历史快照仅供对照：1b 时点 40 项；A7 落地时的静态推导值 87 项（偏低，实跑 89）。

**这 5 轮过程本身就是 harness 在真实执行的证据**：第 1 轮 25 FAIL 说明断言不是空转，
失败会真的挂住；随后每修一处就少一批 FAIL，直到 0 FAIL。

#### 本轮跑出来的一处产品缺陷与修复（最重要的一条）

- **现象**：`runtime prepare` 在一个**完全正常**的部署形态下直接失败——`opsd` 以 root 运行
  （`useradd`/`chown`/`systemctl` 需要），而 `/etc/opsd` 的属主是**服务用户**。
  `ensureCredentialReachable` 原先只有「目录属主 == 本进程 euid」这一支，于是 root 明明能修
  却被判成「不属于我们」→ `PERMISSION_DENIED`。第 1 轮的 25 项失败中有 1 项是它，
  其余 45 项 runtime 断言是它的下游（同一个失败原因）。
- **修复**：新增纯函数 `canChmod(euid, dirUID)`（`files.go` 第 258 行：`euid == 0 || dirUID == euid`），
  并把可收敛范围显式收敛为三级「归我们管理」集合（第 196–200 行），
  **`/` 与 `/etc` 只校验、绝不修改**；新增表驱动 `TestCanChmod`
  （`credentialpath_internal_test.go` 第 43 行）。
- **修复后**：该断言转 PASS，其余 45 项 runtime 断言同时转 PASS；第 5 轮 89/0。
- **教训**：本地全绿的测试挡不住这类**部署形态差异**——假 runner / 临时根前缀下，
  「进程身份」与「目录属主」的关系永远是测试自己造的那一种。这正是需要第二个
  **以 root 运行**的 `opsd` 实例的原因。

#### 本轮修正的一条不稳定断言

`Restart=on-failure` 下 unit 失败后 `ActiveState` 只是瞬时 `failed`，等待自动重启期间是
`activating`（`SubState=auto-restart`），`RestartSec=5` 又凑不满默认 start limit；第一版断言
瞬时 `failed`，实跑当场挂掉（第 3/4 轮各 1 FAIL 的来源）。现改为「12×0.5s 窗口内从未 `active`，
且观察到的状态只能属于 `activating`/`failed`」，并把观察到的状态集合打进输出
（`verify.sh` 第 649–678 行）。**「停止」与「失败」不是一回事**，断言不能混用。

#### 仍未验证（本节不覆盖）

- **`legacy` 档（systemd 219–239）**：本次容器是 systemd **255**，只覆盖 `strict` 档，
  **零 `legacy` 证据**；不得写成「已验证支持 219」。
- **真实 Linux 主机**：reboot 后 unit 持久化、`WantedBy` 实际生效、SELinux/AppArmor、
  sudoers/PAM。容器证据**不能**替代它们。

### 2026-09-24：1c 的 e2e 用例首次在 Linux 上执行，暴露一处平台耦合缺陷并修复

**背景**：1c 的实现提交后（`1168f29`）CI 的 `test` job 在干净 ubuntu-latest runner 上**失败**
——本地 macOS 全绿、CI 红。这是 1c 的 e2e 用例第一次在 Linux 上执行：此前 CI 跑过的
`test/e2e` 还是 1b 的用例集，`runtime_test.go` 是随该提交才进入仓库的。

**缺陷**（`test/e2e/runtime_test.go`）：`TestRuntimeWithoutAdapterThroughCLI` 把「本机没有运行时
适配器」当作前提，断言 `runtime.{validate,prepare,start,stop,health}` 全部退出码 21
（`RUNTIME_UNSUPPORTED`）。这个前提**只在非 Linux 上成立**——装配层的平台选择是
`GOOS == linux` 才注入 systemd 适配器（`cmd/opsd/main.go`）。于是在 Linux 上 `runtime validate`
返回 `0`，断言失败（CI 日志：`runtime validate want exit 21 (RUNTIME_UNSUPPORTED), got 0`）。

**修法**：按 `GOOS` 分断言，两个平台各钉各自成立的那个命题，而不是把 macOS 的行为写死：

- 非 Linux（macOS）：保持原意——适配器不可用，五个动作都必须快速失败为退出码 21，且不留
  任何 `runtime.*` 操作行（`start` 是快速失败：适配器不可用是部署属性，排队没有意义）。
- Linux：断言**反面**——适配器确实被装配，`runtime validate` 退出码 0；因为是同步用例，
  同样不留 Operation 行。真正的启停与就绪链路需要 root 与 systemd，不在 e2e 的覆盖范围内，
  由 `test/linux/verify.sh` 的 `check_runtime` 承担。

**证据**：

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `go test ./test/e2e/... -run TestRuntimeWithoutAdapterThroughCLI -v` | PASS（走非 Linux 分支） | e2e（macOS） |
| 容器内 `go test ./test/e2e/... -run TestRuntimeWithoutAdapterThroughCLI -v` | PASS；`POST /applications/billing-api/runtime/validate` = **200**，容器 systemd 252 | e2e（Linux 容器） |
| `gh run view 35942862018 --log-failed` | 失败点＝`runtime_test.go:63`，「want 21, got 0」 | CI 日志 |

Linux 侧的做法：`docker build` 一个 `golang:1.27-bookworm` 镜像，`apt-get install systemd`
只为拿到 `systemctl` 二进制（版本探测走 `systemctl --version`，它只输出编译进去的版本，
不需要 systemd 作为 PID 1）；源码用 `git archive HEAD` 做**干净副本**而不是 bind mount
macOS 目录——后者对权限断言不保真，是 AGENTS.md 已记录的坑。

**教训**：e2e 此前只在 macOS 上跑过，平台耦合缺陷直到 CI 才暴露。这与 A7 那次「本地全绿的
测试挡不住部署形态差异」是同一类问题的两个面——**断言里凡是用到「本机如何」的前提，都要先
问它在另一个受支持平台上是否成立**。

### 2026-09-24（补记）：1c 的容器断言首次在干净 runner 上跑全，89/0

修复提交（`3c7376e`）后 CI run `35944393593` 两个 job 均通过：

- `test` job：`make ci`（fmt / vet / test / test-race / 交叉编译）在干净 ubuntu-latest
  runner 上通过，其中 `test/e2e` 走的是本次修正后的平台分支。
- `linux-verify` job：容器内 **89 项通过 / 0 项失败**（含 `check_runtime` 的 49 项），
  证据类型仍是「Linux 容器」。

**这是一条新证据，不只是复现**：1c 的容器断言此前只在本地 OrbStack（Apple Silicon，
**arm64**）上跑过，这是第一次在干净 runner 的 **amd64** 上执行。也就是说该证据现在覆盖
两种架构——容器断言里的路径、权限与 systemd 行为没有架构相关的偶然性。

**边界不变**：GitHub runner 同样是 cgroup v2 的虚拟环境，仍**不是**「Linux 主机」证据；
`legacy` 档与真机项（reboot 持久化、SELinux/AppArmor、sudoers/PAM）继续挂起并标注未验证。

### 结论汇总（2026-09-23，含本次修订）

- 第 13 节的 12 条验收标准：**11 条达成、1 条部分达成（第 8 条的 `legacy` 档）**。
  逐条的证据类型见第 13 节末尾的判定表——**单元 / 集成 / e2e** 与 **Linux 容器** 分列，
  没有把容器与真机混为一谈。
- 三处曾经成立的旧表述已被替换（保留在此以便追溯）：
  1. 「A2–A6b 只是工作树里的在建工作、未独立复核」→ 已落地且过门禁（第 16 节两段「已实现范围」）。
  2. 「第 2/3 条只有 `proc` 上的合约断言，其余各条全部未达成」→ 已不再成立（systemd 也过同一套合约）。
  3. 「A7 的容器断言只跑过桩、容器未运行」→ 已在容器中实跑：5 轮、第 5 轮 **89/0**。
- **仍未验证的两项**（不得写成已验证）：`legacy` 档（systemd 219–239）、真实 Linux 主机项
  （reboot 后 unit 持久化、SELinux/AppArmor、sudoers/PAM）。
- **本节的数字口径**：断言数一律以运行时输出为准 = **89**；`40`（1b 时点）与 `87`
  （A7 落地时的静态推导）只作历史快照。

## 18. 真实 Linux 主机验证 2026-09-24

**证据类型：Linux 主机**（不是 Linux 容器）。这是第 14 节与第 15 节一直挂着的那一项的第一次落地。

### 背景：同一台机器，用途不同

第 15 节记录过：用户曾提供 `root@192.168.11.101`，**只读探测后排除**，理由有二——
它是生产机（MES/MySQL/Redis/TDengine/EMQX/OpenResty，1Panel 管理），用户明确要求只隔离部署、
不碰 `/etc`、不建系统用户、不写 unit；且它是 systemd 257，比容器里的 255 还新，**证明不了
`legacy` 档**。

2026-09-24 用户重新确认该机「可以作为测试使用」，并在被明确问到时授权**允许在主机上安装**
（建专用系统用户、写 unit、装 opsd，不动既有业务文件）。因此本轮用途与上次不同：不是去
验证 `legacy` 档（那一条**仍未落实**），而是拿**真实主机**这一档的证据——RHEL 系发行版、
真实 systemd、**SELinux enforcing**、以及真实主机上的用户与权限落盘结果。

### 做法

| 文件 | 作用 |
| --- | --- |
| `test/host/run.sh` | 工作站侧：交叉编译 linux/amd64 的 `opsd`/`opsctl`/探针 → 上传 → 把断言脚本经 stdin 送进主机以 root 执行 |
| `test/host/verify.sh` | 主机侧（root）的断言脚本；**结尾把自己创建的一切删干净并逐项报告**（用户/组、unit、目录、数据库），`FRZ_HOST_KEEP=1` 可保留现场 |
| `test/host/opsd.host.verify.yaml` | 生产形态的 opsd 配置（`/etc/opsd`、`/run/opsd`、`/var/lib/opsd`） |

脚本经 stdin 送入是刻意的：它在结尾会删掉自己所在的那个目录，从文件执行会读到一半就没了。

opsd 本身在这轮里**以 systemd 服务运行**（harness 写一个测试用 unit），因为真机的部署形态
就是这样；`RuntimeDirectory=opsd` 是必需的——`/run` 是 tmpfs、开机即空，socket 的父目录必须
由 systemd 每次启动时建。**这一点在容器 harness 里被 `install -d` 掩盖了**。

### 环境事实（证据的一部分）

| 项 | 值 |
| --- | --- |
| 发行版 | Rocky Linux 10.2 (Red Quartz) |
| 内核 | 6.12.0-211.54.1.el10_2.x86_64 |
| systemd | 257 (257-23.el10_2.2.rocky.0.1-gb237c67) |
| SELinux | **Enforcing** |
| cgroup | cgroup2fs |
| 架构 | x86_64 |

### 结果：71 项通过 / 0 项失败

| 分组 | 断言数 |
| --- | --- |
| 前置（干净主机、端口空闲） | 3 |
| 安装态（模式、属主、SELinux 上下文） | 8 |
| opsd 以 systemd 服务运行（socket、RuntimeDirectory） | 6 |
| RuntimeAdapter Prepare（unit 落盘与内容、用户、权限收敛、幂等） | 17 |
| 凭据与环境的落盘（模式、属主、内容、副本逐字节一致） | 16 |
| 凭据路径的穿越链（运行用户可读、无关用户被拒） | 4 |
| start / health（真实 systemd 的 unit 生命周期、凭据逐字节到达进程、ProtectSystem=strict 的实际约束） | 14 |
| stop | 3 |

命令与结果：`bash test/host/run.sh` → **71 项通过，0 项失败**（上面的数字由脚本运行时打印，
分段计数由此得出）。

### 发现（三条都是真机才暴露得出来的）

1. **本工具在 SELinux 下能装能跑，但没有任何 SELinux 加固。** 实测上下文：unit 文件是
   `system_u:object_r:systemd_unit_file_t:s0`（正确），而 **opsd 进程与托管的应用进程都是
   `unconfined_service_t`**——我们不为托管应用装策略模块，于是它们落在默认的非受限域里。
   **结论要写在明处：SELinux 对托管应用的约束等于未生效。** 这不是缺陷而是**未实现的能力**，
   因此第 14 节那条「SELinux 未验证」从现在起改写成：**enforcing 下的实际行为已观测
   （不加固），提供 SELinux 策略模块属于未实现**。
2. **以 root 运行的 opsd 建出的 socket 是 `root:root 0660`**，非 root 用户用不了 `opsctl`；
   配置里**没有** socket 属组项。真机暴露的部署缺口，本轮不修，记为待定（今天的权宜做法是
   用 root 跑 opsctl）。
3. **端口的 `Status` 没有对外暴露。** `RuntimeAdapter.Status`（「进程本身的状态」）已实现、
   内部也在用（启动超时判定），但 **CLI 与 HTTP API 都只有 `health`**——运维问不出
   「进程活着但没就绪」这个状态，而端口注释里恰恰写着这两者「刻意不合并」。记为待定。

### 重启验证 2026-09-24（同一台机，经用户授权）

用户单独授权重启后做了这一轮。它补上的是从本节第 14 节起一直挂着的**「reboot 后的 unit
持久化」**——在此之前 `systemctl is-enabled` 只能证明「配置上是持久的」。

harness 因此分了三段：`FRZ_HOST_PHASE=prepare`（建状态并启动，**不清场**）→ 重启 → `check`
（重启后接着断言，跑完再删干净）。分段是必需的：跨重启存活的那个状态正是要留下的东西。

**重启事实**：发起 14:57:56，SSH 恢复 14:58:40（停机约 **44 秒**）；`boot_id`
`3ac249b2…` → `4146571f…`（**这两个值不同，是「真的重启过」的硬证据**，而不是在检查一台
没重启的机器）；开机的 9 个业务容器（全部 `RestartPolicy=always`）在恢复连接时已经全部回来。

**重启后 21 项通过 / 0 项失败**：

| 断言 | 结果 |
| --- | --- |
| `boot_id` 变了、系统运行时长只有 20 秒 | PASS |
| `frz-opsd-verify.service` 仍 enabled，且**自动**回到 active | PASS |
| **`/run/opsd` 被 systemd 重新创建（模式 750）、socket 重新出现（0660）** | PASS |
| opsd 能应答 | PASS |
| `frz-probe.service` 仍 enabled，且**自动**回到 active、health 就绪 | PASS |
| 托管进程仍以 `frz-probe` 运行（SELinux 上下文仍是 `unconfined_service_t`） | PASS |
| unit 文件、`/etc/opsd` 的 0751、配置 600、凭据 600 与凭据副本逐字节一致，全部保留 | PASS |
| **重启前创建的 Operation 重启后仍可查且终态仍是 `succeeded`**（任务引擎状态是持久的） | PASS |
| **重启后凭据仍逐字节到达进程**（探针开机重跑，报告是新的） | PASS |
| 主机上 9 个业务容器都回来了 | PASS |

其中「`/run/opsd` 被重新创建」这一条尤其值得记：`/run` 是 tmpfs、开机即空，socket 的父目录
**必须**由 systemd 在每次启动时建（harness 用的是 `RuntimeDirectory=opsd`）。这正是容器 harness
里被 `install -d` 掩盖、只有真实重启才验证得了的那一点。

### 仍未验证

- **`legacy` 档（systemd 219–239）**：本机 systemd 257 比容器里的 255 还新，仍然证明不了它。
  这一档继续标注**未验证**，不得声称「已支持」。
- AppArmor（RHEL 系没有）与 sudoers/PAM 的**实际策略**仍未验证（`SudoConfig` 至今只有模型、
  零行为）。

## 19. legacy 档真机验证 2026-09-24

**证据类型：Linux 主机**。第 14 节、第 18 节一直挂着的那条「`legacy` 档未验证」，在这里
第一次落地——而且是在**这一档最老的那一端**（systemd 219）上。

### 为什么之前验不了，这次为什么行

档位由探测到的 systemd 版本决定（`TierFor`）：≥240 走 strict，219～239 走 legacy。
容器 harness 是 systemd 255（strict），此前那台真机是 257（strict），**两处都证不了 legacy**。
而 legacy 档的 unit 模板只能用旧指令（`ProtectSystem=yes`、`ReadWriteDirectories=`、
`StandardOutput=journal`），这些取值在新 systemd 上**不会按老语义生效**，所以拿新机器凑不出
证据。这一次用户提供的 `root@43.142.95.141` 恰好是 **CentOS 7 / systemd 219 / cgroup v1**。

| 事实 | 值 |
| --- | --- |
| 发行版 / 内核 | CentOS Linux 7 (Core) / 3.10.0-1160.119.1.el7 |
| systemd | **219**（最低支持版本） |
| cgroup | **v1**（`stat -fc %T /sys/fs/cgroup` = `tmpfs`） |
| SELinux | Disabled |
| 其他 | 2 vCPU / 2 GiB / 无 docker / 初始无 JDK |

做法不变：`FRZ_HOST=root@43.142.95.141 FRZ_HOST_JAVA_HOME=/opt/jdk-17.0.20.1+1 make verify-host`。

### 结果

**118 项通过 / 0 项失败**（`full` 阶段）。其中 1c 那一组断言（Prepare 产物、凭据穿越链、
启停与就绪、凭据逐字节到达进程）在 legacy 档上全部成立，说明**同一套语义在旧档上依然是那套
语义**——这正是当初把两档分开时要防的事。

### 这一轮第一次量到的事实（都会进 unit 与审计）

| 断言 | 实测结果 |
| --- | --- |
| `RuntimeDirectory=opsd` + `RuntimeDirectoryMode=0750` | 219 上有效：`/run/opsd` 被创建、模式 750 |
| legacy unit 模板能否加载 | 能：`ProtectSystem=yes`、`ReadWriteDirectories=`、`StandardOutput=journal`、`NoNewPrivileges=`、`PrivateTmp=`、`EnvironmentFile=`、`RestartSec=`、`TimeoutStartSec/StopSec=` 全部被接受，进程正常起来 |
| `ProtectSystem=yes` 的保护范围 | **只保护 `/usr`**：运行用户自有的 `/usr/local/...` 写入被拒，而同样自有的 `/var/lib/...` 写入**成功**（合成实验与 harness 各测一遍） |
| `CPUQuota=200%` | 落到内核：`cpu.cfs_quota_us=200000`、`cpu.cfs_period_us=100000` |
| `MemoryMax=536870912` | **毫无效果**：unit 照常加载并启动，`systemctl show` 里没有它，cgroup 里也没有它 |
| `MemoryLimit=536870912` | 真的生效：`memory.limit_in_bytes=268435456`（探针值）与 `536870912`（harness 值）两次都落到 cgroup |
| `systemctl show --value` | **219 不支持**（该开关 systemd 230 才加入），报 `unrecognized option` |

由此产生两处改动（都在 3c 那一轮落地）：

1. **legacy 档的内存上限改用 `MemoryLimit=` 表达**（与 `MemoryMax=` 同义，231 起只是改名），
   不再「这一档拒绝内存上限」——因为实测证明这一档**表达得了**，用不着让用户为了「机器老」
   放弃限制。
2. **harness 停用 `systemctl show --value`**，改成解析 `Prop=value` 那一行。有意思的是
   **产品代码早就刻意避开了它**（`systemd` 适配器 `unitStatus` 的注释里写着「用了会把 legacy
   档的 Status/Start/Health 一起废掉」），是 harness 自己没跟上同一条规则。

### Java 与资源限制（迭代 3c）也在这台机上验完了

在这台机上装了 Temurin **JDK 17.0.20.1**（`/opt/jdk-17.0.20.1+1`，sha256 与 Adoptium 官方
发布值逐字节一致），用主机上的 `javac`/`jar` 构建真实 JAR、部署、跑起来，断言：JVM 报告的
工作目录就是 `current` 解析出的 release 目录、`/proc/<pid>/cmdline` 与 manifest 的 argv
逐元素一致、`-Xmx256m` 生效（堆上限 259522560 字节）、`MemoryLimit=` 与 `CPUQuota=` 在
systemd 与 **cgroup v1** 两侧都是声明的值、解释器路径写错时以 `MANIFEST_INVALID` 在部署**之前**
失败且不留副作用。

### 仍未验证

- **`legacy` 档的 232～239 那一段**：与 219 共享同一套 unit 模板，但没有那个版本段的主机跑过。
  措辞上不得写成「整档已验证」——代码里的 `tierLegacyNote` 就是这么写的。
- **重启验证**：这一轮只跑了 `full` 阶段，`prepare → 重启 → check` 那一轮（部署出来的 release
  跨重启存活）**没有做**。
- SELinux：这台机是 Disabled，enforcing 下的行为只在 Rocky Linux 那台机上观测过。
- AppArmor 与 sudoers/PAM 的实际策略：仍未验证。
