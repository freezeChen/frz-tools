# Linux 迭代 1d：任务引擎的重试、退避与并发策略

> 文档日期：2026-09-24
> 文档状态：**规格已冻结（2026-09-24）、待实现**——第 12 节的 5 条决定已全部按推荐值拍板
> 对应路线图：[2026-09-21-linux-ops-tool-roadmap.md](./2026-09-21-linux-ops-tool-roadmap.md)（第 4 节「任务引擎」，归属决定见第 13 节 2026-09-22 记录）
> 前置迭代：[2026-09-21-iteration-1c.md](./2026-09-21-iteration-1c.md)（已实现、已提交、CI 全绿）

## 1. 定位与前置

迭代 0 的任务引擎做了**单次**执行的骨架：状态机、超时、取消、幂等、资源锁、审计，
以及「守护进程重启后把 running 一律标记失败、**不重放**」。1b 的调度器只决定**何时**建
一个 Operation，不决定失败之后怎么办。1c 的 `runtime.*` 复用同一条创建路径。

缺的是**失败之后的第二次机会**：重试、指数退避、最大尝试次数、可重试的错误集合，
以及重试与并发/锁/调度/重启恢复之间的关系。迭代 2 的备份与迭代 3 的部署都要用这套
（备份遇到瞬时故障要重试、部署要等实例就绪），放到最后做会让它们各自将就一套。

**1d 不改状态机、不改资源锁语义、不改执行器接口**——只在既有骨架上补一层
「失败后按策略再排一次」的机制。

沿用三层验证策略（见 1a 第 1 节第 2 条）：端口 + 假实现可在 macOS 与 CI 上跑；
Linux 容器验证真实 systemd 交互；真实 Linux 主机项挂起并标注未验证。

## 2. 范围

### 1d 实现

- 提交时声明重试策略，失败后按策略**自动**排下一次尝试（指数退避 + 抖动）。
- 最大尝试次数与重试链（`attempt` / `maxAttempts` / `retryOf`）。
- **可重试的错误码白名单**，默认不重试。
- 守护进程重启后的重放：**仅**对显式声明了重试策略的操作，按策略排一次重试。
- 退避期间的并发正确性：未到 `notBefore` 的操作**不被领取、不占 worker**。
- 观测：每次尝试独立的日志与审计；API 暴露 `attempt` / `nextAttemptAt`。
- 配置与 CLI 增量。

### 1d 不实现

- **断点续传**（同一进程内从字节/分片的中断处继续）：这需要每个适配器自备 checkpoint，
  属于迭代 2 的备份适配器（`BackupAdapter`）自己的事，1d 不定义通用机制。
  1d 的「断点恢复」只到**操作级**：把被中断的操作按策略重新排队，而不是让同一次执行续跑。
- **优先级队列与抢占**：没有真实需求，不做。重试与普通操作**同优先级**。
- **按 kind / 按 resource 的自定义并发配额**：1d 不做。跨资源的公平性由「先到先服务 + 退避
  不占 worker」（D4）保证就够了；在没有真实的多租户或配额需求之前冻结一套配额模型，
  属于把层次猜错（见路线图第 13 节「迭代 1 曾把内容拆错归属」的教训）。
- metrics 与成功/失败通知接口、CLI `init`/`validate`/`status`（迭代 5）。
- 发布编排相关的重试语义（迭代 3/4）：1d 只提供机制，不定义「发布失败该退回到哪个 slot」。

## 3. 关键决策

这一节是本规格的核心。每条都写清**为什么**，实现时不得偏离；要改必须先改本节。

### D1：重试的载体是**新建 Operation**，不是同一行回到 `pending`

方法状态机是冻结的：`pending → {running, cancelled}`、`running → {succeeded, failed, cancelled}`，
**没有回到 `pending` 的边**（`internal/domain/operation.go`）。手动重试（`Service.Retry`）
已经是「新建一行 + `retry_of` 指向来源」的形态。

因此自动重试沿用同一形态：**每一次尝试是一个独立的 Operation**，用 `retry_of` 串成链，
用 `attempt` 记录它是第几次。好处是锁、审计、日志、取消、查询全部复用，不需要为
「同一个操作的多条记录」发明新的数据结构；`operation get/logs` 的语义不变。

### D2：自动重试**默认关闭**（opt-in）

执行器跑的是**任意 argv**，自动重试会真的把副作用做第二遍。对没有声明幂等性的命令默认重试
是危险的——这比「不重试」坏得多。所以只有请求（或计划）里**显式给出重试策略**时才生效；
未声明时 `maxAttempts` 恒为 1，行为与迭代 0 完全一致。

### D3：可重试的错误集合是**白名单**，且必须在写入时判定

不是所有失败都值得重试。默认白名单：

| 错误码 | 可重试 | 理由 |
| --- | --- | --- |
| `EXEC_TIMEOUT` | 是 | 瞬时慢，重试有意义 |
| `EXEC_EXIT_NONZERO` | 是 | 命令自身偶发失败（资源竞争、临时文件冲突） |
| `RUNTIME_NOT_READY` | 是 | 预热没完成，正是「等一等再试」的典型 |
| `DAEMON_RESTARTED` | 是 | 被上一个守护进程中断，与命令本身无关 |
| `EXEC_CANCELLED` | **否** | 用户主动取消，重试等于违抗指令 |
| `PERMISSION_DENIED` | **否** | 权限不会因为重试而改变 |
| `LOCK_BUSY` | **否** | 锁冲突在提交期就返回了，不是执行期失败 |
| `MANIFEST_INVALID` / `INVALID_REQUEST` / `CONFIG_INVALID` | **否** | 配置错误，重试只会重复同样的错误 |
| `SECRET_UNRESOLVED` | **否** | 凭据缺失不会自愈；且重试会反复触碰凭据路径 |
| `INTERNAL` | **否** | 未知原因，默认不重试（宁可让运维看见） |

