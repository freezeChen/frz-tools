-- 把既有时间列统一规范化成**定宽**格式。
--
-- 背景：0001–0005 期间所有时间列都用 time.RFC3339Nano 写入，而它会裁掉末尾的零、并在小数
-- 部分为零时把小数点整段省略。于是同一秒内 "…T00:00:00Z" 按字典序**大于** "…T00:00:00.5Z"，
-- 但时间上更早。SQLite 比的是字符串，凡按时间列排序或比较的地方都会因此出错：
--   * operations 的领取顺序（ClaimNextPending 的 `ORDER BY created_at`）与
--     RecoverRunning 的重放顺序；
--   * not_before 的 `<=` 过滤（0005 起就写定宽，但同库里混着可变宽度的 created_at）；
--   * schedule_runs 的 (schedule_id, scheduled_for) 去重与 schedule_runs 的列表顺序；
--   * 各类列表查询的 `ORDER BY created_at`。
--
-- 写入侧已在同一提交里改成定宽（internal/adapters/sqlite/store.go 的 timeLayout），本迁移
-- 负责把**已经写在库里**的可变宽度值改写成同一种格式。不做这一步，老库升级后新旧行混在一起，
-- 排序仍然是错的（而且 schedule_runs 的去重会按字符串比较而认不出同一个时刻）。
--
-- 表达式（下文每列复用，只在这里解释一次）：
--     substr(ts, 1, 19) || '.' || substr(substr(rtrim(ts, 'Z'), 21) || '000000000', 1, 9) || 'Z'
-- 即「秒级前缀 + 小数点 + 右补零到 9 位的小数部分 + Z」。rtrim 去掉尾部的 'Z' 后，第 21 位起
-- 就是小数部分（没有小数部分时取到空串，补零成全零）。对已经是定宽的值它是**恒等变换**，
-- 因此本迁移可重复执行，也不需要「跑过没有」的状态标记。
--
-- WHERE 子句只挑「本工具写出的 UTC 形态」：第 20 位是 'Z'（无小数部分）或 '.'（有小数部分）。
-- 带时区偏移（'+'/'-'）、长度异常、或本身就是 NULL 的值一律不动——它们不是本工具写的，
-- 按上面的表达式改写只会得到垃圾。NULL 行因 `substr(NULL,…) IN (…)` 为 NULL 而天然被跳过。

UPDATE operations SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE operations SET
    started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(started_at, 20, 1) IN ('Z', '.');
UPDATE operations SET
    finished_at = substr(finished_at, 1, 19) || '.' || substr(substr(rtrim(finished_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(finished_at, 20, 1) IN ('Z', '.');
-- not_before 从 0005 落地起就是定宽，这条是恒等变换；保留它是为了让「所有时间列同一个格式」
-- 这件事在迁移里也是完整的，而不是一条只能靠读代码才成立的例外。
UPDATE operations SET
    not_before = substr(not_before, 1, 19) || '.' || substr(substr(rtrim(not_before, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(not_before, 20, 1) IN ('Z', '.');

UPDATE operation_logs SET
    ts = substr(ts, 1, 19) || '.' || substr(substr(rtrim(ts, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(ts, 20, 1) IN ('Z', '.');

UPDATE resource_locks SET
    acquired_at = substr(acquired_at, 1, 19) || '.' || substr(substr(rtrim(acquired_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(acquired_at, 20, 1) IN ('Z', '.');
UPDATE resource_locks SET
    released_at = substr(released_at, 1, 19) || '.' || substr(substr(rtrim(released_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(released_at, 20, 1) IN ('Z', '.');

UPDATE audit_events SET
    ts = substr(ts, 1, 19) || '.' || substr(substr(rtrim(ts, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(ts, 20, 1) IN ('Z', '.');

UPDATE artifacts SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE artifacts SET
    deleted_at = substr(deleted_at, 1, 19) || '.' || substr(substr(rtrim(deleted_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(deleted_at, 20, 1) IN ('Z', '.');

UPDATE applications SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE applications SET
    updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(updated_at, 20, 1) IN ('Z', '.');

UPDATE releases SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');

UPDATE application_specs SET
    updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(updated_at, 20, 1) IN ('Z', '.');

UPDATE hosts SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE hosts SET
    updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(updated_at, 20, 1) IN ('Z', '.');

UPDATE environments SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE environments SET
    updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(updated_at, 20, 1) IN ('Z', '.');

UPDATE schedules SET
    created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(created_at, 20, 1) IN ('Z', '.');
UPDATE schedules SET
    updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(updated_at, 20, 1) IN ('Z', '.');
UPDATE schedules SET
    next_run_at = substr(next_run_at, 1, 19) || '.' || substr(substr(rtrim(next_run_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(next_run_at, 20, 1) IN ('Z', '.');
UPDATE schedules SET
    last_run_at = substr(last_run_at, 1, 19) || '.' || substr(substr(rtrim(last_run_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(last_run_at, 20, 1) IN ('Z', '.');

-- scheduled_for 同时是 (schedule_id, scheduled_for) 唯一索引的一列，去重走的是字符串相等，
-- 所以这一列尤其必须与写入侧逐字符一致。
UPDATE schedule_runs SET
    scheduled_for = substr(scheduled_for, 1, 19) || '.' || substr(substr(rtrim(scheduled_for, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(scheduled_for, 20, 1) IN ('Z', '.');
UPDATE schedule_runs SET
    started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(started_at, 20, 1) IN ('Z', '.');
UPDATE schedule_runs SET
    finished_at = substr(finished_at, 1, 19) || '.' || substr(substr(rtrim(finished_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(finished_at, 20, 1) IN ('Z', '.');

-- schema_migrations.applied_at 只是记录，没有比较语义；一并规范化，避免留下「只有它还是
-- 可变宽度」的例外。
UPDATE schema_migrations SET
    applied_at = substr(applied_at, 1, 19) || '.' || substr(substr(rtrim(applied_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE substr(applied_at, 20, 1) IN ('Z', '.');
