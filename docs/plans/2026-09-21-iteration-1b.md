# Linux 运维工具迭代 1b：调度器

> 文档日期：2026-09-22
> 文档状态：Proposed / 待确认设计，未实现
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
- 错过执行策略（skip / runOnce / runAll）与重启后的补偿判断。
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
| `missedRunPolicy` | `skip`（默认）/ `runOnce` / `runAll` |
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
| `result` | `dispatched` / `skipped` / `missed` / `failed` |
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

`opsd` 启动时：

1. 载入所有 `enabled` 的计划。
2. 对每个计划算出「上次应当触发到现在之间」漏掉的时刻。
3. 按 `missedRunPolicy` 处理：
   - `skip`：为每个漏掉的时刻写一条 `result=missed` 的 `ScheduleRun`，不建 Operation。
   - `runOnce`：只补跑**最近漏掉的那一个**，更早的写 `missed`。
   - `runAll`：逐个补跑，按时间顺序排队。
4. 写回 `nextRunAt`，进入正常循环。

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
        CHECK (missed_run_policy IN ('skip', 'runOnce', 'runAll')),
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
        CHECK (result IN ('dispatched', 'skipped', 'missed', 'failed')),
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
                       --timezone <tz> --missed-run-policy skip|runOnce|runAll
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
- 错过策略三种行为，边界（只漏一个、漏很多个）。
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
3. 三种错过执行策略行为符合定义。
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

## 14. 未验证内容

- 真实时区数据库（`tzdata`）在目标镜像/主机中的可用性：容器内 `time.LoadLocation`
  需要 `tzdata`，精简镜像可能缺失。1b 无法在 macOS 上验证 Linux 容器内的时区行为，
  需要扩展 `make verify-linux`，在 `Dockerfile.systemd` 中显式装 `tzdata` 并断言
  `Asia/Shanghai` 可解析。
- 真实 Linux 主机上跨重启、跨夏令时的长期运行稳定性（需要长时间观察）。
- 多 `opsd` 实例同时调度的场景（不在本迭代范围）。