判定发生在**结束操作的那一刻**（worker 把 `FinishInput` 落库时），不是在恢复时重算。

### D4：退避是**持久化**的，抖动在写入时决定

新增 `not_before` 列：`NULL` 表示立即可领取，否则 worker 不领取早于它的 pending 操作。

退避公式：`delay = min(base * 2^(attempt-1), maxDelay)`，再叠加抖动。

两个必须做对的地方：

- **抖动在写入时算一次并存进 `not_before`**，不在读取时重算。否则守护进程重启后
  同一个 operation 会算出不同的时间，退避窗口漂移，且测试无法断言。
- **退避不得占着 worker**：到点前**不领取**。绝不允许「领到手里再 sleep」——
  那会让退避中的操作把 worker 池占满，把正常的新操作饿死。这是 1d 最容易写错的一处。

### D5：重启恢复只对**显式声明了重试**的操作重放

现状（迭代 0）：`RecoverRunning` 把 running 一律置为 `failed`（`DAEMON_RESTARTED`），
**不重放任何东西**。这个默认语义是对的，1d 不改。

1d 增加一条**很窄**的例外：如果该操作声明了重试策略、且 `DAEMON_RESTARTED` 在白名单里
（默认在），则由恢复流程排一次重试。也就是说——

- 没声明重试的操作：**一字不变**，仍是标记失败、不重放。
- 声明了重试的操作：能续上，但仍受 `maxAttempts` 与退避约束。

**不得**把这条例外扩大成「所有 running 都自动重放」：那会让一个非幂等的命令在每次重启后
被重新执行一次，是最危险的一类行为变化。

### D6：重试与 `resource` 锁、调度器、`runtime.*` 三者的关系

- **锁**：重试的尝试是**普通的 pending operation**，必须等同一 `resource` 的锁释放，
  不插队、不豁免。重试不绕过 `LOCK_BUSY`。
- **调度器**：1b 的调度器只负责「到点建一个 operation」，**不感知重试**。重试链上新建的
  操作**不再触发调度**，也不会改变计划的 `nextRunAt`。`missedRunPolicy`（错过了怎么办）
  与重试策略（失败了怎么办）是两件事，**不得互相代替**。
- **`runtime.*` 与 systemd 的 `Restart=`**：这两层重试**不得叠加**。unit 的 `Restart=on-failure`
  已经在进程级拉起崩溃的进程；opsd 的 `runtime.start` 重试是**操作级**的，判据是
  「`startTimeout` 内就绪探测没通过」。因此：
  - `runtime.start` 成功过一次（就绪通过）之后，进程后来崩溃属于 **unit 的 `Restart=`**
    与健康检查的职责，**不触发**操作级重试。
  - `runtime.start` 只有在**就绪从未通过**时才失败，此时才轮到操作级重试。
  - 这条要在实现里用测试钉住：进程起来但从未就绪 → 失败 → 可重试；
    起来且就绪过再被杀 → **不**产生重试。

> **2026-09-24 更正（容器实跑发现的规格缺陷）**：上面第二条「`runtime.start` 只有在就绪
> 从未通过时才失败」**不成立**。适配器的 `Start` 根本不等就绪——它只做一次 `systemctl start`
> 就返回（`internal/adapters/runtime/systemd/systemd.go`），就绪超时只在 `Health` 里判定
> （`startDeadlineExceeded`）。所以「就绪从未通过」不会让 `runtime.start` 失败。
>
> 这不改变 D6 的**结论**，反而让它更简单：就绪失败不是操作失败，因此**没有**可重试的东西——
> 本来就不该、也不会冒出重试行。实际操作级的 `runtime.start` 失败只来自 `systemctl start`
> 自身失败（映射为 `RUNTIME_NOT_READY`，在白名单里，因此可重试）。
>
> 容器断言已按**真实语义**重写（`check_retry` 的三个夹具），详见第 13 节的「规格缺陷」与
> 第 14 节。这条是本次迭代**唯一**由容器实跑纠正的规格内容。

### D7：`dryRun` 与重试互斥

`dryRun` 没有真实副作用，也就没有「值得重试的失败」；把两者一起提交是规格误用。
沿用 `runtime.*` 拒绝 `dryRun` 的做法：**提交带 `retry` 的 `dryRun` 操作直接拒绝**，
返回 `INVALID_REQUEST`，并在消息里指向「去掉 dryRun 或去掉 retry」。

## 4. 端口与接口

`Executor` 接口**不变**——重试是编排层的事，执行器不需要知道自己在第几次尝试里
（这也保证了 `proc` 与将来任何执行器不必各自实现一份重试）。

`Repository` 新增/改动：

