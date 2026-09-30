# frz-tools 使用手册

本手册面向直接使用 `opsd` / `opsctl` 的运维与开发。命令与旗标的**权威来源是二进制自带的
`--help`**（与本手册同源，均由代码生成）；本文补的是帮助文本放不下的东西：工作流顺序、
语义约定、退出码与排障。设计依据见 `docs/plans/` 下各迭代文档。

---

## 目录

1. [总览与形态](#1-总览与形态)
2. [构建与安装](#2-构建与安装)
3. [首次上手](#3-首次上手)
4. [第一个部署（单机）](#4-第一个部署单机)
5. [蓝绿发布](#5-蓝绿发布)
6. [多主机批量发布](#6-多主机批量发布)
7. [远程连接（mTLS）](#7-远程连接mtls)
8. [备份、恢复与保留](#8-备份恢复与保留)
9. [定时计划](#9-定时计划)
10. [制品与凭据](#10-制品与凭据)
11. [观察与排障](#11-观察与排障)
12. [配置参考](#12-配置参考)
13. [安全模型与边界](#13-安全模型与边界)
14. [附录 A：命令速查](#附录-a命令速查)

---

## 1. 总览与形态

```
opsctl ──Unix socket / mTLS──> opsd ──> systemd / nginx / pg_dump / mysqldump …
```

- **opsd** 是目标主机上唯一的执行者：Operation 状态机（`pending → {running, cancelled}`
  → `succeeded/failed/cancelled`）、资源锁（同一 `resource` 任意时刻最多一个未完成操作，
  否则 `LOCK_BUSY`）、审计、结构化日志、重试与退避，全部在 opsd 进程内完成。长任务执行
  期间不持有数据库连接；worker 在短事务中领取任务后提交，再启动进程。
- **opsctl** 只做两件事：把请求送到 opsd、把看到的结果汇总成视图（批量发布的编排就在
  opsctl 侧，见第 6 节）。
- **连接目标三选一**（互斥，同时给会报错）：`--socket`（默认 `/run/opsd/opsd.sock`）；
  `--host <name>`（打到本机库里登记的那台主机上）；`--remote <host:port>`（直接打那个
  地址，不查库，排障用）。
- **平台边界**：`opsd` 的运行时语义（useradd/chown/systemctl）只在 Linux 上可用，其它
  平台相关调用返回 `RUNTIME_UNSUPPORTED`（退出码 21）；`opsctl` 与配置校验类命令
  （`init`/`config validate`）在任何平台都能跑。

## 2. 构建与安装

要求 Go ≥ 1.27。仓库位于 <https://github.com/freezeChen/frz-tools>。

```bash
make build    # 本机平台，输出到 output/
make cross    # 交叉编译 linux/amd64 与 linux/arm64，输出到 output/linux-<arch>/
```

部署形态：**每台被管主机一个 opsd**。生产建议装成 systemd 服务，参考样例：

```ini
# /etc/systemd/system/opsd.service（按实际路径调整）
[Unit]
Description=frz-tools opsd
After=network.target

[Service]
ExecStart=/usr/local/bin/opsd --config /etc/opsd/opsd.yaml
Restart=on-failure
# RuntimeAdapter 要 useradd/chown/systemctl，opsd 需要 root（或具备等价权限的服务账号）
User=root

[Install]
WantedBy=multi-user.target
```

`make verify-host`（`test/host/`）演示了完整的真机安装、验证与清场流程，可直接参照。

## 3. 首次上手

```bash
opsctl init /etc/opsd/opsd.yaml        # 生成配置骨架；目标已存在时拒绝覆盖（--force 才覆盖）
opsctl init /etc/opsd/opsd.yaml --remote   # remote 段取消注释的版本：开远程用，需填真实证书路径
opsctl config validate --file /etc/opsd/opsd.yaml   # 离线校验，不连接 opsd
sudo opsd --config /etc/opsd/opsd.yaml
```

- 配置问题会**逐条**列出（如 `socket.path must be an absolute path` 对应的中文消息），
  全部解决前 opsd 拒绝启动（`CONFIG_INVALID`，退出码 2）。
- 启动后先看三眼：

```bash
opsctl health     # daemon/database/workers/uptime 四项
opsctl status     # 健康四项 + 最近操作列表（pending/running 标「进行中」）
opsctl identity   # 对端身份（主机名/版本/backend）+ 本次连接身份（CN/scope/应用白名单）
```

- `opsctl version` 打印客户端构建版本（模块版本 + VCS 修订），**默认不连 opsd、退出码恒 0**
  ——查版本经常正是「环境已经坏了」时做的第一件事，它不该再依赖一个可能连不上的守护进程。
  加 `--daemon` 才连上去追加对端版本，连不上也以 0 退出、只多打一行标注。
- 常用环境变量：`OPSD_SOCKET`、`OPSD_CLIENT_CERT`、`OPSD_CLIENT_KEY`、`OPSD_CA_CERT`。
- 每条命令都支持 `--json` 输出与 API 逐字段对应的原始 JSON，便于脚本消费。

## 4. 第一个部署（单机）

一次部署 = **上传制品 → 建应用 → 提交 manifest → 登记版本 → 部署**。manifest 是应用的
「当前规格」，部署时按它物化 release 目录、写 systemd unit、切换 `current` 指针、启动并
等就绪。**健康探测通过才算部署成功**；任何一步失败都会把上一个稳定版本放回去，并以
`DEPLOY_ROLLED_BACK`（退出码 29）收场。同一版本重复部署是幂等的：已是当前版本时什么都不做。

```bash
# 1. 上传制品（内容寻址，返回 art_xxx 与 sha256:<hex>）
opsctl artifact put ./orders-api.tar.gz --name orders-api

# 2. 创建应用
opsctl app create orders-api --label env=prod

# 3. 提交 manifest（见下方示例）
opsctl spec put --app orders-api --file app.yaml

# 4. 把制品登记为应用的版本 v1.0.0
opsctl release add --app orders-api --artifact sha256:abcd... --version v1.0.0

# 5. 部署（异步 Operation）
opsctl app deploy --app orders-api --file app.yaml

# 6. 跟日志到终态（成功退出 0；失败按退出码表）
opsctl operation logs <operation-id> --follow
```

单槽应用 manifest（字段与 e2e 夹具同构）：

```yaml
apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_xxx                    # 单机部署用 id；多主机必须改用 digest（见第 6 节）
  fileName: bin/app              # 制品包内的相对路径
  unpack:
    strategy: none               # 归档解包策略
exec:
  argv: [bin/app]                # 只接受 argv，永不经过 shell
  workingDirectory: /var/lib/orders-api
  runUser: orders-api            # 运行用户（opsd 创建）
logs:
  directory: /var/log/orders-api
health:
  readiness:
    type: tcp                    # 或 http
    target: "127.0.0.1:8080"
```

回滚**连配置一起回滚**：每个 release 都记着它当时那份 manifest，回滚用的是它，不会出现
「旧二进制配新配置」：

```bash
opsctl app rollback --app orders-api            # 回到上一个曾经激活过的版本
opsctl app rollback --app orders-api --to v1.0.0
```

日常查询：`app list` / `app inspect` / `release list --app` / `release inspect <release-id>` /
`spec show --app`。

### 4.1 手动运行时操作（不部署也能用）

`app deploy` 内部就是校验 → 准备 → 启停 → 等就绪；排查或演示时可以拆开单独打。
`runtime start` / `stop` 走 opsd 的 Operation（`kind=runtime.start` / `runtime.stop`），
因此 `operation get/logs/cancel/retry` 对它们同样适用；`validate` / `prepare` / `health`
是同步调用，立即返回结果。

```bash
opsctl runtime validate --app orders-api   # 只校验 manifest 能否被本机适配器执行，无副作用
opsctl runtime prepare --app orders-api    # 建运行用户/目录/环境文件/unit，幂等可重复
opsctl runtime start --app orders-api      # 先幂等 prepare，再启动（Operation）
opsctl runtime stop --app orders-api       # 停止（Operation）
opsctl runtime health --app orders-api     # 已就绪退出 0；未就绪退出 19（RUNTIME_NOT_READY）
```

- `--slot blue|green`：**蓝绿应用必填、单槽应用不能给**，给错直接拒绝；`validate` 不接受
  `--slot`（它只看 manifest）。
- `runtime health` 未就绪时退出码 `19` 就是 `RUNTIME_NOT_READY`，脚本可以直接用它判断，
  不必解析输出。

## 5. 蓝绿发布

manifest 里声明 `exec.slots`（blue/green 两个槽位、各自端口与就绪探测）与 `nginx.listen`，
应用即成为蓝绿应用：`app deploy` 部署到**空闲槽位**，就绪后由 opsd 改 Nginx upstream 切流；
`app rollback` 切回另一侧。

```yaml
exec:
  argv: [bin/app]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
  slots:
    blue:
      ports: [18081]
      readiness:
        type: tcp
        target: "127.0.0.1:18081"
        consecutiveSuccesses: 1
    green:
      ports: [18082]
      readiness:
        type: tcp
        target: "127.0.0.1:18082"
        consecutiveSuccesses: 1
health:
  startTimeoutSeconds: 30
logs:
  directory: /var/log/orders-api
nginx:
  listen: 8080
```

> 前提：主机上的 Nginx 已把 `include` 指向本工具的受管配置目录——首次部署前的这一步
> 是运维手工做的（工具会明确报错提示）。

```bash
opsctl app slot list --app orders-api     # 两类事实并排：库里的运营记录 + 线上实际（upstream 指向、进程在不在）
opsctl app slot history --app orders-api  # 切换时间线：谁在什么时候切到过哪个版本
```

`slot list` 刻意把「库说 A、线上是 B」的不一致标出来——那本身就是要看的信息（对账）。
单槽应用没有槽位可看，这两条命令会明确报错。

## 6. 多主机批量发布

批量发布的编排在 **opsctl 侧**（`internal/fleet`）：每台主机上的 opsd 是它那次部署的唯一
事实来源，批次只是一个视图——没有「批次表」，**批次号就是每台机上的幂等键**：
「继续」= 同批次号重跑（跳过已完成的机器），「重来」= 换一个批次号。

### 6.1 主机记录与环境记录

`--host` 与批量目标都建立在**主机记录**上：

```bash
opsctl host create web-1 --address 10.0.0.11:9443 --label env=prod
opsctl host create control --label role=control    # 不给 --address 就是本机记录
opsctl host list                                   # 列出全部主机
opsctl host inspect web-1                          # 单台详情（地址、标签、创建时间）
opsctl host check web-1                            # 连过去对身份：对方主机名/版本/backend + 我方 CN/scope
opsctl host list --check                           # 逐台探活并汇总
```

- `address` 只对**填了地址的记录**有意义：`--host <name>` 与 `host check` 会真的拨号，
  因此它必须是 `host:port`；**地址为空的记录表示本机**，对它加 `--host` 会被明确拒绝
  （直接执行即可）。
- `host list --check` 的结果**不写库**：写进去就会出现「库里说可达、其实早就挂了」的
  第二份会过期的真相。
- **环境记录**（`opsctl env create <name> --label k=v` / `env list` / `env inspect <name-or-id>`）
  目前只是一张**带标签的注册表**：不参与部署选择、不做过滤、也不影响授权。别把它当成
  「部署到某个环境」的开关。

### 6.2 批量发布

```bash
# 0. 目标主机的 opsd 已装好并开了远程监听（见第 7 节）
# 1. 登记：本机 opsd 的主机表 + 应用的部署目标（声明的意图，不是探活结果）
opsctl host create web-1 --address 10.0.0.11:9443
opsctl host create web-2 --address 10.0.0.12:9443
opsctl app target set --app orders-api --hosts web-1,web-2
opsctl app target list --app orders-api   # 不带 --app 就跨主机汇总列出全部应用的部署目标

# 2. 批量部署：@应用名 = 用登记的部署目标展开
opsctl app deploy --app orders-api --file app.yaml --hosts @orders-api \
    --batch-size 5 --concurrency 5 --batch rel-20260930-01

# 3. 批量回滚（注意：--hosts 是批量目标，--to 是版本，两个旗标刻意不同名）
opsctl app rollback --app orders-api --to v0.9.0 --hosts @orders-api
```

三条必须理解的规则：

1. **准备阶段先行**：任何一台部署之前，先对所有目标做完可达性 / 应用已登记 / 制品在不在
   的检查。默认**准备阶段不过就一台都不动**（`BATCH_PREFLIGHT_FAILED`，退出码 36）；
   `--allow-partial` 才只发其余主机（没过的标 `skipped` 并说明原因）。
2. **汇总恒等式**：成功 + 失败 + 跳过 + 未执行 = 总数，每一台都必须有结局。批次结束时
   逐台给结论并汇总；默认一波失败即停（`BATCH_FAILED`，退出码 37），`--keep-going` 才
   继续下一波。
3. **`--hosts` 模式要求 manifest 按 `artifact.digest` 引用制品**：`art_xxx` 只在某台机上
   有意义，digest 是内容寻址的、在每台机上指同一份字节。这一条在提交任何东西之前被拒。
   配合 `--artifact <本地文件>` 可在准备阶段把缺制品的机器补齐（上传是幂等的）。

其余旗标：`--pause`（波间等待）、`--concurrency`（波内并发）、`--created-by`、
`--idempotency-key`、`--retry-*`（见 `opsctl app deploy --help`）。

## 7. 远程连接（mTLS）

**默认形态是关的**：配置里没有 `remote.listen` 就完全不听 TCP，只有 Unix socket。

### 7.1 证书准备（运维手工做，工具只消费、不签发、不轮换）

```bash
# CA
openssl genrsa -out ca.key 2048 && openssl req -x509 -new -key ca.key -out ca.crt -subj "/CN=frz-ops-ca"
# 服务端证书：SAN 必须包含客户端用来连它的地址（IP 用 IP:，域名用 DNS:）
openssl req -new -key server.key -out server.csr -subj "/CN=web-1"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -out server.crt \
    -extfile <(printf "subjectAltName=IP:10.0.0.11\nextendedKeyUsage=serverAuth")
# 客户端证书：CN 会成为身份
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -out client.crt \
    -extfile <(printf "extendedKeyUsage=clientAuth")
```

服务端私钥模式必须 ≤ `0600` 且属主为 opsd 的运行用户——过宽直接拒绝启动。

### 7.2 服务端配置与授权模型

```yaml
remote:
  listen: "0.0.0.0:9443"
  certFile: /etc/opsd/pki/server.crt
  keyFile: /etc/opsd/pki/server.key
  clientCAFile: /etc/opsd/pki/ca.crt
  clients:
    - cn: opsctl-central
      scope: write                      # read 只能打只读端点；write 是全部
      applications: [orders-api]        # 省略 = 这台机上的全部应用
    - cn: opsctl-readonly
      scope: read
```

- `listen` 为空时，这一段的其他字段**都不许给**（配了但不听是最容易误读的状态）。
- **握手只验 CA，授权在 HTTP 层**：「证书没签对」与「身份没被授权」是两种不同的错误
  （`HOST_TLS_FAILED`/34 与 `REMOTE_FORBIDDEN`/35），分得清该改哪一边。
- **默认拒绝**：CN 不在 `clients` 名单里一律拒绝；授权只到 `read`/`write` 两个档位加
  应用白名单（更细的 RBAC 是迭代 5c 的事，未实现）。
- 审计的「谁做的」：远程请求记证书 CN（本地 socket 请求记请求体里的 `createdBy`）。

### 7.3 客户端

```bash
opsctl --remote 10.0.0.11:9443 \
       --ca-cert ca.crt --client-cert client.crt --client-key client.key \
       health
opsctl --host web-1 status          # 地址与身份从本机库里的 host 记录读
opsctl host check web-1             # 连过去对身份：对方主机名/版本/backend + 我方 CN/scope
opsctl host list --check            # 逐台探活并汇总（结果刻意不落库）
```

## 8. 备份、恢复与保留

备份策略声明「备份什么、怎么编码、保留多久」；`run`/`verify`/`restore` 走 opsd 的
Operation（异步），因此 `operation get/logs/cancel/retry` 对它们同样适用。

```bash
opsctl backup policy put --file policy.yaml      # 提交或覆盖（kind: BackupPolicy）
opsctl backup policy list                        # 列出全部策略
opsctl backup policy get nightly-files           # 查看一份策略（回看它声明的保留规则）
opsctl backup run --policy nightly-files         # 触发一次（异步）
opsctl backup list --policy nightly-files
opsctl backup show <id>
opsctl backup verify <id>                        # 重算流自洽性，不接触目标资源
```

策略文件（files 资源；数据库资源 `resource.kind` 取 `postgres` 或 `mysql`）：

```yaml
apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: nightly-files
resource:
  kind: files
  paths:
    - /var/lib/orders-api
encoding:
  compression: gzip
  encryption:
    enabled: false
retention:
  keepLast: 3          # 或 keepDays；两者都是保守并集
```

```bash
# 恢复：isolated（默认）= 恢复到临时目录/临时实例，完成后销毁，不碰真实数据
opsctl backup restore <id>
# inPlace = 覆盖真实数据，破坏性最强，必须显式确认
opsctl backup restore <id> --mode inPlace --confirm

# 保留清理：只支持 keepLast/keepDays（GFS 未实现，声明了它的策略在提交期就被拒）
opsctl backup prune --policy nightly-files --dry-run
opsctl backup prune --policy nightly-files
```

只有已成功**且**校验没失败的备份会进入清理范围；正在被恢复或校验的会被跳过；内容仍被其它
备份引用的只标记元数据、不删内容。数据库隔离恢复对备份账号有额外权限要求（不只是
CREATEDB），详见 `docs/plans/2026-09-21-iteration-2.md` 第 21 节。

## 9. 定时计划

到点由 **opsd** 自动创建并执行操作（不是 opsctl 醒着才行）：

```bash
opsctl schedule create --name nightly-backup --resource orders-api-files \
    --cron "0 2 * * *" --timezone Asia/Shanghai --operation-kind backup.run \
    --spec '{"policy":"nightly-files"}'

opsctl schedule create --name disk-check --resource disk \
    --interval 30m -- ./checkdisk.sh          # executor.command 类：-- 后跟 argv

opsctl schedule list                          # 列出全部计划
opsctl schedule inspect nightly-backup        # 单个计划详情
opsctl schedule runs nightly-backup           # 运行历史
opsctl schedule enable nightly-backup         # 重新启用；下次触发时间从当前时刻重算
opsctl schedule disable nightly-backup        # 停用
opsctl schedule delete nightly-backup         # 删除（启用中的必须先 disable）
```

- `--missed-run-policy`：错过的到点怎么处理——`skip`（默认，丢弃）或 `runOnce`（只补跑
  最近一次）。
- `--operation-kind`：到点创建哪种操作，默认 `executor.command`；也接受 `backup.run`、
  `backup.verify`、`runtime.start` 等。
- 凭据同样经 `--secret-env` 注入（见第 10 节），明文不进计划定义与日志。

## 10. 制品与凭据

**制品是内容寻址的**：上传时重算 sha256、按摘要落盘，`verify` 从存储重算并与记录比对。
上传时可给 `--sha256 sha256:<hex>` 声明预期值，不符即拒（`ARTIFACT_CHECKSUM_MISMATCH`）。

```bash
opsctl artifact put ./app.tar.gz --sha256 sha256:abcd...
opsctl artifact list                                  # 列出制品（--cursor / --limit 翻页）
opsctl artifact inspect <artifact-id|digest>
opsctl artifact verify <id>                           # 从存储重算摘要并与记录比对
opsctl artifact download <id> --output ./app.tar.gz   # 校验通过才落盘，不会写半截内容
opsctl artifact gc --older-than 720h --dry-run        # 清理未被 Release 引用的制品
opsctl artifact delete <id>                           # 被 Release 引用时会被拒绝
```

**凭据注入**（`operation submit` 与 `schedule create`）：

```bash
opsctl operation submit --kind executor.command --resource orders-api \
    --secret-env DB_PASSWORD=env:MY_DB_PASSWORD --secret-env TLS_KEY=file:/etc/opsd/secrets/tls.key \
    -- ./deploy.sh
```

- `VAR=env:NAME`：从 opsd 进程环境取；`VAR=file:/path`：从文件读（路径必须在
  `secrets.allowedFileDirectories` 内、且文件不对属组或其他用户开放）。
- 明文不进请求体、不进日志；日志里只出现凭据的长度与 sha256。

## 11. 观察与排障

**观察命令**：

```bash
opsctl status                              # 健康四项 + 最近操作（进行中标出）
opsctl operation get <id>                  # 状态/阶段/attempt x/y/错误码
opsctl operation logs <id> --follow        # 跟日志到终态，退出码随终态
opsctl operation cancel <id>               # 取消待执行或运行中的操作
opsctl operation retry <id>                # 失败/已取消的操作重建一个执行
```

**退出码表**（`api/v1/errors.go` 为权威；CLI 错误输出形如
`opsctl: LOCK_BUSY: <中文消息>`，码即下表第一列）：

| 退出码 | 错误码 | 场景 |
| --- | --- | --- |
| 0 | — | 成功 |
| 1 | `INTERNAL` | 内部错误 / 未映射 |
| 2 | `CONFIG_INVALID` `INVALID_REQUEST` `*_NOT_FOUND` | 配置不合法；参数错误；各类记录不存在（OPERATION/ARTIFACT/APPLICATION/RELEASE/SCHEDULE/SPEC/HOST/ENVIRONMENT/BACKUP） |
| 3 | `IDEMPOTENCY_CONFLICT` | 相同幂等键但请求不同 |
| 4 | `LOCK_BUSY` | 同一 resource 已有未完成操作 |
| 5 | `ARTIFACT_CHECKSUM_MISMATCH` | 摘要不符（上传前声明校验或下载校验） |
| 6 | `ARTIFACT_IN_USE` | 制品被 Release 引用 |
| 7 / 8 | `UPLOAD_TOO_LARGE` / `STORAGE_QUOTA_EXCEEDED` | 超过单文件上限 / 存储配额 |
| 9 | `SECRET_UNRESOLVED` | 凭据解析失败 |
| 10–12 | `EXEC_TIMEOUT` / `EXEC_CANCELLED` / `EXEC_EXIT_NONZERO` | 执行器：超时 / 取消 / 退出码非零 |
| 13 | `DAEMON_RESTARTED` | daemon 重启导致操作中断 |
| 14 | `RELEASE_CONFLICT` | 版本冲突（如重复登记） |
| 15 / 16 | `SCHEDULE_ENABLED` / `SCHEDULE_INVALID` | 计划停用才能删 / 计划定义不合法 |
| 17 / 18 | `MANIFEST_INVALID` / `MANIFEST_CONFLICT` | manifest 不合法 / 冲突 |
| 19 | `RUNTIME_NOT_READY` | 应用未就绪（`runtime health` 的脚本判据） |
| 20 | `PERMISSION_DENIED` | 权限拒绝 |
| 21 | `RUNTIME_UNSUPPORTED` | 非 Linux 平台调用运行时语义 |
| 22 | `RETRY_POLICY_INVALID` | 重试参数不合法 |
| 23–28 | `BACKUP_*` | 备份预检失败 / 校验失败 / 原地恢复未确认 / 备份占用中 / 密钥未解析 / 恢复失败 |
| 29 | `DEPLOY_ROLLED_BACK` | 部署失败已自动回滚（看日志找根因） |
| 30 | `ARTIFACT_UNPACK_FAILED` | 制品解包失败 |
| 31 / 32 | `NGINX_CONFIG_INVALID` / `NGINX_RELOAD_FAILED` | 蓝绿：受管配置不合法 / reload 失败 |
| 33 | `HOST_UNREACHABLE` | 主机连不上 |
| 34 | `HOST_TLS_FAILED` | TLS 握手失败（证书没签对/过期/不受信） |
| 35 | `REMOTE_FORBIDDEN` | 身份未被授权（CN 不在名单 / scope 不够 / 应用不在白名单） |
| 36 | `BATCH_PREFLIGHT_FAILED` | 批量：准备阶段没过，**一台都没动** |
| 37 | `BATCH_FAILED` | 批量：跑完了但有主机没成功（线上混合版本，去看逐台结论） |

**常见症状对照**：

- `opsctl: CONFIG_INVALID: ...` → 配置逐条消息里写的是哪个字段；`opsctl config validate --file` 本地复现。
- `LOCK_BUSY` → `opsctl status` 看谁在跑（或直接看只读端点 `GET /api/v1/operations`）；
  同资源天然串行，不是故障。
- `DEPLOY_ROLLED_BACK` → 部署已自动回滚到上一稳定版本，`operation logs <id>` 找根因
  （常见：就绪探测超时、端口占用、Nginx include 未加）。
- `RUNTIME_NOT_READY`（退出码 19）→ 应用没通过就绪探测；`opsctl runtime health --app <name>`
  可复现，配合 `runtime prepare --app <name>` 看 unit/目录状态与日志。
- `HOST_UNREACHABLE` / `HOST_TLS_FAILED` / `REMOTE_FORBIDDEN` 三态 → 分别是网络、证书、
  授权问题；`opsctl identity --remote ...` 能看到本次连接被认成了谁。
- `BATCH_FAILED` → 批次报告逐台结论里找 `failed` 的机器与原因；修复后**同批次号重跑**
  只会补没完成的机器。

## 12. 配置参考

完整骨架直接看 `opsd.example.yaml`（与 `opsctl init` 生成的模板键集一致，有测试钉住）。
各段一句话：

| 段 | 说明 |
| --- | --- |
| `socket` | Unix socket 路径与八进制模式；默认 `/run/opsd/opsd.sock` |
| `database` | opsd 自己的 SQLite 路径（绝对路径必填） |
| `runtime` | 工作目录、日志目录、worker 数、关机宽限 |
| `execution` | 默认超时、输出上限、`allowedPaths`（执行器只允许这些路径下的可执行文件）、`sensitiveEnvKeys`（脱敏清单） |
| `artifactStore` | 制品存储根目录、文件/目录模式、单文件上限与配额；不配即禁用制品端点 |
| `secrets` | `allowedFileDirectories`：`file:` 类凭据允许引用的目录 |
| `backupStore` | 备份文件落盘位置与保留上限 |
| `nginx` | 蓝绿：受管配置目录与 include 约定 |
| `sudo` | **已声明、没有任何行为**（1c 遗留，未实现），不构成安全控制 |
| `remote` | mTLS 监听与 CN 白名单（见第 7 节）；整段不配 = 不听 TCP |

校验规则要点：四个路径字段必须绝对路径；socket.mode 是八进制；`remote.listen` 为空时
remote 段其他字段一律不许给；私钥文件模式过宽拒绝启动。

## 13. 安全模型与边界

- opsd 建议以 root（或等价权限的服务账号）运行；执行器只接受 argv、永不经过 shell；
  写操作全部走审计，本地记 `createdBy`、远程记证书 CN。
- 默认形态只有 Unix socket，访问控制是 socket 文件权限一层；开远程后是 mTLS +
  默认拒绝的 CN 白名单。
- **明确不做**（引用结论前先看 AGENTS.md 的「验证约定」）：证书签发与轮换、CRL/OCSP、
  SELinux 加固、sudo 执行路径（`sudo` 段无行为）、RBAC/审批/操作签名（迭代 5c 范围）。
- 本手册描述的功能中，**批量发布在多台真实主机上的端到端行为尚未取得真机证据**
  （e2e 覆盖的是同机多实例的编排与协议接线）；大规模并发未压测。

## 附录 A：命令速查

> 本附录是 `opsctl --help` 的**快照**，用于快速定位命令与旗标；取值、默认值与完整语义
> 一律以 `opsctl <命令> --help` 为准（正文与该输出同源）。重新生成：
> `python3 test/manual/gen_command_table.py`（先从仓库根 `make build`，或用 `--binary` 指路径）。

### A.1 全部命令

| 命令 | 说明 |
| --- | --- |
| `opsctl app` | **分组**：管理应用 |
| `opsctl app slot` | **分组**：查看蓝绿应用的槽位与切换时间线 |
| `opsctl app target` | **分组**：管理应用的部署目标：这个应用应该跑在哪些主机上 |
| `opsctl artifact` | **分组**：上传、查询、校验与清理制品 |
| `opsctl backup` | **分组**：管理备份策略，触发、校验与恢复备份 |
| `opsctl backup policy` | **分组**：提交、查看备份策略 |
| `opsctl config` | **分组**：操作 opsd 的配置文件 |
| `opsctl env` | **分组**：管理环境记录：1c 只建身份与标签 |
| `opsctl host` | **分组**：管理主机记录：1c 只建身份与标签，地址为空表示本机 |
| `opsctl operation` | **分组**：提交、查询、取消、重试操作并查看日志 |
| `opsctl release` | **分组**：登记与查询发布记录 |
| `opsctl runtime` | **分组**：校验、准备、启停应用的运行时并检查就绪状态 |
| `opsctl schedule` | **分组**：管理定时计划：到点由 opsd 自动创建并执行操作 |
| `opsctl spec` | **分组**：提交与查看应用的 manifest（应用当前规格） |
| | |
| `opsctl app create <name> [flags]` | 创建应用 |
| `opsctl app deploy --app <name> --file <manifest.yaml> [--hosts <host...>] [flags]` | 部署一个版本：物化制品、准备运行时、切换并启动 |
| `opsctl app inspect <name-or-id> [flags]` | 查看单个应用 |
| `opsctl app list [flags]` | 列出应用 |
| `opsctl app rollback --app <name> [--to <version>] [--hosts <host...>] [flags]` | 回滚到上一个（或指定的）版本 |
| `opsctl app slot history --app <name> [flags]` | 查看切换时间线（新的在前） |
| `opsctl app slot list --app <name> [flags]` | 列出两个槽位的状态、版本、端口与是否在接流量 |
| `opsctl app target list [--app <name>] [flags]` | 列出部署目标；不给 --app 时列出全部应用（跨主机汇总） |
| `opsctl app target set --app <name> --hosts <name,name...> [flags]` | 替换某个应用的部署目标列表 |
| `opsctl artifact delete <artifact-id|digest> [flags]` | 删除制品；被 Release 引用时会被拒绝 |
| `opsctl artifact download <artifact-id|digest> --output <path> [flags]` | 下载制品内容到本地文件 |
| `opsctl artifact gc [flags]` | 清理超出保留策略且未被引用的制品 |
| `opsctl artifact inspect <artifact-id|digest> [flags]` | 查看单个制品 |
| `opsctl artifact list [flags]` | 列出制品 |
| `opsctl artifact put <path> [flags]` | 上传一个制品文件 |
| `opsctl artifact verify <artifact-id|digest> [flags]` | 从存储重算摘要并核对记录 |
| `opsctl backup list [flags]` | 列出备份记录（默认最近 100 条） |
| `opsctl backup policy get <name> [flags]` | 查看一份备份策略 |
| `opsctl backup policy list [flags]` | 列出全部备份策略 |
| `opsctl backup policy put --file <manifest.yaml> [flags]` | 提交或覆盖一份备份策略（kind: BackupPolicy） |
| `opsctl backup prune --policy <name> [--dry-run] [flags]` | 按策略声明的保留规则清理备份 |
| `opsctl backup restore <id> --mode isolated|inPlace [--confirm] [flags]` | 恢复一份备份（默认只允许隔离恢复） |
| `opsctl backup run --policy <name> [flags]` | 触发一次备份（返回 Operation，异步执行） |
| `opsctl backup show <id> [flags]` | 查看一条备份记录的详情 |
| `opsctl backup verify <id> [flags]` | 校验一份备份的流是否自洽（不接触目标资源） |
| `opsctl config validate --file <path> [flags]` | 校验一个 opsd 配置文件，不连接 opsd |
| `opsctl env create <name> [flags]` | 创建环境记录 |
| `opsctl env inspect <name-or-id> [flags]` | 查看单个环境 |
| `opsctl env list [flags]` | 列出环境 |
| `opsctl health [flags]` | 检查 opsd 及其数据库是否可达 |
| `opsctl host check <name-or-id> [flags]` | 连到那台主机并报出它的身份与本次连接的身份 |
| `opsctl host create <name> [flags]` | 创建主机记录 |
| `opsctl host inspect <name-or-id> [flags]` | 查看单个主机 |
| `opsctl host list [flags]` | 列出主机 |
| `opsctl identity [flags]` | 查看对端 opsd 的身份与本次连接的身份 |
| `opsctl init <path> [flags]` | 生成一份 opsd 配置骨架到指定路径，不连接 opsd |
| `opsctl operation cancel <operation-id> [flags]` | 取消一个待执行或运行中的操作 |
| `opsctl operation get <operation-id> [flags]` | 查看单个操作 |
| `opsctl operation logs <operation-id> [flags]` | 打印操作的结构化日志 |
| `opsctl operation retry <operation-id> [flags]` | 把失败或已取消的操作重新建一个操作执行 |
| `opsctl operation submit --kind <kind> --resource <name> [--dry-run] -- <argv...> [flags]` | 提交一个操作交由 opsd 执行 |
| `opsctl release add --app <name-or-id> --artifact <id-or-digest> --version <v> [flags]` | 把制品登记为应用的一个版本 |
| `opsctl release inspect <release-id> [flags]` | 查看单个发布记录 |
| `opsctl release list --app <name-or-id> [flags]` | 列出应用的发布记录 |
| `opsctl runtime health --app <name> [flags]` | 查询应用是否已就绪；未就绪时退出码为 19（RUNTIME_NOT_READY） |
| `opsctl runtime prepare --app <name> [flags]` | 创建运行用户、目录、环境文件与 unit（幂等，可重复执行） |
| `opsctl runtime start --app <name> [flags]` | 启动应用：先幂等准备，再启动（走 Operation，可用 operation 命令查询） |
| `opsctl runtime stop --app <name> [flags]` | 停止应用（走 Operation，可用 operation 命令查询） |
| `opsctl runtime validate --app <name> [flags]` | 只校验应用的 manifest 能否被本机适配器执行，不产生副作用 |
| `opsctl schedule create --name <name> --resource <resource> (--cron <expr> | --interval <duration>) [--operation-kind <kind>] -- <argv...> [flags]` | 创建定时计划 |
| `opsctl schedule delete <name-or-id> [flags]` | 删除计划；启用中的计划必须先停用 |
| `opsctl schedule disable <name-or-id> [flags]` | 停用计划；已在运行的 Operation 不受影响 |
| `opsctl schedule enable <name-or-id> [flags]` | 启用计划；下一次触发时间从当前时刻重新计算 |
| `opsctl schedule inspect <name-or-id> [flags]` | 查看单个计划 |
| `opsctl schedule list [flags]` | 列出计划 |
| `opsctl schedule runs <name-or-id> [flags]` | 查看计划的运行历史 |
| `opsctl spec put --app <name> --file <manifest.yaml> [flags]` | 提交应用的 manifest，替换上一次提交的规格 |
| `opsctl spec show --app <name> [flags]` | 查看应用当前的 manifest |
| `opsctl status [flags]` | 一屏查看 opsd 的健康与最近操作 |
| `opsctl version [flags]` | 打印 opsctl 的构建版本 |

另有 cobra 提供的 `opsctl completion <bash\|zsh\|fish\|powershell>`（生成补全脚本）与 `opsctl help [命令]`（查看帮助），不在上表。

### A.2 各命令自有旗标

只列旗标名，取值与语义看 `opsctl <命令> --help`；全局旗标见 A.3。

| 命令 | 自有旗标 |
| --- | --- |
| `opsctl app create` | `--label` |
| `opsctl app deploy` | `--allow-partial` `--app` `--artifact` `--batch` `--batch-size` `--concurrency` `--created-by` `--file` `--hosts` `--idempotency-key` `--keep-going` `--pause` `--retry-base` `--retry-max` `--retry-max-delay` |
| `opsctl app inspect` | — |
| `opsctl app list` | — |
| `opsctl app rollback` | `--allow-partial` `--app` `--artifact` `--batch` `--batch-size` `--concurrency` `--created-by` `--hosts` `--idempotency-key` `--keep-going` `--pause` `--retry-base` `--retry-max` `--retry-max-delay` `--to` |
| `opsctl app slot history` | `--app` `--limit` |
| `opsctl app slot list` | `--app` |
| `opsctl app target list` | `--app` |
| `opsctl app target set` | `--app` `--hosts` |
| `opsctl artifact delete` | — |
| `opsctl artifact download` | `--output` |
| `opsctl artifact gc` | `--dry-run` `--keep` `--older-than` |
| `opsctl artifact inspect` | — |
| `opsctl artifact list` | `--cursor` `--limit` |
| `opsctl artifact put` | `--created-by` `--media-type` `--name` `--sha256` |
| `opsctl artifact verify` | — |
| `opsctl backup list` | `--limit` `--policy` |
| `opsctl backup policy get` | — |
| `opsctl backup policy list` | — |
| `opsctl backup policy put` | `--file` |
| `opsctl backup prune` | `--dry-run` `--policy` |
| `opsctl backup restore` | `--confirm` `--created-by` `--idempotency-key` `--mode` `--retry-base` `--retry-max` `--retry-max-delay` |
| `opsctl backup run` | `--created-by` `--idempotency-key` `--policy` `--retry-base` `--retry-max` `--retry-max-delay` |
| `opsctl backup show` | — |
| `opsctl backup verify` | `--created-by` `--idempotency-key` `--retry-base` `--retry-max` `--retry-max-delay` |
| `opsctl config validate` | `--file` |
| `opsctl env create` | `--label` |
| `opsctl env inspect` | — |
| `opsctl env list` | — |
| `opsctl health` | — |
| `opsctl host check` | — |
| `opsctl host create` | `--address` `--label` |
| `opsctl host inspect` | — |
| `opsctl host list` | `--check` |
| `opsctl identity` | — |
| `opsctl init` | `--force` `--remote` |
| `opsctl operation cancel` | — |
| `opsctl operation get` | — |
| `opsctl operation logs` | `--cursor` `--follow` `--limit` |
| `opsctl operation retry` | — |
| `opsctl operation submit` | `--dry-run` `--idempotency-key` `--kind` `--resource` `--retry-base` `--retry-max` `--retry-max-delay` `--secret-env` |
| `opsctl release add` | `--app` `--artifact` `--created-by` `--label` `--version` |
| `opsctl release inspect` | — |
| `opsctl release list` | `--app` `--limit` |
| `opsctl runtime health` | `--app` `--slot` |
| `opsctl runtime prepare` | `--app` `--slot` |
| `opsctl runtime start` | `--app` `--created-by` `--idempotency-key` `--retry-base` `--retry-max` `--retry-max-delay` `--slot` |
| `opsctl runtime stop` | `--app` `--created-by` `--idempotency-key` `--retry-base` `--retry-max` `--retry-max-delay` `--slot` |
| `opsctl runtime validate` | `--app` |
| `opsctl schedule create` | `--created-by` `--cron` `--interval` `--missed-run-policy` `--name` `--operation-kind` `--resource` `--secret-env` `--spec` `--timezone` |
| `opsctl schedule delete` | — |
| `opsctl schedule disable` | — |
| `opsctl schedule enable` | — |
| `opsctl schedule inspect` | — |
| `opsctl schedule list` | `--limit` |
| `opsctl schedule runs` | `--limit` |
| `opsctl spec put` | `--app` `--file` `--updated-by` |
| `opsctl spec show` | `--app` |
| `opsctl status` | — |
| `opsctl version` | `--daemon` |

`opsctl app`、`opsctl backup policy`、`opsctl app slot`、`opsctl app target` 等分组命令自身没有旗标。

### A.3 全局旗标

每条命令都接受下列全局旗标（`opsctl --help` 末尾的「全局选项」）。

| 旗标 | 说明 |
| --- | --- |
| `--socket string` | opsd 的 unix socket 路径；默认 `/run/opsd/opsd.sock`，也可用环境变量 `OPSD_SOCKET` |
| `--host string` | 打到本机库里登记的那台主机上（地址从记录里读） |
| `--remote string` | 直接打到这个 `host:port` 上的 opsd（不查库，排障用） |
| `--client-cert string` | mTLS 客户端证书路径；也可用环境变量 `OPSD_CLIENT_CERT` |
| `--client-key string` | mTLS 客户端私钥路径，模式必须不宽于 `0600`；也可用环境变量 `OPSD_CLIENT_KEY` |
| `--ca-cert string` | 校验对端证书用的 CA 路径；也可用环境变量 `OPSD_CA_CERT` |
| `--json` | 输出原始 JSON 而不是人类可读文本 |
| `--timeout duration` | HTTP 客户端超时时间，默认 `30s` |

`--socket` / `--host` / `--remote` **互斥**，同时给会被拒绝（宁可直接报错，也不静默打到本机）。
不给任何目标就是本机 socket。

几个跨命令通用的旗标语义（帮助文本里逐条都有，这里只讲容易踩的）：

- `--idempotency-key`：相同键 + 相同请求重复提交返回**同一个**操作；键相同但请求不同报
  `IDEMPOTENCY_CONFLICT`（退出码 3）。
- `--created-by`：写进审计的调用方标识；远程连接时它会被证书 CN 覆盖（谁做的由 opsd 定，
  不由请求体自报）。
- `--retry-max` / `--retry-base` / `--retry-max-delay`：**一个都不给就是不自动重试**；
  给了 `--retry-max` 才启用退避。`--retry-max 1` 等价于不重试。
- `--dry-run`：只做校验与预演，不产生副作用（`operation submit`、`artifact gc`、
  `backup prune` 支持）。
- `--limit`：列表类命令的条数上限。需要翻页的是 `artifact list` 与 `operation logs`，
  它们另有 `--cursor`；`operation logs --follow` 就是基于 `--cursor` 的轮询循环。

### A.4 生成方式

附录 A.1 / A.2 由 `test/manual/gen_command_table.py` 解析 `opsctl --help` 生成（A.3 手写，
因为全局旗标只有一处、变动极少）。改命令或旗标后重新跑一次即可。
