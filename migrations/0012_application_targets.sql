-- 部署目标（迭代 5b）：一个应用**应该**跑在哪些主机上。
--
-- 语义是**声明的意图**，不是探测的结果。为什么不用「问每台机有哪些应用」来推导：
-- 那会把「这台机本来就不该有这个应用」与「这台机本该有、但它挂了或者漏了」混成
-- 同一件事，而在批量发布里后者会变成「静默少发一台，然后报成功」。
--
-- 主键是 (应用名, 主机名)：同一对重复登记是幂等的，不需要额外去重。按**应用名**
-- 而不是外键关联本机的 applications 表——登记目标的这台机器上可能根本没有这个应用
-- （它跑在别处），而应用名才是全集群稳定、跨主机一致的标识。
--
-- 写入是**替换**语义（先删后插，同一事务）：这张表表达的是「现在应该在哪几台」，
-- 不是「历史上曾经在哪几台」——历史在 audit_events 里。
CREATE TABLE application_targets (
    application_name TEXT NOT NULL,
    host_name        TEXT NOT NULL REFERENCES hosts (name),
    created_at       TEXT NOT NULL,
    PRIMARY KEY (application_name, host_name)
);

CREATE INDEX ix_application_targets_application
    ON application_targets (application_name);
