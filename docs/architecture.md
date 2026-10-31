# GlusterFS CSI Driver — 架构设计

> 状态：Draft v0.1（2026-10-31） · 角色：架构 / 需求 / 任务拆分

## 1. 背景与定位

| 历史方案 | 现状 | 教训 |
|---|---|---|
| in-tree `kubernetes.io/glusterfs` | K8s 1.25 弃用、1.26 移除 | 必须走 CSI |
| Heketi（REST 管理层） | 已归档，仅维护模式 | 额外有状态组件（自带 DB）是长期维护负担 |
| gluster/gluster-csi-driver | 依赖 GlusterD2，GD2 已死，项目停更 | 不要绑定一个独立演进的控制面 |

**本项目目标**：一个**无状态、无额外数据库、只依赖原生 glusterd + gluster CLI** 的 CSI 驱动，严格遵循 CSI spec v1.x 与 kubernetes-csi 社区的 sidecar 体系，可长期维护。

**风险提示**：GlusterFS 上游活跃度下降（Red Hat 已结束 RHGS），驱动设计上要把对 Gluster 的依赖收敛在 `pkg/gluster` 一层，并只使用稳定多年的 CLI 子命令（`--xml --mode=script` 输出）。

## 2. 设计原则

1. **严格 CSI 合规**：所有 RPC 幂等、错误码按 spec 返回、能力如实声明；以 `csi-sanity` 和 Kubernetes external storage e2e 作为合规基线。
2. **控制器无状态**：所有删除/扩容需要的信息编码在 `volume_id` 中；集群连接信息放在 ConfigMap（按 `clusterID` 索引），不写进 PV。
3. **不重复造轮子**：provisioning / resize / snapshot / registrar / liveness 全部使用官方 sidecar，驱动本身只实现 CSI gRPC。
4. **最小权限、可选依赖**：最小可用模式（静态卷 + 子目录动态卷）不需要 SSH 到 Gluster 节点；需要配额/独立卷/快照时才启用 Executor。
5. **挂载不随插件生命周期丢失**：FUSE 进程重启导致 `Transport endpoint is not connected` 是此类驱动的头号事故，必须有设计级的应对。

## 3. 总体架构

```
                         Kubernetes 集群
 ┌──────────────────────────────────────────────────────────────────┐
 │  Controller Deployment (replicas=2, leader election 由 sidecar 做)  │
 │  ┌───────────────────┐ ┌──────────────┐ ┌──────────────────────┐  │
 │  │external-provisioner│ │external-resizer│ │external-snapshotter* │  │
 │  └─────────┬─────────┘ └──────┬───────┘ └──────────┬───────────┘  │
 │            └────── unix socket (CSI gRPC) ──────────┘              │
 │                  ┌──────────────────────────────┐                  │
 │                  │ glusterfs-csi (controller)   │── FUSE 挂载父卷 ──┼──┐
 │                  │  Identity + Controller       │── Executor(SSH) ──┼─┐│
 │                  └──────────────────────────────┘                  │ ││
 │                                                                    │ ││
 │  Node DaemonSet (每个 K8s 节点)                                     │ ││
 │  ┌──────────────────────┐ ┌──────────────────────────────┐         │ ││
 │  │node-driver-registrar │ │ glusterfs-csi (node)         │─ FUSE ──┼─┼┤
 │  └──────────────────────┘ │  Identity + Node             │         │ ││
 │  ┌──────────────┐         │  mount.glusterfs / 恢复器     │         │ ││
 │  │livenessprobe │         └──────────────────────────────┘         │ ││
 │  └──────────────┘                                                  │ ││
 └────────────────────────────────────────────────────────────────────┘ ││
                                                                         ││
     GlusterFS 可信存储池 (3 节点)   ◀── gluster CLI (仅需配额/独立卷时) ──┘│
     glusterd :24007  bricks :49152+ ◀── FUSE 数据面 ──────────────────────┘
```
`*` snapshotter 仅在 volume 模式 + LVM thin brick 下启用。

CSIDriver 对象：
```yaml
attachRequired: false          # 网络文件系统，无 attach 阶段，不需要 external-attacher
podInfoOnMount: false
fsGroupPolicy: File            # Gluster 支持 chown/chmod
volumeLifecycleModes: [Persistent]
requiresRepublish: false
```

