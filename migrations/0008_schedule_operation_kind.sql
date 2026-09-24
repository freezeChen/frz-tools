-- 迭代 2a：计划可以声明「触发哪种操作」。
--
-- 在此之前调度器写死了 executor.command（internal/application/scheduler.go），
-- 于是「按计划跑备份」从模型上就做不到（迭代 2 规格 D6）。
--
-- 默认值是 executor.command：省略这个字段的既有计划行为**逐字节不变**，
-- 因此这是同一 apiVersion 内的向后兼容变更，不提升 apiVersion。

ALTER TABLE schedules ADD COLUMN operation_kind TEXT NOT NULL DEFAULT 'executor.command';
