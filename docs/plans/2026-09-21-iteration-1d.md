# Linux 迭代 1d：任务引擎的重试、退避与并发策略

> 文档日期：2026-09-24
> 文档状态：**规格待冻结**——第 12 节的未决事项需先拍板，之后才进入实现
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
| 1–12 | **未实现** | 本规格尚未冻结（第 12 节） |

## 11. 未验证内容

本迭代**尚未实现**，因此第 10 节全部条目当前均为「未实现」。实现后按证据类型逐条填写；
以下是**已知将来也不能由 1d 的常规证据覆盖**的项：

- 真实主机的长退避（分钟级）在长时间运行下的行为——容器能验短退避，长时间稳定性需真机。
- 与真实 systemd 应用崩溃策略（`RestartSec`、`StartLimitBurst`）叠加后的实际表现，
  在**不同发行版**上的一致性（真机项，与 1c 的挂起项同源）。
- 远程/多主机下的重试（迭代 5）。

## 12. 未决事项（进入实现前必须冻结）

以下每条都给了推荐值，采纳与否需要拍板。

1. **退避默认值与上限**。推荐：`base = 5s`、`maxDelay = 5m`、抖动为 `±20%` 的均匀分布、
   `maxAttempts` 上限 `10`。
   *理由*：5s 足够吸收瞬时抖动，5m 上限避免一条链占着一整天；`maxAttempts = 10`
   配 5m 上限约等于 40 分钟收敛，再长就该让人而不是机器来决定。
   *替代方案*：全抖动（full jitter，`rand(0, computed)`）——分布更好，但退避时间的下限
   变成 0，运维更难解释「为什么又立刻跑了」。**推荐前者**。

2. **是否允许请求级覆盖可重试错误码白名单**。推荐：**允许缩小，不允许扩大到不可重试的码**。
   *理由*：见 D3 与第 5 节——「取消也要重试」这类意图不该由调用方表达。

3. **`runtime.*` 是否允许声明重试**。推荐：**允许**，但语义严格按 D6——只有
   「就绪从未通过」才失败、才可重试；已就绪过再崩溃归 unit 的 `Restart=`。
   *替代方案*：`runtime.*` 一律禁止重试。**不推荐**，因为「应用启动慢、首次就绪探测超时」
   正是最需要重试的场景，禁掉等于把最痛的用例排除在外。

4. **重试策略是否要能挂在 `Schedule` / `Application` 上**（而不是每次提交都写）。
   推荐：**1d 不做**，只在 `CreateOperationRequest` 上支持；等迭代 2 的备份计划真正需要时
   再决定挂在 `Schedule` 还是 `Application` 层。
   *理由*：现在就加会在没有真实用例的情况下冻结错层次（见路线图第 13 节的教训：
   迭代 1 的七类交付曾被拆错归属）。

5. **重试链的长度上限**（防止一条链无限增长）。推荐：不额外设链长上限，
   由 `maxAttempts` 本身约束；但 `operation get` 的输出要能看出链，且日志里每次尝试都有
   独立行，避免排查时要挨个查。

## 13. 实现记录

待实现后填写。实现必须在本节记录：逐文件要点、对规格的补充与偏离（若有）、
以及每条偏离的理由。

## 14. 验证记录

待实现后填写。格式沿用 1c 第 17 节：命令 / 结果 / 证据类型三列表格，
且**单元 / 集成 / e2e / Linux 容器 / Linux 主机**分列，不混用。
断言数（若新增容器断言）以 `test/linux/verify.sh` 运行时打印的行为准。

## 15. 结论汇总

**本规格尚未冻结**，第 12 节的 5 条待拍板。在此之前：

- 1d **不进入实现**；
- 迭代 2 的入口条件此前定义为「1a + 1c 完成」，1c 已达成，因此**迭代 2 可以先于 1d 开始**
  （路线图第 13 节 2026-09-22 记录已确认这一点）。
  但重试机制是备份场景的刚需，建议 1d 与迭代 2 的规格一起推进，避免备份先用一套将就的实现。
