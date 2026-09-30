# frz-tools

Go 实现的 Linux 运维工具：**应用部署与回滚、Nginx 蓝绿发布、多主机批量发布、备份恢复与
定时计划**，由两个二进制组成——

- **`opsd`**：跑在目标主机上的守护进程，执行一切需要权限的动作。锁、状态机、审计、日志
  都在它那里；同一资源任意时刻最多一个未完成操作，执行器只接受 argv、永不经过 shell。
- **`opsctl`**：用户 CLI，发起操作、查询状态与日志。两者通过 Unix socket（默认
  `/run/opsd/opsd.sock`）上的 HTTP/JSON `api/v1` 协议通信；开远程后走 mTLS TCP
  （`opsctl --host` / `--remote`）。

## 30 秒上手

```bash
make build                                   # 构建本机二进制到 output/
opsctl init /etc/opsd/opsd.yaml              # 生成配置骨架（本地动作，不连 opsd）
vim /etc/opsd/opsd.yaml                      # 按需调整路径与配额
opsctl config validate --file /etc/opsd/opsd.yaml
sudo opsd --config /etc/opsd/opsd.yaml       # 前台启动；生产建议装成 systemd 服务
opsctl status                                # 健康四项 + 最近操作一屏
```

第一个部署、蓝绿、批量发布、备份恢复的完整流程见 **[使用手册](docs/manual.md)**。

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/manual.md](docs/manual.md) | **使用手册**：安装、配置参考、各工作流、退出码表、排障，附录为全部命令与旗标速查 |
| [AGENTS.md](AGENTS.md) | 仓库协作约定、架构不变量、证据边界（哪些验证过、哪些没有） |
| [docs/plans/](docs/plans/) | 设计与迭代记录：总路线图、各迭代规格与实现/验证记录 |

## 开发

```bash
make ci            # fmt + vet + test + test-race + cross（提交前必须通过）
make verify-linux  # Linux 容器验证（文件模式、socket ACL、systemd 等，需要 docker）
make verify-db     # 真实 PostgreSQL/MySQL/MariaDB 上的备份适配器合约（需要 docker）
make verify-host   # 真实 Linux 主机验证（需要一台能 ssh 的目标主机，不进 CI）
```

证据边界是本仓库的硬纪律：「Linux 容器」与「Linux 主机」是两类证据，引用任何验证结论前
先看 AGENTS.md 的「验证约定」一节。
