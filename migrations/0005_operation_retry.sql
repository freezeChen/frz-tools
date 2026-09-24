-- 迭代 1d：任务引擎的重试与退避。
--
-- 只新增列与索引，不动任何既有列、不改 resource_locks。所有新列都有默认值，
-- 因此既有行的行为与升级前逐字节一致：attempt = 1 且 retry_policy_json 为空即「不重试」,
-- not_before 为 NULL 即「立即可领取」。

-- attempt 是这条重试链上的第几次尝试（从 1 开始）。
-- 它是**可变的操作状态**，所以是列；而策略本身（含 maxAttempts、退避参数、白名单）
-- 是不可变的提交内容，整体存在 retry_policy_json 里，不拆成列——拆开就多了一份
-- 会与原文漂移的真相来源。
ALTER TABLE operations ADD COLUMN attempt INTEGER NOT NULL DEFAULT 1;

-- not_before 是退避窗口的终点。早于它的 pending 操作不被领取。
--
-- 格式刻意与其它时间列不同：用**定宽**的纳秒格式，因为领取查询要在 SQL 里做
-- `not_before <= ?` 比较，而仓库既有的 timeLayout 是 RFC3339Nano——它会裁掉末尾的零、
-- 甚至在小数部分为零时整个省略，于是 "…T00:00:00Z" 按字典序大于 "…T00:00:00.5Z"，
-- 但时间上更早。字符串序不等于时间序，比较就会错。
-- 定宽格式让字典序与时间序一致，同时保持可读、可被既有的 RFC3339 解析器读出。
ALTER TABLE operations ADD COLUMN not_before TEXT;

-- retry_policy_json 是提交时的策略原文（可为 NULL = 不重试）。
-- 存原文而不是解析后的字段：策略字段增减不需要再加列，且事后能查到
-- 「当时到底按什么策略重试的」。
ALTER TABLE operations ADD COLUMN retry_policy_json TEXT;

-- 领取查询的形状是：status = 'pending' AND (not_before IS NULL OR not_before <= ?)
-- ORDER BY created_at, id。索引覆盖前两段。
CREATE INDEX ix_operations_pending_not_before
    ON operations (status, not_before, created_at);

-- 重试链的查询（「这条链上尝试了几次」「它从哪来」）按 retry_of 走。
CREATE INDEX ix_operations_retry_of
    ON operations (retry_of) WHERE retry_of IS NOT NULL;
