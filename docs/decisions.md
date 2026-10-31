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