```go
// ClaimNextPending 增加「不早于」过滤：只领 not_before 为 NULL 或已到期的行。
// 排序仍是 created_at（先到先服务），保证重试不插队、也不被饿死。
ClaimNextPending(ctx context.Context, now time.Time) (*domain.Operation, error)  // 签名不变

// 新增：为重试链排下一次尝试。
ScheduleRetry(ctx context.Context, in domain.RetryInput, now time.Time) (*domain.Operation, bool, error)
```

`domain` 新增重试策略值对象与校验（非法策略在**提交时**就返回错误，不等到执行失败）：

```go
type RetryPolicy struct {
    MaxAttempts   int           // 含首次；1 = 不重试
    BaseDelay     time.Duration
    MaxDelay      time.Duration
    RetryableCodes []api.ErrorCode // 空 = 用默认白名单（D3）
}

func (p RetryPolicy) Validate() error
// Retryable(code) bool、NextDelay(attempt int) time.Duration
```

`NextDelay` 必须是**纯函数且抖动由调用方注入**（`rand` 作为参数或字段），否则测试无法断言
退避序列——这是 D4 能写成测试的前提。

## 5. 模型冻结（API 与 CLI）

### 请求增量

```yaml
# POST /api/v1/operations 的请求体新增（可选段）
retry:
  maxAttempts: 3          # 含首次；1 = 不重试；默认 1
  baseDelaySeconds: 5     # 默认 5
  maxDelaySeconds: 300    # 默认 300
  retryableErrorCodes:    # 省略 = 用 D3 的默认白名单
    - EXEC_TIMEOUT
```

校验（提交时，全部返回 `RETRY_POLICY_INVALID`）：

- `maxAttempts` 必须 ≥ 1；上限 `MaxRetryAttempts = 10`（再大没有意义，且会让退避窗口失控）。
- `baseDelaySeconds` ≥ 1；`maxDelaySeconds` ≥ `baseDelaySeconds`。
- `retryableErrorCodes` 只允许 D3 表里的取值；`EXEC_CANCELLED` 等明确不可重试的码**不得**
  由用户加进白名单（用户可以说「不重试某些码」，不能说「取消也要重试」）。
- 与 `dryRun` 同时出现 → `INVALID_REQUEST`（D7）。

### 响应增量

`Operation` 新增字段，沿用既有 JSON 命名风格（camelCase、可省略的用 `omitempty`）：

```json
{
  "attempt": 2,
  "maxAttempts": 3,
  "nextAttemptAt": "2026-09-24T02:15:30Z",
  "retryOf": "op_...",
  "retryExhausted": false
}
```

`nextAttemptAt` 只在 `status=pending` 且退避未到期时出现；`retryExhausted` 只在
「最后一跳失败且已达上限」时为 `true`，让调用方不必自己数链。

### CLI

```bash
opsctl operation submit --kind executor.command --resource r --retry-max 3 \
  --retry-base 5s --retry-max-delay 5m -- /usr/bin/foo
opsctl operation get <id>          # 人类可读输出增加 attempt / nextAttemptAt
opsctl operation retry <id>        # 语义不变：手动重试，不受策略约束、不计入 maxAttempts
```

`operation retry` 是**手动**动作，与自动重试刻意分开：手动重试不受 `maxAttempts` 限制
（运维的判断优先），但也会写 `attempt = 上一跳 + 1`，保持链上计数连续。

## 6. 持久化（migration `0005_operation_retry.sql`）

```sql
ALTER TABLE operations ADD COLUMN attempt        INTEGER NOT NULL DEFAULT 1;
ALTER TABLE operations ADD COLUMN max_attempts   INTEGER NOT NULL DEFAULT 1;
ALTER TABLE operations ADD COLUMN not_before     TEXT;
ALTER TABLE operations ADD COLUMN retry_policy_json TEXT;

-- 领取查询是 status = 'pending' AND (not_before IS NULL OR not_before <= ?)
-- ORDER BY created_at，因此索引把这三段都覆盖到。
CREATE INDEX ix_operations_pending_not_before
    ON operations (status, not_before, created_at);

-- 重试链的查询（「这条链上有几次尝试」）与运维排查都按 retry_of 走。
CREATE INDEX ix_operations_retry_of ON operations (retry_of) WHERE retry_of IS NOT NULL;
```

**兼容性**：所有新列都有默认值，既有行的 `max_attempts = 1` 即「不重试」，
与升级前的行为**逐字节一致**；`not_before = NULL` 即「立即可领取」。
本次只 `ALTER TABLE ... ADD COLUMN` 与建索引，**不动任何既有列、不改 `resource_locks`**。

`retry_policy_json` 存**策略原文**而不是解析后的字段：这样「当时到底按什么策略重试的」
在事后可查，且策略字段增减不需要再加列。解析失败时按「不重试」处理并记一条 warning 日志
（宁可少重试一次，也不能因为策略读不出来就乱重试）。

## 7. 错误码与退出码增量

只新增一个：

| 错误码 | HTTP | 退出码 | 用途 |
| --- | --- | --- | --- |
| `RETRY_POLICY_INVALID` | 400 | 22 | 重试策略本身非法（第 5 节的校验） |

不新增「重试耗尽」之类的错误码：重试链的最后一跳终态就是 `failed`，`errorCode` 保持
**原始的失败原因**（这样运维看到的是「为什么失败」，而不是「重试用完了」这种不解释原因的状态）；
链的信息由 `retryOf` / `attempt` / `retryExhausted` 表达。

## 8. 兼容性与迁移影响

