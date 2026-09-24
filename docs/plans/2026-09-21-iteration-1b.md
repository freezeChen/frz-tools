# Linux 运维工具迭代 1b：调度器

> 文档日期：2026-09-22
> 文档状态：**已实现并提交**（2026-09-22 实现；验证记录见第 16 节，2026-09-23 回填状态行）
> 对应路线图：[2026-09-21-linux-ops-tool-roadmap.md](./2026-09-21-linux-ops-tool-roadmap.md)（第 4 节「调度器」）
> 前置迭代：[2026-09-21-iteration-1a.md](./2026-09-21-iteration-1a.md)（已实现并提交）

## 1. 定位与前置

调度器把「已登记的计划」变成「按时刻触发的 Operation」。它复用 1a 已经建立的能力——
resource 锁、Operation 状态机、审计与日志——**不自己执行任何命令**，只负责决定「现在该不该跑、
由哪个 Operation 跑、过去错过了什么」。

前置条件：

- 1a 的资源模型与制品管理已落地。
- `Operation` 的 `pending → running → {succeeded,failed,cancelled}` 语义已冻结，本迭代不改状态机。
- **需要用户确认的一项依赖决策**，见第 13 节第 1 条。

## 2. 范围

### 1b 实现

- 领域模型：`Schedule`、`ScheduleRun`。
- cron 表达式与固定间隔两种计划类型，带时区。
- 错过执行策略（skip / runOnce）与重启后的补偿判断。
- 持久化表 `schedules`、`schedule_runs`，migration `0003`。
- 调度循环：计算下一次触发时间，到点创建 Operation 并触发 worker。
- 每次触发写入 `ScheduleRun`，可查询运行历史与失败原因。
- API 端点与 CLI：`opsctl schedule create|list|inspect|runs|disable|enable`。

### 1b 不实现

- 分布式调度：只有本机 `opsd` 自己调度，多主机见迭代 5。
- systemd timer 单元：**已决定不生成**。调度权威归 `opsd` 的进程内调度循环，
  1c 只生成 `.service` 用于托管长期运行的进程，不生成 `.timer`，避免同一条计划被
  两套机制各触发一次（见第 13 节第 2 条）。
- 部署、备份、发布（属于后续迭代）。
- cron 的秒级字段、`L`/`#`/`W` 等扩展语法（见第 13 节）。

## 3. 领域模型

### 3.1 Schedule

| 字段 | 说明 |
| --- | --- |
| `id` | 前缀 `sch_` |
| `name` | 人类可读名称，唯一 |
| `enabled` | 是否启用 |
| `kind` | `cron` 或 `interval` |
| `cron` | cron 表达式（`kind=cron` 时必填，5 个字段） |
| `intervalSeconds` | 间隔秒数（`kind=interval` 时必填，≥ 60） |
| `timezone` | IANA 时区，例如 `Asia/Shanghai`；空则用 `UTC` |
| `resource` | 触发的 Operation 使用的 resource |
| `spec` | `executor.command` 的 spec，触发时原样写入 Operation |
| `missedRunPolicy` | `skip`（默认）/ `runOnce` |
| `nextRunAt` | 下一次应当触发的时间，由服务端计算后写回 |
| `lastRunAt` / `lastResult` | 上一次触发的结果摘要，便于列表页展示 |
| `createdAt` / `updatedAt` / `createdBy` | 审计信息 |

不变量：

- `kind=cron` 必须有合法 cron 表达式，`kind=interval` 必须有 ≥60 秒的间隔，二者互斥。
- `resource` 与 `spec` 在创建时就校验，复用 1a 的 `BuildCommandSpec`，
  避免到触发时才发现计划本身是坏的。
- 同一个 `resource` 允许多个计划，但执行仍由 resource 锁串行化。

### 3.2 ScheduleRun

| 字段 | 说明 |
| --- | --- |
| `id` | 前缀 `run_` |
| `scheduleId` | 归属计划 |
| `scheduledFor` | 该次触发对应的计划时刻（不是实际执行时刻） |
| `startedAt` / `finishedAt` | 实际开始与结束 |
| `result` | `dispatched` / `missed` / `failed` |
| `operationId` | 产生的 Operation；未触发时为空 |
| `errorCode` / `errorMessage` | 失败原因 |

`(scheduleId, scheduledFor)` **唯一**：这是防止重复触发的关键约束——`opsd` 重启后重算
触发时间时，已经为某个时刻建过记录就不再建一次。

## 4. cron 与间隔、时区

