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
| D-14 | Go 最低版本（`go.mod` 的 `go` 指令） | 1.26 | 跟随 Go 官方仍在维护的最老版本；CI 与镜像构建使用最新稳定版（当前 1.27） |
| D-15 | Dependabot 依赖升级 PR | 由架构师处理：`@dependabot rebase` 等指令 → 本地跑完整门禁 → CI 全绿 → 在对话中告知 → squash 合并 | 涉及 `go.mod` 的多个 PR 逐个合并，后续 PR 由 Dependabot 自动变基 |
| D-16 | Node 侧不支持的卷能力的错误码 | `NodeStageVolume` / `NodePublishVolume` 遇到 Block、非 glusterfs 的 fs_type、不支持的访问模式返回 `FAILED_PRECONDITION`（CSI 规范错误表 "Exceeds capabilities"）；Controller 侧仍为 `INVALID_ARGUMENT` | 缺字段、`UNKNOWN`/无法识别的访问模式属于请求畸形，仍为 `INVALID_ARGUMENT`；AGENTS.md 错误码表同步更新 |
| D-17 | 单节点访问模式下同一卷 Publish 到第二个 target | `SINGLE_NODE_WRITER`、`SINGLE_NODE_READER_ONLY`、`SINGLE_NODE_SINGLE_WRITER` 返回 `FAILED_PRECONDITION`；`SINGLE_NODE_MULTI_WRITER` 与 `MULTI_NODE_*` 放行（CSI 规范 NodePublishVolume 错误表，声明了 SINGLE_NODE_MULTI_WRITER 时） | 依据挂载表判断（mount-utils `GetMountRefs`，Linux 按 mountinfo 的 root + major:minor 匹配），无内存状态，插件重启后仍有效；同一 target 重复调用仍幂等 |
| D-18 | 卷健康上报方式 | CSI v1.13 已删除 `VOLUME_CONDITION`（能力枚举 4 与 `NodeGetVolumeStatsResponse` 字段 2 均为 reserved），改为实现 alpha 的 `NodeGetVolumeHealth` 并声明 `GET_VOLUME_HEALTH`：失效挂载报 `INACCESSIBLE`；`NodeGetVolumeStats` 只返回 statfs 用量，挂载失效时返回 `Internal` | 不用原始枚举值或 unknown fields 伪造旧字段；kubelet 何时调用 NodeGetVolumeHealth 取决于上游，失效挂载的自愈由 T-08 恢复器负责 |
| D-19 | 插件重启后业务 Pod 的 I/O 恢复范围 | 恢复器在宿主机上重挂失效的 staging 挂载并刷新各 Pod 的 bind 挂载；业务 Pod 的 volumeMount 声明 `mountPropagation: HostToContainer` 时 I/O 自动恢复，未声明的 Pod 需重启容器（建议配存活探针）；完全无感由 M5 hostMounter 提供 | 容器在创建时把挂载复制进自己的 mount namespace（默认 private），宿主机侧的重挂不会传播进去；不删除业务 Pod（需要 Pod 删除权限且可能误伤有状态应用） |
| D-20 | 恢复器触发方式与本地记录 | 启动时扫描一次，之后按 `--recovery-interval`（默认 60s，0 表示只在启动时）定期扫描；NodeStage 成功后在 `--state-dir`（默认 `/var/lib/kubelet/plugins/glusterfs.csi.io/staged`，空值关闭记录与恢复）写一份 staging 记录，NodeUnstage 成功后删除 | 记录只含 volume_id、staging 路径、只读标志、mount_flags 与恢复中断时尚待刷新的 bind target；servers 每次从当前 ConfigMap 取；mount-utils `GetMountRefs` 对损坏挂载返回空列表，因此 bind target 由解析 mountinfo（同 major:minor 与 root）得到 |