- **行为默认不变**：不声明 `retry` 的一切调用与计划，行为与迭代 0/1a/1b/1c 完全一致。
- **旧库升级**：见第 6 节，既有行 `max_attempts = 1`。
- **API 版本**：`Operations` 的请求体新增**可选段**，响应新增**可选字段**，属于同一
  `apiVersion` 内的向后兼容变更，**不提升 apiVersion**（见 AGENTS.md 的版本化规则）。
- **`operation retry` 的既有行为**：手动重试的对外语义不变，只是链上多了 `attempt` 计数。
- **调度计划**：`Schedule` 模型本次**不加**重试字段（留到第 12 节未决事项 4 决定）；
  因此计划触发的操作暂时无法声明重试策略。这是**已知的功能缺口**，1d 验收标准不含它。

## 9. 测试计划

### 单元（`internal/domain`、`internal/application`）

- `RetryPolicy.Validate`：上界、下界、非法错误码、`EXEC_CANCELLED` 不可入白名单。
- `RetryPolicy.NextDelay`：表驱动断言退避序列（抖动注入固定值），`maxDelay` 截断生效。
- `RetryPolicy.Retryable`：D3 表的每一行都有一条用例（可重试与不可重试都要有）。
- 判定时机：失败落库时按 D3 判定，**恢复时不得重算**。

### 集成（`internal/application`，用假 Repository / 假执行器）

- 未声明 retry → 失败后 `attempt` 仍为 1，**不产生新行**（这是最容易写反的一条）。
- 声明 retry → 在可重试错误上新建下一跳，`retry_of` 指向上一跳、`attempt` 递增。
- 达上限 → 不再新建，最后一跳保留原始 `errorCode`、`retryExhausted = true`。
- 不可重试错误 → 即使声明了 retry 也不新建。
- **退避不占 worker**：`not_before` 未到期时 `ClaimNextPending` 不返回该行（关键用例）。
- 锁：重试的尝试必须等 `resource` 锁释放，不插队。
- 取消：链上任一跳被取消后不再排新尝试。
- `dryRun + retry` 被拒绝（`INVALID_REQUEST`）。
- 手动 `retry` 不受 `maxAttempts` 限制，但仍递增 `attempt`。

### e2e（`test/e2e`）

- 端到端跑一条会先失败后成功的命令（一个写计数文件、第 2 次才 `exit 0` 的脚本），
  断言确实重试且最终 `succeeded`，且链上两跳都能 `operation get` 到。
- 退避在 e2e 里用**很大的 base**（例如 60s）反证：提交后立刻查询，`nextAttemptAt` 在未来、
  且在这一刻状态仍是 `pending`、**没有**被领取。
- **平台注意**：1c 的 e2e 用例刚因为把「本机如何」的前提写死而在 Linux CI 上失败过
  （见 1c 第 17 节 2026-09-24 记录）。1d 的 e2e 用例**不得**依赖「本机装了什么」，
  平台相关的断言必须按 `GOOS` 分开写。

### Linux 容器（`test/linux/verify.sh`）

- D6 的 `runtime.*` 与 `Restart=` 不叠加：进程起来但从未就绪 → `runtime.start` 失败；
  起来且就绪过再 `kill` → **不**产生操作级重试，由 unit 的 `Restart=` 接管。
  这一条**只能**在真实 systemd 上验证，是本迭代主要的新增容器断言。

### 回归

迭代 0 / 1a / 1b / 1c 的全部用例继续通过，尤其：
`TestRestartRecoversRunningOperation`（重启恢复的既有语义）、
`TestDuplicateIdempotentSubmissionExecutesOnce`（幂等）、
`TestSubmitRejectsBusyResource`（锁）。

## 10. 验收标准

1. 未声明重试的操作，失败后不自动重试（`attempt` 恒为 1，不产生新 Operation）。
2. 声明重试的操作在可重试错误上自动新建下一次尝试，`retryOf` 指向上一跳、`attempt` 递增。
3. 退避被真实遵守：`notBefore` 之前该操作**不被领取**，且**不占 worker**。
4. 达到 `maxAttempts` 后不再新建尝试，最后一跳的终态与 `errorCode` 保留原始失败原因，
   且 `retryExhausted = true`。
5. `EXEC_CANCELLED`、`PERMISSION_DENIED`、`SECRET_UNRESOLVED` 等不可重试错误，
   即使声明了重试也不重试；且用户**无法**把这些码加进白名单。
6. 重试不绕过资源锁：链上任一尝试都必须等同一 `resource` 的锁释放。
7. `dryRun` 与 `retry` 同时提交被拒绝（`INVALID_REQUEST`）。
8. 重启恢复：声明了重试的 running 操作被标记 `DAEMON_RESTARTED` 后按策略排一次重试；
   未声明的一字不变（标记失败、不重放）。
9. 取消优先：链上任一跳被取消后，不再排新的尝试。
10. `runtime.*` 与 systemd `Restart=` 不叠加（D6），证据类型为 **Linux 容器**。
11. 每次尝试都有独立的日志与审计；`operation get` 能看出 `attempt` 与 `nextAttemptAt`。
12. `make ci` 与 `make verify-linux` 全绿，且迭代 0/1a/1b/1c 的回归用例全部继续通过。

### 逐条判定（待实现后填写）