- cron 使用 5 个字段：`分 时 日 月 周`。
- **计划时刻先用计划自己的时区解释，再换算成 UTC 存储**，避免夏令时切换导致的漏跑或重复。
- `timezone` 必须能被 `time.LoadLocation` 解析，否则创建计划时直接返回 `CONFIG_INVALID`。
  运行时要注意容器内可能缺少 `tzdata`：Ubuntu 镜像已带，但 `scratch`/精简镜像没有——
  这会是 1c 镜像构建时必须处理的点，记在第 14 节。
- 间隔计划按「上次计划时刻 + interval」递推，而不是按「实际执行时刻 + interval」，
  否则一次执行耗时会把计划时间不断推后。

## 5. 错过执行与重启恢复

### 先区分「正常到点」和「停机积压」

调度器按精确时刻唤醒，正常情况下的延迟在毫秒级。但如果把「刚到期」和「停机期间积压」
一视同仁，`skip` 策略就会把自己本应执行的准点触发也丢掉。

因此引入**误触发宽限窗口**（`misfireThreshold`，实现取 1 分钟）：只有当**恰好一个**
到期时刻、且它的延迟不超过该窗口时，才算「正常到点」，此时无论策略是什么都执行；
否则整批按错过策略处理。

### 策略

只有两种取值（为什么没有 `runAll` 见第 13 节第 4 条）：

- `skip`：为每个漏掉的时刻写一条 `result=missed` 的 `ScheduleRun`，不建 Operation。
- `runOnce`：只补跑**最近漏掉的那一个**，更早的写 `missed`。

### 恢复流程

`opsd` 启动时（以及此后每次轮询）：

1. 载入所有 `enabled` 的计划。
2. 对 `nextRunAt` 已落在过去的计划，算出到 `now` 为止的所有计划时刻。
3. 按上面的规则判定「正常到点」还是「积压」，再按策略决定每个时刻是派发还是记为 `missed`。
4. 写回 `nextRunAt`（被截断时不继续追赶积压，从 `now` 之后重新开始），进入正常循环。

这与迭代 0 的 `DAEMON_RESTARTED` 恢复语义不同：那里恢复的是**已中断的 Operation**，
这里决定的是**尚未发生的计划是否补偿**。两者都要记录，不能互相替代。

## 6. 调度循环与并发

- 单个调度 goroutine：算出最近的 `nextRunAt`，用 `time.Timer` 精确等待，到点醒来。
  **不做每秒轮询**，避免常驻空转。
- 到点后：在事务内插入 `ScheduleRun`（靠唯一约束拒绝重复），插入 `pending` Operation，
  写审计，然后 `Pool.Notify()` 唤醒 worker。
- Operation 创建走既有的 `CreateOperation`，因此 resource 锁、幂等、`LOCK_BUSY` 语义完全不变：
  **如果资源此刻被占用，这次触发按 `LOCK_BUSY` 记为 `ScheduleRun.failed`，不排队等待**。
- 调度器本身不长事务、不持有数据库连接跨进程执行。

## 7. 持久化（migration `0003`）

```sql
CREATE TABLE schedules (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    kind              TEXT NOT NULL CHECK (kind IN ('cron', 'interval')),
    cron              TEXT,
    interval_seconds  INTEGER,
    timezone          TEXT NOT NULL,
    resource          TEXT NOT NULL,
    spec_json         TEXT NOT NULL,
    missed_run_policy TEXT NOT NULL DEFAULT 'skip'
        CHECK (missed_run_policy IN ('skip', 'runOnce')),
    next_run_at       TEXT,
    last_run_at       TEXT,
    last_result       TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    created_by        TEXT
);

CREATE TABLE schedule_runs (
    id            TEXT PRIMARY KEY,
    schedule_id   TEXT NOT NULL REFERENCES schedules (id),
    scheduled_for TEXT NOT NULL,
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    result        TEXT NOT NULL
        CHECK (result IN ('dispatched', 'missed', 'failed')),
    operation_id  TEXT REFERENCES operations (id),
    error_code    TEXT,
    error_message TEXT
);

CREATE UNIQUE INDEX ux_schedule_runs_schedule_moment
    ON schedule_runs (schedule_id, scheduled_for);
CREATE INDEX ix_schedule_runs_schedule ON schedule_runs (schedule_id, scheduled_for DESC);
```

