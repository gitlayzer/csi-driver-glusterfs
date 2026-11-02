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
| D-19 | 插件重启后业务 Pod 的 I/O 恢复范围 | 恢复器在宿主机上重挂失效的 staging 挂载，并在各 Pod 失效的 bind 挂载**之上叠加**一层新的 bind（不先卸载）；业务 Pod 的 volumeMount 声明 `mountPropagation: HostToContainer` 时 I/O 自动恢复，未声明的 Pod 需重启容器（建议配存活探针）；完全无感由 M5 hostMounter 提供 | 容器在创建时把挂载复制进自己的 mount namespace（默认 private），宿主机侧的重挂不会传播进去；不删除业务 Pod（需要 Pod 删除权限且可能误伤有状态应用）；T-09 实测：HostToContainer 容器里的挂载是宿主机 target 的 rslave 副本，宿主机 detach target 后重新 bind 不会传播进容器，叠加的新层会作为子挂载传播进去（T-08b）；代价是每次插件重启 target 上多一层挂载，NodeUnpublish 循环卸载全部层 |
| D-20 | 恢复器触发方式与本地记录 | 启动时扫描一次，之后按 `--recovery-interval`（默认 10s，0 表示只在启动时；T-09 实测启动那一轮可能与 FUSE 进程退出竞态而漏判，10s 让漏判最多延迟一个短周期，T-08b）定期扫描；NodeStage 成功后在 `--state-dir`（默认 `/var/lib/kubelet/plugins/glusterfs.csi.io/staged`，空值关闭记录与恢复）写一份 staging 记录，NodeUnstage 成功后删除 | 记录只含 volume_id、staging 路径、只读标志、mount_flags 与恢复中断时尚待刷新的 bind target；servers 每次从当前 ConfigMap 取；mount-utils `GetMountRefs` 对损坏挂载返回空列表，因此 bind target 由解析 mountinfo（同 major:minor 与 root）得到 |
| D-21 | node 插件解析 Gluster 主机名的方式 | node DaemonSet 使用 `hostNetwork: true` 与 `dnsPolicy: ClusterFirstWithHostNet`；hostNetwork Pod 的 `/etc/hosts` 由 kubelet 从节点复制，因此只要 K8s 节点能解析 Gluster 主机名（DNS 或节点 `/etc/hosts`），FUSE 客户端就能连到 volfile 中按主机名列出的 brick | 与 D-12 配套；前置条件写入 usage.md；healthz 端口占用节点端口，选 29651 以避开常见 CSI 驱动的 9808 |
| D-22 | StorageClass 参数格式与校验 | 参数在 T-10 定义为纯函数解析，T-11 CreateVolume 与 ValidateVolumeCapabilities 共用：`clusterID`（必填）、`provisioningMode`（缺省 `subdir`；`static` 拒绝，`volume` 在 M3 前拒绝）、`parentVolume`（subdir 必填）、`onDelete`（`delete`/`retain`/`archive`，缺省 `delete`）、`quota`（`"true"`/`"false"`，`"true"` 在 M3 前拒绝）；key 区分大小写，不认识的 key 返回 `InvalidArgument`，`csi.storage.k8s.io/` 前缀的 sidecar 参数放行并忽略 | 拼错的参数（如 `onDelte`）立即暴露，而不是静默走缺省值；`quota: "true"` 若被忽略，用户会误以为容量受限 |
| D-23 | M2 Controller 骨架的范围 | T-10 的 ValidateVolumeCapabilities 只按 volume_id 能否解码、clusterID 是否在当前配置中判断卷是否存在（否则 `NotFound`），不访问 Gluster；T-11 控制器能挂载父卷后补真实的目录存在检查。`CREATE_DELETE_VOLUME` 在 T-11 实现 CreateVolume/DeleteVolume 时才声明，T-10 只声明 `SINGLE_NODE_MULTI_WRITER` | 不声明尚未实现的能力；T-10 保持纯逻辑、可单测 |
| D-24 | Controller 访问父卷的方式 | 每个 `clusterID`+父卷只挂载一次，挂载点为 `--controller-mount-dir`（默认 `/var/lib/glusterfs-csi/controller`）下的 `<clusterID>/<parentVolume>`，进程内缓存并保持挂载；每次使用前用 `VolumeMountState` 检查，失效则 Detach 后重挂，进程退出时尽力卸载。目录操作全部经由挂载点上打开的 `os.Root` 进行 | 创建为毫秒级，突发大量 PVC 也只有一个 FUSE 进程；`os.Root` 保证路径不逃出父卷，父卷被重挂后旧句柄上的操作只会失败，不会落到挂载点下的本地目录 |
| D-25 | subdir 卷的元数据与父卷布局 | 父卷根目录只放卷目录 `<name>`（权限 0777）；驱动自己的数据放在隐藏目录 `.glusterfs-csi/`：`volumes/<name>.json` 记录容量与 `onDelete`（先写临时文件、fsync、再 rename），`archive/` 放归档目录。CreateVolume 先写元数据再建目录，DeleteVolume 先处理目录再删元数据；名称以 `.` 开头的卷被拒绝 | DeleteVolume 只拿得到 volume_id，`onDelete` 必须持久化；同名重复创建要能判断容量/参数是否兼容（`AlreadyExists`）；有目录无元数据说明目录不是本驱动建的，绝不接管或删除 |
| D-26 | onDelete: archive 的去处 | 重命名为 `.glusterfs-csi/archive/<name>-<UTC 时间戳，纳秒精度>`，不放在父卷根目录 | 父卷根目录只剩活着的卷，归档目录不会与新卷重名；同一卷名反复创建删除也不会撞名 |
| D-27 | 新建子目录的权限 | 0777（显式 chmod，不受 umask 影响） | 与 nfs-subdir-external-provisioner 一致，任意 UID 的 Pod 开箱可写；业务 Pod 只挂载自己的子目录，看不到其它卷；需要更严格权限的 Pod 可设置 fsGroup（CSIDriver `fsGroupPolicy: File`） |
| D-28 | Controller 部署形态 | Deployment `glusterfs-csi-controller` 2 副本，csi-provisioner v6.4.0 以 `--leader-election` 选主（Lease 在 `glusterfs-csi` 命名空间），`RollingUpdate`（`maxSurge: 0`、`maxUnavailable: 1`）；`hostNetwork: true` + `dnsPolicy: ClusterFirstWithHostNet`，与 node 插件一样用节点 `/etc/hosts` 解析 Gluster 主机名（同 D-21）；端口：驱动 healthz（livenessprobe sidecar）29652、provisioner 的 metrics 与选主健康检查 29653，避开 node 插件的 29651；父卷挂载点 `--controller-mount-dir` 用 emptyDir，挂载传播为 None；驱动容器特权（`/dev/fuse` 与 mount 系统调用）；provisioner `--timeout=60s`。RBAC：ClusterRole 照搬上游 v6.4.0 `deploy/kubernetes/rbac.yaml` 的规则（不含 secrets），命名空间 Role 只给 leases（M3 启用 `GET_CAPACITY` 前不需要 csistoragecapacities） | 备用副本不挂载父卷（只有 leader 的 provisioner 发 RPC），切换约 15s；hostNetwork 的 Pod 声明的端口即宿主机端口，两个副本天然不会调度到同一节点，`maxSurge: 0` 保证 2～3 节点的集群也能滚动；第一次 CreateVolume 要挂载父卷，15s 默认超时偏紧；与上游 RBAC 一致便于升级时比对 |
| D-29 | 示例 StorageClass | `examples/dynamic` 的 StorageClass `glusterfs-subdir`：`onDelete: archive`、`reclaimPolicy: Delete`、`volumeBindingMode: Immediate`、`allowVolumeExpansion: false`（M3 实现扩容并部署 external-resizer 后改为 true）；示例业务 Pod 以非 root（UID 1000）运行 | 用户照抄示例时误删 PVC 仍能从 `.glusterfs-csi/archive/` 找回数据（与 nfs-subdir 的 archiveOnDelete 习惯一致）；没有 resizer 时开启扩容会让 PVC 扩容请求永远挂起；非 root 写入验证 D-27 的 0777 |