## 4. 卷供应模式

通过 StorageClass `parameters.provisioningMode` 选择，三种模式共用同一套 Node 侧逻辑。

### 4.1 static（静态卷，M1）
管理员已有 Gluster 卷，手写 PV：
```yaml
csi:
  driver: <DRIVER_NAME>
  volumeHandle: gfs:v1:static:<clusterID>:<glusterVolume>[:<subdir>]
  volumeAttributes: { clusterID: prod, volume: shared-vol }
```

### 4.2 subdir（共享卷子目录，M2，默认模式）
- 每个 PVC = 预建大卷 `parentVolume` 下的一个子目录 `/<pvName>`。
- Controller 以 FUSE 挂载父卷，`mkdir` / `rm -rf`（或归档重命名）。**不需要 SSH**。
- Node 侧使用 Gluster 原生子目录挂载 `host:/parentVolume/pvName`，客户端只看得到自己的目录。
- 可选 `quota: "true"`：通过 Executor 执行 `gluster volume quota <vol> limit-usage /<dir> <size>` 实现容量强制和扩容。
- 优点：秒级创建、可支撑数千 PV、不增加 brick 进程数。缺点：隔离性弱于独立卷，不支持快照。

### 4.3 volume（每 PVC 一个独立 Gluster 卷，M3/M4）
- Executor 在每个 Gluster 节点上准备 brick → `gluster volume create <name> replica 3 ...` → start → 设置卷选项。
- `brickBackend: directory`（在已有 XFS 上建目录，配额靠 quota）或 `lvm-thin`（每 brick 一个 thin LV，容量天然隔离，支持 gluster snapshot / clone）。
- 优点：强隔离、可快照。缺点：每卷多一组 brick 进程（建议开启 `cluster.brick-multiplex`），创建慢。

## 5. 关键设计

### 5.1 volume_id 编码（无状态的核心）
```
gfs:v1:<mode>:<clusterID>:<glusterVolume>[:<subdir>]
```
- 带版本号 `v1`，未来格式变化可向后兼容解析。
- `clusterID` → ConfigMap `glusterfs-csi-config` 中的条目：`servers`（多个，用于 `backup-volfile-servers`）、`executorSecretRef`、默认挂载选项。服务器 IP 变化只改 ConfigMap，不需要改 PV。
- 所有字段经白名单正则校验（`^[A-Za-z0-9._-]{1,128}$`），**拒绝任何可能进入 shell 的特殊字符**。

### 5.2 Executor（Gluster 管理通道）
```go
type Executor interface {
    Run(ctx context.Context, host string, args ...string) (stdout []byte, err error)
}
```
- MVP 实现：SSH（`golang.org/x/crypto/ssh`），参数逐个 shell-quote，绝不拼接字符串。
- 只调用 `gluster --mode=script --xml ...`，用 XML 解析结果，不解析人类可读输出。
- 主机故障时按 ConfigMap 中 servers 顺序故障转移（任一 glusterd 都可执行管理命令）。
- 安全：提供 `hack/gluster-csi-wrapper.sh`，配合 `authorized_keys` 的 `command=` 限制该密钥只能执行白名单 gluster/lvm 命令。
- 预留第二实现：Gluster 节点上的轻量 agent（gRPC + mTLS），不在早期里程碑内。

### 5.3 Node 挂载与故障恢复
- 实现 `STAGE_UNSTAGE_VOLUME`：NodeStage 在 staging 路径做一次 FUSE 挂载；NodePublish 只做 bind mount（多个 Pod 共享同一 FUSE 进程）。
- 挂载参数：`backup-volfile-servers=<其余 servers>`、`log-level=WARNING`、`log-file=/var/log/glusterfs/csi/<vol>.log`，以及 SC `mountOptions` 透传（白名单）。
- **FUSE 进程存活问题**：容器内 FUSE 进程随 node 插件 Pod 一起退出。分两阶段解决：
  1. M1：容器内挂载 + 恢复器。DaemonSet `updateStrategy: OnDelete`；插件启动时扫描 kubelet 目录中本驱动的 staging 挂载，对 `ENOTCONN` 的挂载点 lazy-unmount 后按本地记录的参数重挂，再刷新 bind mount；`NodeGetVolumeStats` 通过 `VOLUME_CONDITION` 上报异常。
  2. M5：`hostMounter`——通过 `nsenter -t 1 -m` 在宿主机 mount namespace 调用宿主机的 `mount.glusterfs`，FUSE 进程归属宿主机，插件升级不影响已挂载卷（需要宿主机安装 glusterfs-fuse，作为可选模式）。
