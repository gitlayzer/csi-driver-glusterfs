# 决策记录

由项目负责人拍板，2026-10-31。

| # | 事项 | 决定 | 说明 |
|---|---|---|---|
| D-1 | 仓库 / Go module | `github.com/gitlayzer/csi-driver-glusterfs` | |
| D-2 | CSI 驱动名 | `glusterfs.csi.io` | 可通过 `--drivername` 覆盖；已知该域名不归本项目所有，接受潜在重名风险 |
| D-3 | 镜像仓库 | `ghcr.io/gitlayzer/glusterfs-csi` | |
| D-4 | License | Apache-2.0 | |
| D-5 | Node 默认挂载方式 | 容器内 FUSE + 挂载恢复器 | 宿主机挂载模式作为 M5 可选项 |
| D-6 | Kubernetes 支持范围 | 1.28 – 1.37 | sidecar 版本需选用同时兼容该区间的版本；CSIDriver/RBAC 只使用 1.28 已 GA 的 API |
| D-7 | 提交流程 | 任务验收通过即本地 commit，不自动 push；里程碑完成时询问是否 push | |
| D-8 | 贡献协议 | DCO（commit 需 `Signed-off-by`，CI 检查 PR 中的提交） | |
| D-9 | 镜像发布 | main 推送 → `:main`、`:sha-<短 sha>`；`v*` tag → 语义化版本标签；PR 只构建不推送 | |
| D-10 | main 分支保护 | 暂不开启，M2 完成后开启“必须 PR + CI 通过” | |
| D-11 | 与远程初始提交合并 | 本地提交变基到远程 `Initial commit`（含 GitHub 生成的 LICENSE）之上，不强推 | |
| D-12 | Gluster 节点命名 | 以主机名为准（peer、brick 均用主机名），不改主机名；各节点 `/etc/hosts` 写入映射：dxt-gluster-node-1=192.168.129.2、node-2=192.168.129.3、node-3=192.168.129.1 | IP 顺序与主机名不对应属正常 |
| D-13 | GlusterFS 服务端版本 | Ubuntu 24.04 自带 11.1（apt `glusterfs-server`） | 兼容性测试另覆盖 9/10/11 |