| # | 判定 | 证据与类型 |
| --- | --- | --- |
| 1–12 | **未实现** | 规格已于 2026-09-24 冻结（第 12 节），待实现后逐条填写 |

## 11. 未验证内容

本迭代**尚未实现**，因此第 10 节全部条目当前均为「未实现」。实现后按证据类型逐条填写；
以下是**已知将来也不能由 1d 的常规证据覆盖**的项：

- 真实主机的长退避（分钟级）在长时间运行下的行为——容器能验短退避，长时间稳定性需真机。
- 与真实 systemd 应用崩溃策略（`RestartSec`、`StartLimitBurst`）叠加后的实际表现，
  在**不同发行版**上的一致性（真机项，与 1c 的挂起项同源）。
- 远程/多主机下的重试（迭代 5）。

## 12. 已冻结的决定（原「未决事项」）

> **2026-09-24：以下 5 条全部按推荐值冻结，1d 可以进入实现。**
> 冻结的是取值与范围，不是实现细节——实现时若发现某条不可行，必须先改本节并记录原因，
> 不得默默偏离（沿用 1c 的做法：偏离写在实现记录里并说明理由）。

### 决定 1：退避默认值与上限 —— **按推荐**

`base = 5s`、`maxDelay = 5m`、抖动为 `±20%` 的均匀分布、`maxAttempts` 上限 `10`。

*理由*：5s 足够吸收瞬时抖动，5m 上限避免一条链占着一整天；`maxAttempts = 10` 配 5m 上限
约等于 40 分钟收敛，再长就该让人而不是机器来决定。
*被否决的替代方案*：全抖动（full jitter，`rand(0, computed)`）——分布更好，但退避时间的
下限变成 0，运维更难解释「为什么又立刻跑了」。

### 决定 2：请求级白名单 —— **只允许缩小，不允许扩大**

请求可以给出 `retryableErrorCodes`，但只能从 D3 的默认白名单里**去掉**某些码；把
`EXEC_CANCELLED` 之类明确不可重试的码加进来会被拒绝（`RETRY_POLICY_INVALID`）。

*理由*：见 D3 与第 5 节——「取消也要重试」这类意图不该由调用方表达。

### 决定 3：`runtime.*` 允许声明重试 —— **允许，按 D6 收口**

只有「就绪从未通过」才失败、才可重试；已就绪过再崩溃归 unit 的 `Restart=` 与健康检查。

*理由*：「应用启动慢、首次就绪探测超时」正是最需要重试的场景，禁掉等于把最痛的用例排除在外。
*被否决的替代方案*：`runtime.*` 一律禁止重试。

> **2026-09-24 更正**：本决定的**结论（允许）不变**，但上面这条理由的**前提不成立**——
> `runtime.start` 不等就绪，所以「启动慢」根本不会让它失败，也就谈不上靠重试去覆盖它。
> 实际操作级的 `runtime.start` 失败来自 `systemctl start` 自身失败与适配器的前置校验
> （`prepared`），错误码都是 `RUNTIME_NOT_READY`、都在白名单里——**允许重试的正当理由是这些
> 瞬时失败，而不是就绪超时**。详见第 3 节 D6 的更正与第 13 节的「规格缺陷」。

### 决定 4：策略不挂到 `Schedule` / `Application` —— **1d 不做**

只在 `CreateOperationRequest` 上支持。等迭代 2 的备份计划真正需要时，再决定挂在
`Schedule` 还是 `Application` 层。

*理由*：现在就加会在没有真实用例的情况下冻结错层次（见路线图第 13 节的教训：
迭代 1 的七类交付曾被拆错归属）。

### 决定 5：不额外设重试链长度上限 —— **由 `maxAttempts` 约束**

`operation get` 的输出要能看出链，日志里每次尝试都有独立行，避免排查时要挨个查。

## 13. 实现记录

实现完成于 2026-09-24，分两个提交：主体见 `19734a9`（重试策略、退避与持久化），
收尾见紧随其后的提交（e2e、容器断言与本文档）。

### 逐文件要点

**领域（`internal/domain/retry.go`、`operation.go`）**
- `RetryPolicy` 值对象：`Validate` / `Retryable` / `NextDelay`，以及 `RetryPolicyFromSpec`
  （提交原文 → 值对象，同时作为**读回持久化策略**的入口，保证写入与读回的解释不漂移）
  与 `ParseRetryPolicy`。
- 默认白名单只有四个码；`neverRetryableCodes` 是一份任何情况下都不得重试的集合，它同时被
  `Validate`（拒绝用户把它列进白名单）与 `Retryable`（最终判据）使用——两处都用它，
  所以「塞进白名单就能救回来」这条路不存在。
- `NextDelay(attempt, jitter)` 的抖动由调用方注入。这不是为了好看：**不注入就无法断言退避
  序列**，而 D4 要求抖动在写入时算一次，落成测试的前提正是这里可注入。
- `Operation` 新增 `Attempt` / `NotBefore` / `RetryPolicy` / `RetryPolicyJSON` 与
  `RetryExhausted()`；`FinishInput` 新增 `Retry *RetryPlan`。

**持久化（`migrations/0005_operation_retry.sql`、`internal/adapters/sqlite/store.go`）**
- 新增 `attempt`、`not_before`、`retry_policy_json` 三列与两个索引；全是新增、都有默认值，
  既有行的行为逐字节不变。
