# 路线图与任务拆分

每个任务（T-xx）是一次交给 pi（deepseek-flash）的独立工作单元：目标单一、改动范围明确、可独立验收。
任务细则（接口签名、边界情况）在派发前写入 `docs/tasks/T-xx.md`，派发时作为 pi 的输入。

## 里程碑总览

| 里程碑 | 交付物 | 用户可感知结果 |
|---|---|---|
| **M0 基础** | 脚手架、Identity、volume_id、配置、CI | 驱动可注册到 kubelet |
| **M1 静态卷** | Node 服务、FUSE 挂载与恢复、部署清单、镜像 | 已有 Gluster 卷可被 Pod 以 RWX 挂载 |
| **M2 子目录动态卷** | Controller Create/Delete（subdir）、csi-sanity、Helm | `kubectl apply` PVC 即自动建卷 |
| **M3 配额/扩容/独立卷** | SSH Executor、quota、扩容、volume 模式(directory) | 容量强制、在线扩容、强隔离卷 |
| **M4 快照/克隆** | lvm-thin brick、gluster snapshot、clone | VolumeSnapshot / 从快照恢复 |
| **M5 生产化** | host mounter、指标、e2e 套件、发布流水线、文档 | 可无损升级、可观测、正式 v1.0 |

## 任务清单

### M0 基础
| ID | 任务 | 验收标准 |
|---|---|---|
| T-01 | 仓库脚手架：go.mod、`cmd/glusterfs-csi`、Makefile（build/test/lint/image）、`.golangci.yml`、Dockerfile（多阶段，运行镜像含 glusterfs-fuse）、Apache-2.0 | `make build test lint` 全绿；`docker buildx` 能构建 amd64+arm64 |
| T-02 | `pkg/volumeid`：编解码 + 白名单校验 + 版本前缀 | 表驱动测试覆盖合法/非法/注入字符；覆盖率 ≥ 90% |
| T-03 | `pkg/config`：从挂载文件加载 clusterID → servers/secretRef，热更新 | 测试覆盖缺失 clusterID、空 servers、文件变更 |
| T-04 | gRPC server 骨架 + Identity 服务 + Probe；`--mode` 切换 | 单测验证 GetPluginInfo/Capabilities；`csi-sanity` Identity 部分通过 |

### M1 静态卷
| ID | 任务 | 验收标准 |
|---|---|---|
| T-05 | `pkg/mounter`：Mounter 接口、容器内 glusterfs 实现、挂载参数构造（backup-volfile-servers、mountOptions 白名单） | FakeMounter 单测；参数构造无 shell 拼接 |
| T-06 | Node 服务：Stage/Unstage/Publish/Unpublish/GetInfo/GetCapabilities，幂等 + 错误码 | 单测覆盖重复调用、路径不存在、只读、ROX/RWOP |
| T-07 | NodeGetVolumeStats + VOLUME_CONDITION（statfs、ENOTCONN 检测） | 单测模拟 ENOTCONN 返回 abnormal |
| T-08 | 挂载恢复器：启动时扫描并恢复失效 staging 挂载、刷新 bind | 单测 + 真实集群：杀掉 node 插件 Pod 后业务 Pod I/O 自动恢复 |
| T-09 | `deploy/kustomize`：CSIDriver、RBAC、node DaemonSet、ConfigMap 样例、静态 PV/PVC/Pod 样例 | 真实集群：3 节点 Pod 同时写同一 RWX 卷，数据一致 |

### M2 子目录动态卷
| ID | 任务 | 验收标准 |
|---|---|---|
| T-10 | Controller 服务骨架：GetCapabilities、ValidateVolumeCapabilities、in-flight 锁 | 单测 |
| T-11 | subdir Create/Delete：控制器挂载父卷、mkdir/删除/归档、幂等、参数校验 | 单测（fake mounter + tmpdir）；同名重复创建返回同一卷 |
| T-12 | Controller Deployment + external-provisioner + RBAC；StorageClass 样例 | 真实集群：PVC Bound → Pod 写入 → 删除 PVC 后目录被清理/归档 |
| T-13 | `test/sanity`：csi-sanity 接入（本地 gluster 容器作为后端） | csi-sanity 全部用例通过（不支持的能力自动跳过） |
| T-14 | Helm chart（与 kustomize 同源参数） | `helm lint`、`helm template` 与 kustomize 渲染等价；真实集群安装可用 |

### M3 配额 / 扩容 / 独立卷
| ID | 任务 | 验收标准 |
|---|---|---|
| T-15 | `pkg/gluster` Executor 接口 + SSH 实现 + 主机故障转移 | 单测（内置 SSH server）；参数逐个 quote，注入测试 |
| T-16 | gluster CLI 封装 + XML 解析（volume info/status/create/start/stop/delete、quota） | 用真实 `--xml` 输出做 golden 测试（9/10/11 版本各一份） |
| T-17 | subdir quota + ControllerExpandVolume + GetCapacity | 真实集群：写超额失败；PVC 扩容后可继续写 |
| T-18 | volume 模式（directory brick）Create/Delete/Expand | 真实集群：每 PVC 生成独立 replica 3 卷，删除后 brick 清理干净 |
| T-19 | `hack/gluster-csi-wrapper.sh` 受限 SSH 命令 + 文档 | 非白名单命令被拒绝 |

### M4 快照 / 克隆
| ID | 任务 | 验收标准 |
|---|---|---|
| T-20 | lvm-thin brick 后端（LV 创建/扩容/删除） | 真实集群 |
| T-21 | Create/Delete/ListSnapshots + external-snapshotter 部署 | VolumeSnapshot ReadyToUse |
| T-22 | 从快照/卷克隆（snapshot clone） | 恢复出的 PVC 数据与快照一致 |

### M5 生产化
| ID | 任务 | 验收标准 |
|---|---|---|
| T-23 | host mounter（nsenter 模式） | 升级 node DaemonSet 期间业务 I/O 不中断 |
| T-24 | Prometheus 指标 + ServiceMonitor 样例 | 指标可抓取 |
| T-25 | K8s external storage e2e（testdriver.yaml）+ kind 环境 CI | e2e storage 套件通过 |
| T-26 | 发布流水线：多架构镜像、签名、SBOM、CHANGELOG、版本矩阵 CI | tag 触发自动发布 |
| T-27 | 用户文档：安装、迁移（in-tree/Heketi → 本驱动）、排障 | 文档评审 |

## 验收流程（每个任务都执行）

1. **自动门禁**：`go build ./... && go vet ./... && golangci-lint run && go test -race ./...` 全绿。
2. **范围检查**：`git diff --stat` 只涉及任务声明的文件；新增依赖需在任务报告中说明理由。
3. **逐行代码评审**（架构师执行），重点：
   - CSI 错误码是否符合 spec（见 `AGENTS.md`）；
   - 幂等性：每个 RPC 重复调用的行为；
   - 任何外部输入进入命令行/路径前是否经过校验（命令注入、路径穿越）；
   - 日志是否泄露 Secret；
   - 是否有 panic 路径、goroutine 泄漏、未处理的 ctx 取消。
4. **测试质量**：测试必须断言行为，不接受只为覆盖率存在的空测试。
5. **集成验证**：M1 起每个里程碑在真实 3+3 集群上跑对应场景。
6. 不合格 → 写明具体问题打回 pi 返工；同一任务连续两次不合格则由架构师拆小任务或重写细则。
