# 使用指南（目标形态）

> 本文描述 v1.0 时用户的使用方式，开发过程中按里程碑逐步变为现实。

## 1. 前置条件

**GlusterFS 侧**
- 一个健康的可信存储池（`gluster peer status` 全部 Connected）。
- 端口：24007/tcp（glusterd）、24008/tcp、49152–60999/tcp（brick 端口，由 glusterd.vol 的 base-port/max-port 决定，Gluster 11 在该区间内随机分配）对所有 K8s 节点开放。
- subdir 模式：预建一个父卷（建议 `replica 3`），例如 `csi-parent`；如需配额：`gluster volume quota csi-parent enable`。
- volume / 配额 / 快照模式：一个可 SSH 登录、可执行 `gluster` 的账号（建议用受限 wrapper，见 `hack/gluster-csi-wrapper.sh`）。

**Kubernetes 侧**
- 所有节点内核支持 FUSE（`/dev/fuse` 存在）。容器内挂载模式下，宿主机无需安装 glusterfs 客户端。
- K8s 节点到 Gluster 节点的上述端口网络可达。
- K8s 节点能解析 Gluster 服务器的主机名（DNS 或节点 `/etc/hosts`）：node 插件使用 hostNetwork，容器内的 `/etc/hosts` 来自节点（D-21），FUSE 客户端按 volfile 中的主机名连接 brick。
- kubelet 根目录为 `/var/lib/kubelet`（k3s、microk8s 等：Helm 设置 `kubeletDir`，kustomize 需修改清单中的 hostPath）。

## 2. 安装

**M1：kustomize（当前可用）**。在 `deploy/kustomize/overlays/` 下复制 `sample` 为自己的 overlay（保持在同级目录，`../../base` 的相对路径才有效），修改 `config.json` 中的 servers，并把镜像 tag 固定为
`sha-<短 sha>` 或发布版本：
```bash
cp -r deploy/kustomize/overlays/sample deploy/kustomize/overlays/mycluster   # 编辑其中的 config.json 与 images.newTag
kubectl apply -k deploy/kustomize/overlays/mycluster
kubectl -n glusterfs-csi get pods -o wide            # 每个节点一个 glusterfs-csi-node（3/3），两个 glusterfs-csi-controller（3/3）
kubectl get csinode -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.drivers[*].name}{"\n"}{end}'
kubectl -n glusterfs-csi get lease                   # 当前持有选主 Lease 的 controller Pod
```
node DaemonSet 使用 `updateStrategy: OnDelete`：升级时先腾空一个节点的业务 Pod，再删除该节点的插件 Pod，逐节点进行。
controller Deployment（M2 起）2 副本、由 csi-provisioner 选主，可直接滚动升级；它与 node 插件一样使用 hostNetwork，
占用宿主机端口 29652、29653，两个副本会被调度到不同节点（D-28）。controller 挂载父卷，同样需要能解析 Gluster 主机名。
静态卷示例见 `deploy/kustomize/examples/static`（修改 PV 的 volumeHandle 后 `kubectl apply -k`）；
动态卷示例见 `deploy/kustomize/examples/dynamic`（修改 StorageClass 的 clusterID 与 parentVolume 后 `kubectl apply -k`）。

**M2：Helm（T-14 起）**。chart 在 `deploy/helm/glusterfs-csi`，默认值渲染出的对象与 kustomize 的 `overlays/sample` 逐字相同（D-32、D-33），
资源名固定，一个集群只能安装一份（kustomize 与 Helm 二选一）。chart 不创建命名空间；启用了 Pod Security Admission 的集群需要先建好并打标签：
```bash
kubectl create namespace glusterfs-csi
kubectl label namespace glusterfs-csi pod-security.kubernetes.io/enforce=privileged
helm install glusterfs-csi deploy/helm/glusterfs-csi -n glusterfs-csi -f my-values.yaml
```
`my-values.yaml` 至少要有 `clusters`（键名与下文的配置文件相同，原样写入 ConfigMap `glusterfs-csi-config`），并建议固定镜像 tag：
```yaml
image:
  tag: sha-<短 sha>          # 默认是 chart 的 appVersion（main）
clusters:
  - clusterID: prod
    servers: [10.0.0.11, 10.0.0.12, 10.0.0.13]
storageClasses:              # 可选，默认不建
  - name: glusterfs-subdir
    parameters: { clusterID: prod, provisioningMode: subdir, parentVolume: csi-parent, onDelete: archive }
```
其它可调项（sidecar 镜像、资源、nodeSelector/tolerations/affinity、`kubeletDir`、`logLevel` 等）见 chart 的 `values.yaml`。
`helm upgrade` 修改 `clusters` 后驱动约 10s 内热加载；node DaemonSet 仍是 `OnDelete`，升级镜像后要逐节点腾空并删除插件 Pod。

