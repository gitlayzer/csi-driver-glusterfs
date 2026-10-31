# 使用指南（目标形态）

> 本文描述 v1.0 时用户的使用方式，开发过程中按里程碑逐步变为现实。

## 1. 前置条件

**GlusterFS 侧**
- 一个健康的可信存储池（`gluster peer status` 全部 Connected）。
- 端口：24007/tcp（glusterd）、24008/tcp、49152–49251/tcp（brick，按 brick 数量放开）对所有 K8s 节点开放。
- subdir 模式：预建一个父卷（建议 `replica 3`），例如 `csi-parent`；如需配额：`gluster volume quota csi-parent enable`。
- volume / 配额 / 快照模式：一个可 SSH 登录、可执行 `gluster` 的账号（建议用受限 wrapper，见 `hack/gluster-csi-wrapper.sh`）。

**Kubernetes 侧**
- 所有节点内核支持 FUSE（`/dev/fuse` 存在）。容器内挂载模式下，宿主机无需安装 glusterfs 客户端。
- K8s 节点到 Gluster 节点的上述端口网络可达。

## 2. 安装
```bash
helm install glusterfs-csi deploy/helm/glusterfs-csi -n glusterfs-csi --create-namespace \
  --set clusters[0].id=prod \
  --set clusters[0].servers='{10.0.0.11,10.0.0.12,10.0.0.13}'
```
集群配置最终写入 ConfigMap `glusterfs-csi-config`：
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
  quota: "true"            # 需要 executor；不开启则容量仅为声明值
  onDelete: delete         # delete | retain | archive
reclaimPolicy: Delete
allowVolumeExpansion: true
mountOptions: [log-level=WARNING]
```

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
| Pod 报 `Transport endpoint is not connected` | node 插件被重启；查看插件日志中恢复器输出，或改用 host mounter 模式 |
| PVC Pending | `kubectl logs deploy/glusterfs-csi-controller -c csi-provisioner`；确认父卷存在且已启动 |
| 挂载超时 | K8s 节点到 49152+ 端口是否放通；`gluster volume status <vol>` 看 brick 端口 |
| 客户端日志 | 节点上 `/var/log/glusterfs/csi/<vol>.log`（hostPath 挂出） |