## 8. API 与 CLI

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/schedules` | 创建计划，返回 `201` |
| `GET` | `/api/v1/schedules` | 列表，支持 `limit`/`cursor` |
| `GET` | `/api/v1/schedules/{id}` | 详情 |
| `POST` | `/api/v1/schedules/{id}/enable` | 启用 |
| `POST` | `/api/v1/schedules/{id}/disable` | 停用；不取消已在跑的 Operation |
| `DELETE` | `/api/v1/schedules/{id}` | 删除未启用的计划；启用中返回 `SCHEDULE_ENABLED` |
| `GET` | `/api/v1/schedules/{id}/runs` | 运行历史，支持 `cursor`/`limit` |

```text
opsctl schedule create --name <n> --resource <r> --cron "<表达式>"|--interval <dur>
                       --timezone <tz> --missed-run-policy skip|runOnce
                       -- <argv...>
opsctl schedule list
opsctl schedule inspect <id-or-name>
opsctl schedule runs <id-or-name> [--limit <n>]
opsctl schedule enable <id-or-name>
opsctl schedule disable <id-or-name>
opsctl schedule delete <id-or-name>
```

`create` 的 argv 与 `--secret-env` 复用 `operation submit` 的解析逻辑，保持两个入口一致。

## 9. 错误码与退出码增量

| 错误码 | HTTP | CLI 退出码 | 含义 |
| --- | ---: | ---: | --- |
| `SCHEDULE_NOT_FOUND` | 404 | 2 | 计划不存在 |
| `SCHEDULE_ENABLED` | 409 | 15 | 删除前必须先停用 |
| `SCHEDULE_INVALID` | 400 | 16 | cron 表达式、间隔、时区或 spec 非法 |

退出码 `15`/`16` 与迭代 0、1a 已占用的码不冲突。触发时资源被占用仍复用既有
`LOCK_BUSY`（退出码 4），不新增。

## 10. 兼容性与迁移影响

- migration `0003` 只 `CREATE TABLE`，不动既有表，可重复执行。
- 不修改 Operation 状态机、不修改 `opsd` 与 `opsctl` 的既有端点。
- 不启用计划时，本迭代的代码路径完全不被触达，迭代 0/1a 行为不变。

## 11. 测试计划

### 单元

- cron 解析：合法表达式、字段数量、非法表达式、跨时区同一表达式的下一个触发时间。
- 间隔递推：执行耗时不会把计划时间推后。
- 夏令时切换前后不漏跑、不重复。
- 错过策略两种行为，边界（准点、单个迟到、积压多个）。
- `(scheduleId, scheduledFor)` 唯一约束拒绝重复触发。
- 触发时 spec 非法在**创建计划**时就失败，而不是到点才失败。
- 资源被占用时该次触发记为 `failed` + `LOCK_BUSY`，不排队。

### 集成

- 启动 → 创建计划 → 到点 → Operation 产生 → 执行成功 → `ScheduleRun.result=dispatched`。
- `opsd` 重启后按策略补偿，且不会为同一时刻重复触发。
- 删除启用中的计划被拒绝；停用后可删。
- 迭代 0/1a 的回归用例继续全绿。

## 12. 验收标准

1. 创建 cron 与间隔计划各一个，到点各触发一次且只触发一次。
2. `opsd` 重启后不为已记录的时刻重复触发。
3. 两种错过执行策略行为符合定义，且「准点触发」不会被 skip 丢掉。
4. 同一时刻同一只计划只能产生一条 `ScheduleRun`（唯一约束）。
5. 计划 spec 非法在创建时即失败，错误码 `SCHEDULE_INVALID`。
6. 资源被占用时该次触发记为 `failed`/`LOCK_BUSY`，既有 Operation 不受影响。
7. 时区与夏令时切换不漏跑、不重复。
8. `make ci` 与 `make verify-linux` 全绿。
9. 迭代 0 与 1a 的回归用例全部继续通过。

## 13. 未决事项（进入实现前必须冻结）

1. **cron 解析依赖（已决定，2026-09-22）**：引入 `github.com/robfig/cron/v3`——
   成熟的 5 字段 cron 解析，自带时区支持，测试覆盖完整。第三方依赖因此从 3 个增加到 4 个，
   `AGENTS.md` 的依赖约定已同步更新。
   本项目**不**支持 `L`/`#`/`W`、秒字段，也不接受 `@every` 之外的别名；
   计划类型只有 `cron`（5 字段）与 `interval`（固定间隔秒数）两种。