- Mounter 接口隔离，单元测试使用 fake mounter（`k8s.io/mount-utils` 的 FakeMounter）。

### 5.4 CSI 能力声明
| 服务 | 能力 | 里程碑 |
|---|---|---|
| Identity | `CONTROLLER_SERVICE`（不声明 `VOLUME_ACCESSIBILITY_CONSTRAINTS`，Gluster 对全部节点可达） | M0 |
| Identity | `VolumeExpansion: ONLINE` | M3 |
| Controller | `CREATE_DELETE_VOLUME` | M2 |
| Controller | `EXPAND_VOLUME`（quota / LV 扩容，无需 Node 扩容） | M3 |
| Controller | `SINGLE_NODE_MULTI_WRITER` | M2 |
| Controller | `GET_CAPACITY`（父卷剩余 / VG 剩余） | M3 |
| Controller | `CREATE_DELETE_SNAPSHOT`, `CLONE_VOLUME`（仅 volume+lvm-thin） | M4 |
| Node | `STAGE_UNSTAGE_VOLUME`, `GET_VOLUME_STATS`, `VOLUME_CONDITION`, `SINGLE_NODE_MULTI_WRITER` | M1 |

访问模式：RWX / ROX / RWO / RWOP；`volumeMode: Filesystem`（不支持 Block，CreateVolume 收到 Block 返回 `InvalidArgument`）。

### 5.5 并发与幂等
- 以 volume 名 / volume_id 为键的 in-flight 锁（同 kubernetes-csi 惯例），冲突返回 `Aborted`。
- CreateVolume：同名且参数一致返回已有卷；同名但容量/参数不兼容返回 `AlreadyExists`。
- DeleteVolume：卷不存在返回成功。
- Controller 多副本由 sidecar 做 leader election，驱动本身不需要选主。

### 5.6 删除策略
SC 参数 `onDelete: delete | retain | archive`（archive = 重命名为 `archived-<pv>-<时间戳>`），默认 `delete`。

### 5.7 可观测性
- klog 结构化日志，每个 RPC 记录 method / volume_id / 耗时 / 结果码；Secret 内容绝不入日志。
- Prometheus 指标：`glusterfs_csi_operations_total{method,code}`、`..._duration_seconds`、`..._executor_errors_total`、`..._stale_mounts_recovered_total`。
- livenessprobe sidecar + `Probe` RPC（Node 侧检查 `/dev/fuse` 与 `mount.glusterfs` 可用）。

## 6. 仓库结构
```
cmd/glusterfs-csi/            # 单二进制，--mode=controller|node|all
pkg/driver/                   # identity.go controller.go node.go server.go
pkg/volumeid/                 # volume_id 编解码与校验
pkg/config/                   # clusterID ConfigMap 加载（文件挂载 + 热更新）
pkg/gluster/                  # Executor、CLI 封装、XML 解析、quota/volume/snapshot 管理
pkg/mounter/                  # Mounter 接口、容器内实现、host 实现、恢复器
pkg/metrics/
deploy/kustomize/  deploy/helm/glusterfs-csi/
test/sanity/  test/e2e/       # csi-sanity、K8s external storage e2e testdriver.yaml
hack/                         # 本地 gluster 测试环境、wrapper 脚本
docs/
```

## 7. 兼容性矩阵（目标）
| 组件 | 版本 |
|---|---|
| CSI spec | v1.9+（使用当时最新稳定版 Go 绑定） |
| Kubernetes | 1.28 – 1.37（见 decisions.md D-6） |
| GlusterFS 服务端 | 9.x / 10.x / 11.x |
| 镜像架构 | linux/amd64, linux/arm64 |

## 8. 已定决策
见 [decisions.md](decisions.md)：module `github.com/gitlayzer/csi-driver-glusterfs`、驱动名 `glusterfs.csi.io`、镜像 `ghcr.io/gitlayzer/glusterfs-csi`、Apache-2.0、K8s 1.28–1.37。