- `not_before` 用**定宽**纳秒格式（`2006-01-02T15:04:05.000000000Z`），而不是仓库通用的
  `time.RFC3339Nano`。原因见下面的「发现但未修」。
- `ClaimNextPending` 增加 `(not_before IS NULL OR not_before <= ?)` 过滤——退避在**领取之前**
  过滤掉，绝不「领到手里再 sleep」。
- `Finish` 在同一个事务里调用 `insertRetryOperation` 创建下一跳；`insertRetryOperation` 复用
  原操作的 kind/resource/spec/请求摘要，`attempt` 由原操作**派生**（`original.Attempt+1`）
  而不由调用方传入，避免「链上第几次」出现两个说法。
- `RecoverRunning` 新增 `plan` 回调：判定留在应用层，事务留在适配器，重放的重试与终态同样
  在一个事务里落库。

**编排（`internal/application/`）**
- `Service.Create`：先拒绝 `dryRun + retry`（D7），再校验策略、把策略**原文**序列化进操作。
- `planRetryFor` 是「要不要重试、该等多久」的唯一判定点，`Pool.planRetry`（执行期失败）与
  `Recover`（重启后被中断）都调它——两处共用一份逻辑，语义不会漂移。
- `Pool` 新增 `newID`（为重试生成 Operation ID，与 Service 共用同一个生成器）与 `jitter`
  （可注入）；`Options` 新增 `Jitter`。
- `Service.Retry`（手动）：沿用链上策略、`attempt` 照常递增、**不受 `maxAttempts` 约束**。

**API 与 CLI**
- `CreateOperationRequest.Retry`、`Operation.{Attempt,MaxAttempts,NextAttemptAt,RetryExhausted}`、
  `RuntimeActionRequest.Retry`。
- `operationDTO` 映射新增字段；`nextAttemptAt` 只在该操作仍处于 `pending` 时给出。
- `opsctl operation submit` 与 `opsctl runtime start/stop` 新增
  `--retry-max` / `--retry-base` / `--retry-max-delay`；`wholeSeconds` 拒绝非整秒的时长而不是
  替用户取整。
- `printOperation` 只在真的牵涉重试时多打 `attempt` / `nextAttemptAt` / 用尽提示。

**测试与 harness**
- 新增 `internal/domain/retry_test.go`、`internal/application/retry_test.go`、
  `test/e2e/retry_test.go`；`internal/adapters/httpapi/runtime_test.go` 补两个透传用例；
  `internal/application/service_test.go` 与 `internal/adapters/sqlite/store_test.go` 跟随签名变更。
- `test/linux/verify.sh` 新增 `check_retry`（D6 的容器断言）；
  `test/linux/Dockerfile.systemd` 加装 `sqlite3`（断言「库里的行数没有变」需要直接查库）。

### 对规格的补充与偏离

1. **不加 `max_attempts` 列**（对规格第 6 节列清单的收窄）。第 6 节同时列了 `max_attempts` 列与
   `retry_policy_json`，并说明后者的存在理由是「策略字段增减不需要再加列」。两者放在一起就是
   第二份会与策略原文漂移的真相来源。因此只保留 `attempt`（**可变的操作状态**，必须可查询）
   与 `retry_policy_json`（**不可变的提交内容**，整体存储）；`maxAttempts` 由策略解析得出。
   原则：列放可变状态，JSON 放不可变策略。

2. **`requestHash` 纳入了 retry 段**（规格未提及的兼容性影响）。重试策略是请求内容的一部分，
   同一个幂等键配上不同策略属于**不同请求**，返回原操作等于静默忽略策略的改动。
   **影响**：摘要的计算方式变了，因此**跨升级复用同一个幂等键**会得到 `IDEMPOTENCY_CONFLICT`。
   这是显式报错而不是静默错行为，但确实是一处行为变化，记录在此以免日后被当成 bug。

3. **`nextAttemptAt` 的给出条件比规格略宽**。规格写「只在 `status=pending` 且退避未到期时出现」，
   实现只判 `status=pending`：HTTP 层的 DTO 映射函数没有时钟，而规格的意图是「不要给终态的操作
   一个已经不可能到来的下一次」，这一条已经满足。按 `pending` 判还有一个好处——退避刚好到期、
   尚未被领取的那一瞬间，调用方仍能看到它排在什么时候，而不是字段突然消失。

4. **策略原文解析失败时不打 warning 日志**（规格要求打一条）。`sqlite` 适配器没有 logger，
   而在 `scanOperation` 里返回错误会让一条策略写坏的行**整条读不出来**，那比少一条日志更糟。
   实现改为按「不重试」处理（安全方向），同时把原文留在 `RetryPolicyJSON` 上，因此这行操作
   仍可被查询与排查。规格要求的「不重试」已满足，「warning 日志」这一半记为此处的偏离。

5. **打通了 `RuntimeActionRequest.Retry`**（补齐规格决策 3 的实现缺口）。决策 3 说 `runtime.*`
   **允许**声明重试，但实现前 API 上根本没有这个字段——也就是说这条决策当时**无法表达**。
   本次补齐：`api/v1/runtime.go`、httpapi 的 `createRuntimeOperation`、client 的
   `RuntimeActionInput`，以及 `opsctl runtime start/stop` 的 `--retry-*`。`httpapi/runtime_test.go`
   里两个用例用「非法策略必须报 `RETRY_POLICY_INVALID` 而不是 `RUNTIME_UNSUPPORTED`」
   钉住了这条链路没有被丢掉。