2. **调度器与 systemd timer 的关系（已决定，2026-09-22）**：采用**方案 A——`opsd` 的
   进程内调度循环是唯一权威**。理由：无论哪种机制，最终都要经过 `opsd` 执行（它持有数据库、
   锁与审计），`opsd` 停摆时 systemd timer 触发的提交同样会失败，因此 timer 并未带来可用性上
   的本质提升；而 systemd 的 `Persistent=true` 会与本迭代的错过执行策略冲突，出现两套「补跑」
   逻辑各自主张。1c 因此**不生成 `.timer`**，只生成 `.service`。将来若确实需要 systemd 集成，
   在 1c 增加一条**显式**的「把某条计划导出为 timer」命令，而不是让两边自动同步。
3. `intervalSeconds ≥ 60` 的下限是否合适（是否需要秒级间隔）。
4. **移除 `runAll` 策略（实现中发现，2026-09-22）**：原设计允许「把积压的时刻逐个补跑」，
   但同一 `resource` 上最多允许一个未完成 Operation（迭代 0 的刻意不变量），而 `resource`
   是计划自带的，因此 5 个补跑里必然有 4 个撞上 `LOCK_BUSY`。一个通常无法达成其承诺的策略
   比没有更危险，所以只保留 `skip` 与 `runOnce`。需要连续补跑多次的场景应改用更短的间隔或
   拆分 `resource`。同时新增**误触发宽限窗口**（第 5 节），否则 `skip` 会连准点触发一起丢掉。

## 14. 未验证内容

- 真实时区数据库（`tzdata`）在目标镜像/主机中的可用性：容器内 `time.LoadLocation`
  需要 `tzdata`，精简镜像可能缺失。**已由 `make verify-linux` 覆盖**（`Dockerfile.systemd`
  显式安装 `tzdata`，并断言容器内可解析 `Asia/Shanghai`、带时区的计划按计划时区解释）。
- 真实 Linux 主机上跨重启、跨夏令时的长期运行稳定性（需要长时间观察）。
- 多 `opsd` 实例同时调度的场景（不在本迭代范围）。

## 15. 实现记录 2026-09-22

### 已实现范围

第 2 节列出的 1b 范围全部落地：`Schedule`/`ScheduleRun` 领域模型与校验、cron（5 字段）
与固定间隔两种触发器、时区、两种错过执行策略与误触发宽限窗口、migration `0003`、
原子派发、调度循环、API 端点、CLI 子命令与测试。

### 对本文档的补充与偏离

1. **移除 `runAll`、新增误触发宽限窗口**（第 13 节第 4 条、第 5 节）：两处都是被测试
   直接暴露出来的设计缺陷，不是实现取舍。
2. **移除 `skipped` 结果**：`runAll` 去掉后没有任何代码路径会产生它，保留在枚举里
   会让人以为存在「跳过但不算错过」的语义。SQL 的 `CHECK` 与领域常量已同步收窄。
3. **启用/停用使用两条显式路由**：原设计用 `POST /schedules/{id}/{action}` 通配，
   它会连 `/schedules/{id}/runs` 一起吞掉，并把未知动作当成停用处理。
4. **计划名称唯一冲突返回 `SCHEDULE_INVALID`**，不新增错误码；理由与 1a 的重名应用一致。
5. **重新启用不补偿**：`enable` 时把 `nextRunAt` 从当前时刻重新计算，否则一启用就会
   补跑一堆停用期间积累的过期任务。
6. **截断后不追赶**：错过时刻超过 1000 个时只处理到上限，并把 `nextRunAt` 从 `now`
   之后重新开始，而不是继续追赶积压。

### 实现过程中被测试暴露的问题

- **`skip` 吞掉准点触发**：调度器无法区分「刚到期」与「停机积压」，导致 `skip` 策略把
  自己的正常触发也记为 `missed`。修复方式是引入误触发宽限窗口。
- **`runAll` 无法兑现**：见第 13 节第 4 条。
- **harness 假阳性**：`$(test -f /usr/share/zoneinfo/...)` 在本机执行，macOS 上有 zoneinfo
  于是「tzdata 已安装」假通过，而容器里其实根本没有该文件——后面的创建失败才是真的。
  整条命令必须经由 `q` 进容器执行，已在 `AGENTS.md` 与代码注释中记录。

## 16. 验证记录 2026-09-22

- 执行者：Command Code agent
- 变更范围：迭代 1b 全部交付内容
- 环境：macOS (darwin/arm64)，Go 1.27.1；Linux 容器证据来自 Docker 29.4 / OrbStack
- 规模：9 个测试包，237 个顶层用例 + 76 个子用例；Linux 容器 harness 40 项断言