集群配置最终写入 ConfigMap `glusterfs-csi-config`（`executorSecretRef` 为 M3 的 volume / 配额 / 快照模式所用）：
```json
[
  {
    "clusterID": "prod",
    "servers": ["10.0.0.11", "10.0.0.12", "10.0.0.13"],
    "executorSecretRef": { "name": "glusterfs-ssh", "namespace": "glusterfs-csi" }
  }
]
```

## 3. 三种使用方式

### 3.1 静态卷（使用已有 Gluster 卷）
```yaml
apiVersion: v1
kind: PersistentVolume
metadata: { name: shared-data }
spec:
  capacity: { storage: 100Gi }
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: glusterfs.csi.io
    volumeHandle: gfs:v1:static:prod:shared-vol
    volumeAttributes: { clusterID: prod, volume: shared-vol }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: shared-data }
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: shared-data
  resources: { requests: { storage: 100Gi } }
```

### 3.2 子目录动态卷（默认推荐）
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: { name: glusterfs-subdir }
provisioner: glusterfs.csi.io
parameters:
  clusterID: prod
  provisioningMode: subdir
  parentVolume: csi-parent
  # quota: "true"          # M3 起支持，需要 executor；M2 设为 "true" 会被拒绝，不设则容量仅为声明值
  onDelete: archive        # delete | retain | archive；缺省 delete，示例用 archive 防误删（D-29）
reclaimPolicy: Delete
allowVolumeExpansion: false  # 扩容在 M3 实现（届时部署 external-resizer 并改为 true）
mountOptions: [log-level=WARNING]
```
`provisioningMode` 缺省为 `subdir`，`onDelete` 缺省为 `delete`。参数名区分大小写；不认识的参数会让创建失败（`InvalidArgument`），以免拼错的参数被静默忽略（D-22）。

父卷 `csi-parent` 需由管理员预先创建并启动。每个 PVC 对应父卷根目录下的一个子目录 `<PV 名>`（权限 0777）；驱动自己的数据放在父卷的隐藏目录 `.glusterfs-csi/` 中：`volumes/` 存每个卷的容量与删除策略，`archive/` 存 `onDelete: archive` 归档的目录（`<PV 名>-<UTC 时间戳>`）。不要手工修改 `.glusterfs-csi/`；归档目录确认不再需要后可直接删除。`onDelete: retain` 保留的目录不会被同名的新卷接管。

### 3.3 独立卷（强隔离 / 快照）
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: { name: glusterfs-volume }
provisioner: glusterfs.csi.io
parameters:
  clusterID: prod
  provisioningMode: volume
  replica: "3"
  brickBackend: lvm-thin   # directory | lvm-thin（快照需要 lvm-thin）
  volumeGroup: vg_gluster
  volumeOptions: "performance.cache-size=256MB,cluster.brick-multiplex=on"
  csi.storage.k8s.io/provisioner-secret-name: glusterfs-ssh
  csi.storage.k8s.io/provisioner-secret-namespace: glusterfs-csi
  csi.storage.k8s.io/controller-expand-secret-name: glusterfs-ssh
  csi.storage.k8s.io/controller-expand-secret-namespace: glusterfs-csi
allowVolumeExpansion: true
```

## 4. 常见排障
| 现象 | 排查 |
|---|---|
| Pod 报 `Transport endpoint is not connected` | node 插件被重启，FUSE 进程随之退出。恢复器会在一个扫描周期（默认 10s）内重挂宿主机侧的挂载；volumeMount 声明了 `mountPropagation: HostToContainer` 的 Pod 自动恢复，其余 Pod 需重启容器（建议配存活探针，如定期 `stat` 挂载目录）。查看插件日志中的 recovery 输出；M5 起可改用 host mounter 模式 |
| PVC Pending | `kubectl logs deploy/glusterfs-csi-controller -c csi-provisioner`；确认父卷存在且已启动 |
| 挂载超时 | K8s 节点到 49152–60999 端口是否放通；`gluster volume status <vol>` 看 brick 端口 |
| 客户端日志 | 节点上 `/var/log/glusterfs/csi/<vol>.log`（hostPath 挂出） |