6. **恢复期的重试与终态同事务**（规格未指定原子性）。规格只说「由恢复流程排一次重试」。
   实现做成原子（`RecoverRunning` 的 `plan` 回调在事务内插入），理由与 `Finish` 相同：分两步做的话，
   两步之间崩溃会让这次重试被静默丢掉，而库里看不出任何迹象。

7. **容器镜像加装 `sqlite3`**。`check_retry` 断言「链上没有多出一行」，而端口上没有
   「列出全部操作」的方法，因此必须直接查库。`check_retry` 开头会检查 `sqlite3` 是否存在，
   缺失时给出可执行的修复方式（`--rebuild`）而不是一个看不懂的断言失败。

### 规格缺陷（由容器实跑发现）

**D6 的第二条前提不成立**：规格写「`runtime.start` 只有在就绪从未通过时才失败」，
但 1c 的实现里 `Adapter.Start` **不等就绪**——它只做一次 `systemctl start` 就返回，
就绪超时只在 `Health` 里由 `startDeadlineExceeded` 判定。

这个缺陷是**断言自己抓出来的**：第一版 `check_retry` 按规格写，夹具 A 断言
「就绪永不通过 → `runtime.start` 失败 → 排出重试」，容器实跑直接给出 4 条 FAIL
（`runtime start 未在超时内失败`）。查下来不是实现错了，而是规格把 `Start` 的语义写错了。

处理方式（沿用 1c「旧表述替换但保留追溯」的做法）：

- D6 的**结论不变**，而且更简单：就绪失败不是操作失败，本来就没有可重试的东西；
- `check_retry` 改为按**真实语义**断言，并拆成三个夹具，覆盖面比原计划更完整：
  1. 就绪永不通过 → `runtime.start` **成功**、unit 是 active、`runtime health` 过期后
     退出码 19（`RUNTIME_NOT_READY`）、链上仍只有一行（就绪失败不产生重试）；
  2. `runtime stop` 一个从未 `Prepare` 的应用 → `RUNTIME_NOT_READY` → **链上两行**
     （证明 runtime.* 的重试确实被武装，否则夹具 1 的「没有重试」是空洞的）；
  3. 已就绪过再被 SIGKILL → unit 由 systemd 重启（`NRestarts=1`）、链上仍只有一行。
- 规格 D6 处已加「2026-09-24 更正」说明，不删原文。

### 发现但**未**修（属本次范围之外；**2026-09-24 已修复**，见下）

- **`created_at` 的排序不是严格的时间序**。仓库的时间戳统一用 `time.RFC3339Nano` 格式化，
  而它会裁掉末尾的零、并在小数部分为零时整个省略——于是 `"…T00:00:00Z"` 按字典序**大于**
  `"…T00:00:00.5Z"`，但时间上更早。所有 `ORDER BY created_at`（含 `ClaimNextPending` 的领取顺序）
  在同一秒内、且其中一个时间戳恰好没有小数部分时就会排错。
  **1d 刻意没有顺手改它**：改存储格式涉及既有数据的重写，属于独立的数据迁移，塞进 1d 会让
  「重试」这个提交同时承担一次格式迁移的风险。本次只保证**新引入的 `not_before` 不踩同一个坑**
  （用定宽格式）。影响面是同一秒内提交的操作可能被乱序领取，不影响正确性、只影响顺序。
  已作为独立事项记录，待单独处理。

> **2026-09-24 已修复**（同一个提交，独立于 1d 的功能范围）：`timeLayout` 改为定宽
> `2006-01-02T15:04:05.000000000Z`（写入侧统一），读回改用 `time.RFC3339Nano`——它能接受
> 任意小数位数（含没有小数部分），因此新旧值都读得动，**宽读窄写**。1d 里为 `not_before`
> 单独加的 `notBeforeLayout` / `formatNotBefore` 是同一处理的局部特例，推广后删除，所有时间
> 列共用 `formatTime`。既有库里的可变宽度值由
> `migrations/0007_normalize_timestamp_width.sql` 规范化（13 张表的全部时间列，表达式是恒等
> 变换、可重复执行）。复现用例、迁移用例、影响面与「未验证」项见路线图
> 「2026-09-24（补记五）」。**上面这段发现记录保留原文，只在此标注状态。**

## 14. 验证记录

格式沿用 1c 第 17 节：命令 / 结果 / 证据类型三列，
**静态 / 单元 / 集成 / e2e / Linux 容器 / Linux 主机**分列，不混用。

### 本轮取得的证据

| 命令 | 结果 | 证据类型 |
| --- | --- | --- |
| `make fmt` / `make vet` | 通过 | 静态检查 |
| `go test ./internal/domain/... -count=1` | 通过（新增 `retry_test.go`：策略校验、退避序列、往返解析、`RetryExhausted`） | 单元测试 |
| `go test ./internal/application/... -count=1` | 通过（新增 `retry_test.go`：未声明重试不产生新行、建链、达上限、白名单只可缩小、退避不占 worker、等锁、取消不重试、恢复只重放声明了策略的） | 集成测试 |
| `go test ./test/e2e/... -count=1` | 通过（新增 `TestRetryReRunsCommandThroughCLI`、`TestRetryBackoffIsVisibleAndNotClaimed`） | e2e |
| `make ci`（fmt / vet / test / test-race / 交叉编译） | 通过 | 静态检查 + 单元 + 集成 + e2e |
| `bash test/linux/verify.sh` | **exit 0：106 项通过 / 0 项失败**，其中新增的 `check_retry` 占 17 项 | **Linux 容器** |