> **统计口径说明（2026-09-23 补注，原数字保留不改）**：本节的 **237 + 76** 与任何一种可
> 复算的口径都对不上。在 HEAD（`747aee9`）上：对所有 `*_test.go` 统计 `^func Test` 行得
> **182**（减去 `test/e2e/harness_test.go:21` 的 `TestMain` 后是 **181** 个 `go test -v`
> 顶层用例），含子测试的总 PASS 为 **313** 项。逐一对不上：
>
> - 若「237」指 `go test -v` 的顶层用例，它不可能大于 HEAD 的 181——HEAD 同时包含 1b 与 1c
>   的全部测试，同口径下 1b 时点的数字必然**不大于** 181；
> - 237 + 76 = 313，恰好等于 HEAD 含子测试的总 PASS，**提示**它的「顶层」可能把子测试一并
>   算了进去（或口径本就是「总用例数」），但原文没写明，无法从现有材料确认；
> - 1a 的「176 + 48 = 224」同样对不上上述任何一种口径。
>
> 结论：两组数字都只能当「规模量级」读，且**不能**与 HEAD 或彼此混用比较。

### 命令与结果

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `make fmt` | PASS | 静态 |
| `go vet ./...` | PASS | 静态 |
| `go test ./... -count=1` | PASS（9 个包；e2e 含真实触发） | 单元 + 集成 + e2e |
| `go test -race ./...` | PASS | 单元 + 集成 + e2e |
| `make cross`（linux/amd64、linux/arm64） | PASS | 交叉编译 |
| `make verify-linux` | PASS（40 项断言） | **Linux 容器** |

### 逐条验收标准的证据

| # | 验收标准 | 证据 |
| --- | --- | --- |
| 1 | cron 与间隔计划到点各触发一次且只触发一次 | e2e `TestScheduleActuallyFiresAndRunsCommand`（真实进程 + 真实时钟，验证 Operation 被创建、执行成功、日志可见）；`TestTickDispatchesDueScheduleOnce` 断言重复 Tick 不产生第二条记录 |
| 2 | `opsd` 重启后不为已记录的时刻重复触发 | `TestRestartDoesNotDispatchSameMomentTwice`（同一数据库、全新调度器实例）；`TestDispatchScheduledRunCreatesOperationOnce`（唯一约束）；`(schedule_id, scheduled_for)` 唯一索引 |
| 3 | 两种错过执行策略行为符合定义，且准点触发不被 skip 丢掉 | `TestMissedRunPolicies`（5 个积压时刻下 skip=0/5、runOnce=1/4）；`TestMisfireBoundary` 三个子用例钉住准点、单个迟到、积压的分界 |
| 4 | 同一时刻同一计划只能产生一条 `ScheduleRun` | `TestDispatchScheduledRunCreatesOperationOnce`、`TestRecordMissedRunIsIdempotentPerMoment`（单元 + 集成） |
| 5 | 计划 spec 非法在创建时即失败 | `TestCreateScheduleValidatesSpec`（单元）、`TestCreateScheduleRejectsInvalidInput/spec_非法`（集成）、`TestScheduleRejectsInvalidCron`（e2e 退出码 16） |
| 6 | 资源被占用时记为 failed/`LOCK_BUSY`，既有 Operation 不受影响 | `TestTickRecordsLockBusyWithoutQueueing`（单元）、`TestDispatchScheduledRunRecordsLockBusy`（集成） |
| 7 | 时区与夏令时切换不漏跑、不重复 | `TestCronTimezoneChangesFiringInstant`、`TestCronAcrossDaylightSavingBoundary`（单元）；`make verify-linux` 断言容器内时区可解析且按计划时区解释（**Linux 容器**） |
| 8 | `make ci` 与 `make verify-linux` 全绿 | 见上表 |
| 9 | 迭代 0 与 1a 的回归用例全部继续通过 | 9 个包全绿，迭代 0/1a 的 e2e 与单元用例未做任何放宽 |

### 说明

- 验收标准 1 的 e2e 用例必然要等到下一个整分钟（cron 粒度决定，平均约 30 秒、最多约 60 秒），
  因此 `go test -short` 会跳过它；默认的 `make test` / `make ci` 会执行。
- 「多 `opsd` 实例同时调度」仍未验证：本迭代只保证单实例内的不重复触发，
  多实例并发调度属于迭代 5 的范围。