`check_retry` 的三个夹具（2026-09-24 实跑，容器内）：

| 夹具 | 断言 | 结果 |
| --- | --- | --- |
| 1 就绪永不通过（进程活着但没人监听就绪端口） | `runtime.start` 成功；unit = `active`；`runtime health` 退出码 19；链上只有 1 行 | 全部 PASS |
| 2 `runtime stop` 一个从未 `Prepare` 的应用 | 首次失败码 `RUNTIME_NOT_READY`；已排出第二次尝试；`maxAttempts=2` 时链上恰好 2 行 | 全部 PASS |
| 3 已就绪过再被 `SIGKILL` | unit 由 systemd 重启（`NRestarts=1`）；链上仍只有 1 行；重启后由健康检查报告就绪 | 全部 PASS |

夹具 1 与夹具 3 合起来就是 D6 的正反两面：**该重试的地方重试了，不该重试的地方没有重试**。

### 干净 runner 上的复现（2026-09-24）

`48a7163` 之后的 CI run `35949920348`：`test` 与 `linux-verify` 两个 job 均通过，其中
`linux-verify` 在干净 ubuntu-latest runner 上 **106 项通过 / 0 项失败**（含 `check_retry` 的
三个夹具，`NRestarts=1` 等断言逐条 PASS）。

至此 1d 的容器断言同时覆盖 **arm64**（本地 OrbStack）与 **amd64**（CI runner）两种架构——
与 1c 的结论一致：这些断言没有架构相关的偶然性。

### 仍未验证

- **真实 Linux 主机**上的重试行为：长退避（分钟级）在长时间运行下的表现、与不同发行版
  systemd 崩溃策略（`RestartSec`、`StartLimitBurst`）叠加后的实际表现。容器证明不了这些
  （与 1c 挂起的真机项同源）。
- 远程 / 多主机下的重试（迭代 5）。

## 15. 结论汇总（2026-09-24）

**规格第 3 节的 D1–D7 与第 12 节冻结的取值全部落地**，第 10 节的 12 条验收标准逐条判定见下。
实现中对规格有 **7 处补充或偏离**，全部记在第 13 节并给了理由；其中第 5、7 条是补齐规格的
实现缺口，第 1、2 条是影响数据模型与兼容性的实质决定。

### 逐条判定（2026-09-24）

| # | 判定 | 证据与类型 |
| --- | --- | --- |
| 1 | **达成** | 集成：`TestNoRetryWithoutPolicy`（未声明重试时链上恒为 1 行且 `attempt=1`） |
| 2 | **达成** | 集成：`TestAutoRetrySchedulesNextAttempt`；e2e：`TestRetryReRunsCommandThroughCLI` |
| 3 | **达成** | 集成：`TestRetryBackoffIsVisibleAndNotClaimed` 的「退避未到期时 `ClaimNextPending` 不返回」；e2e：同名用例（第二跳停在 `pending`、`nextAttemptAt` 在未来、命令只跑了一次） |
| 4 | **达成** | 集成：`TestRetryStopsAtMaxAttempts`（恰好 3 跳、最后一跳保留原始 `errorCode`）；单元：`TestOperationRetryExhausted` |
| 5 | **达成** | 集成：`TestRetrySkipsNonRetryableCode`、`TestRetryNarrowsWhitelist`；单元：`TestRetryPolicyValidate`（不可重试的码无法入白名单）、`TestRetryPolicyRetryable`（不可重试的码无法被白名单救回来） |
| 6 | **达成** | 集成：`TestRetryWaitsForResourceLock`（占锁时领不到、释放后领得到、命令恰好执行两次） |
| 7 | **达成** | 集成：`TestRetryRejectsDryRun` |
| 8 | **达成** | 集成：`TestRecoveryRetriesOnlyOperationsWithPolicy`（两个子用例：声明了策略的排一次重试、未声明的一字不变） |
| 9 | **达成** | 集成：`TestCancelledOperationIsNotRetried` |
| 10 | **达成** | **Linux 容器**：`test/linux/verify.sh` 的 `check_retry` 三个夹具（就绪永不通过 → `runtime.start` 成功但健康报 `RUNTIME_NOT_READY`、链上无重试；`runtime stop` 未 `Prepare` 的应用 → 可重试、链上两行；已就绪过再被 `SIGKILL` → unit 由 systemd 重启、链上无重试） |
| 11 | **达成** | 集成 + e2e：每次尝试都有独立的审计与日志（`insertRetryOperation` 写入 `operation.retried` 与独立日志行）；`operation get` 的 JSON 与人类可读输出都带 `attempt` / `nextAttemptAt`（e2e 两个用例断言） |
| 12 | **达成** | `make ci` 与 `make verify-linux` 全绿；迭代 0/1a/1b/1c 的回归用例全部继续通过 |

**未验证项**见第 14 节末尾。**下一步**：迭代 2 的规格已冻结，按「先 1d、再 2a」的顺序开始
实现 2a——迭代 2 要求的「失败可重试」现在可以直接复用本迭代的机制，不需要再定义一套。
