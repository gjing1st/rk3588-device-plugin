# 排错手册 / Troubleshooting

本文汇总 `rk3588-device-plugin` 在真实环境中遇到的问题、根因与解法。所有条目均按「现象 → 根因 → 解法」组织，便于直接对照排查。

---

## 快速定位：先跑这一组命令

```bash
# 第 1 步：节点上到底有没有 NPU 驱动
dmesg | grep -i rknpu
ls -la /dev/dri/

# 第 2 步：插件 Pod 在不在、日志说了什么
kubectl -n kube-system get pod -l name=rk3588-npu-device-plugin -o wide
kubectl -n kube-system logs daemonset/rk3588-npu-device-plugin --tail=50

# 第 3 步：K8s 有没有认到资源
kubectl get node <节点名> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep rk3588

# 第 4 步：Socket 有没有注册成功
ls -la /var/lib/kubelet/device-plugins/ | grep rk3588
```

四步下来基本能确定故障层次：**驱动层 → 插件层 → kubelet 层 → 调度层**。

---

## 一、设备识别类问题

### 1.1 找不到 `/dev/galcore`，以为驱动没装

**现象**

```bash
ls /dev | grep galcore
# 无输出
```

**根因**

`/dev/galcore` 是**旧版内核或特定发行版**的历史设备名。RK3588 主线 `rknpu` 驱动挂在 DRM 框架下，以 render node 形式暴露，**不会创建这个节点**。

**解法**

不要按设备名找，按内核日志找：

```bash
dmesg | grep -i "Initialized rknpu"
# [    4.256439] [drm] Initialized rknpu 0.9.8 20240828 for fdab0000.npu on minor 1
```

拿到 `minor 1`，设备就是 `/dev/dri/renderD(128+1)` = `/dev/dri/renderD129`。

**验证**：

```bash
ls -la /dev/dri/
# renderD128 → GPU
# renderD129 → NPU
```

---

### 1.2 `renderD129` 不存在，但驱动看起来正常

**现象**

`dmesg` 显示 `Initialized rknpu ... on minor N`，但 `/dev/dri/renderD129` 不存在。

**根因**

`minor` 编号不一定是 `1`。它取决于设备树、系统中其他 DRM 设备的注册顺序。例如 NPU 可能是 `minor 0`（对应 `renderD128`），或 GPU 与 NPU 注册顺序颠倒。

**解法**

先从 `dmesg` 读实际的 `minor N`，再算路径：

```bash
dmesg | grep -oP 'Initialized rknpu.*on minor \K[0-9]+'
# 输出 N，则路径为 /dev/dri/renderD$((128+N))
```

**这也正是本插件用 sysfs 动态发现、而不硬编码 `renderD129` 的原因** —— 插件会自动处理这种情况，你不需要改任何配置。

---

### 1.3 插件日志显示走了静态路径（`detected via static path`）

**现象**

```
[INFO] NPU detected via static path: /dev/dri/renderD129
```

**根因**

sysfs 动态发现失败，退化到静态兜底。常见原因：`/sys/class/drm` 没有挂载进容器，或容器内看不到 `device/driver` 软链接。

**解法**

检查 DaemonSet 的 volumeMounts 是否包含：

```yaml
volumeMounts:
  - name: sys-class-drm
    mountPath: /sys/class/drm
volumes:
  - name: sys-class-drm
    hostPath:
      path: /sys/class/drm
```

**影响评估**：功能上仍然可用（因为兜底路径恰好正确），但换一块 `minor` 不同的板子就会失效。建议修好挂载。

---

### 1.4 插件日志报 `NPU detected via sysfs`，但 `npuLibPath` 是空的

**现象**

```
[WARN] librknnrt.so not found, container will need it from its own image
```

**根因**

宿主机的 `/usr/lib/librknnrt.so` 不存在。**这是正常现象**，插件自身不需要这个库。

**解法**

不需要处理。但业务 Pod 必须**自带** `librknnrt.so`（见 README 6.3）。如果希望在节点层面统一部署，把 RKNN Runtime 装到宿主机 `/usr/lib/` 即可，插件下次启动会自动识别并注入。

---

## 二、K8s 资源与调度类问题

### 2.1 节点上看不到 `rk3588.ai/npu`

**排查顺序**

1. **节点标签打了吗**

   ```bash
   kubectl get node <节点名> --show-labels | grep hardware-type
   ```

   没有则补：

   ```bash
   kubectl label node <节点名> hardware-type=rk3588
   ```

2. **Pod 起来了吗**

   ```bash
   kubectl -n kube-system get pod -l name=rk3588-npu-device-plugin -o wide
   ```

   如果 Pod 在目标节点上根本不存在，说明节点选择器没匹配上。

3. **Pod 在 `CrashLoopBackOff` 或反复重启**

   看日志是不是 `No RK3588 NPU device found`。如果是，说明容器内看不到设备——继续查第 4 步。

4. **容器内设备节点挂进去了吗**

   ```bash
   kubectl -n kube-system exec -it <plugin-pod> -- ls -la /dev/dri/
   kubectl -n kube-system exec -it <plugin-pod> -- ls -la /sys/class/drm/
   ```

   缺哪个就补哪个 hostPath 挂载。

5. **驱动真的加载了吗**

   ```bash
   lsmod | grep rknpu
   dmesg | grep -i rknpu
   ```

   某些国产定制内核（如麒麟国防版）需要额外安装内核模块。若内核本身不带 `rknpu`，插件无从谈起。

---

### 2.2 Pod 一直 `Pending`，事件提示 `Insufficient rk3588.ai/npu`

**根因**

两种可能：① 现有 3 个单元都被占用了；② 集群里没有任何节点上报了该资源。

**解法**

```bash
# 看节点的资源分配情况
kubectl describe node <节点名> | grep -A10 "Allocated resources"

# 看所有节点的 rk3588 资源
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,\
NPU:.status.allocatable.rk3588\.ai/npu
```

如果是被占满：`kubectl get pod -A -o json | jq` 找出占用者，或考虑把 `maxDevices` 从 3 调大（但注意不要超过物理 core 数）。

如果是没有节点上报：回到 2.1。

---

### 2.3 非 RK3588 节点上插件 Pod 反复重启

**现象**

```bash
kubectl get pod -A | grep rk3588
# kube-system   rk3588-npu-device-plugin-xxxxx   0/1   CrashLoopBackOff   12   3m
```

**根因**

插件检测不到 NPU 时会 `os.Exit(0)` 主动退出（避免上报假资源），DaemonSet 随即重启它，形成循环。**这是有意的设计**，不是 bug。

**解法**

给 DaemonSet 限定节点范围，用标签选择器：

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: hardware-type
              operator: In
              values:
                - rk3588
```

（本仓库的 `deploy/daemonset.yaml` 默认已是这个配置。）

---

## 三、容器内推理类问题

### 3.1 容器内报「无法识别平台 / SoC 检测失败 / 不支持当前设备」

**现象**

```
E RKNN: rknn_init, Invalid RKNN model / platform not supported
```
或
```
failed to open /proc/device-tree/compatible
```

**根因**

`rknn-toolkit2` 的部分接口会读取 `/proc/device-tree/compatible` 来判断 SoC 型号。**Kubernetes 默认会把容器内的 `/proc` 做遮蔽（masked）**，这个路径读不到。

这是社区里公认的坑，`akolk/ai-cluster` 项目为此要求业务 Pod 必须开 `privileged: true`。

**解法（按推荐度排序）**

**方案 A：给业务 Pod 开特权**（兼容性最好，代价是权限过大）

```yaml
spec:
  containers:
    - name: infer
      securityContext:
        privileged: true
```

**方案 B：只解遮蔽 `/proc`**（更精细，但依赖内核版本）

```yaml
spec:
  hostUsers: false
  containers:
    - name: infer
      securityContext:
        procMount: Unmasked
```

> ⚠️ `procMount: Unmasked` 依赖内核 **≥ 6.3** 的 `MOUNT_ATTR_IDMAP` 能力。麒麟 V10 国防版的定制内核版本较老，**大概率不支持**，此方案可能无效。Talos + 主线内核 6.18+ 是明确验证过可行的场景。

**方案 C：避开会读 `/proc/device-tree/compatible` 的接口**（最理想，但改造量大）

只使用 `librknnrt.so` 的标准 C API（`rknn_init` / `rknn_run` 等），并显式指定 `target_platform`，不做自动平台探测。

> 📌 说明：本节内容基于社区项目（`akolk/ai-cluster`、`schwankner/talos-rk3588-npu`）的经验总结，**本项目未在麒麟 V10 上逐一验证**。如果你的业务报平台识别错误，优先按这个方向排查。

---

### 3.2 容器内 `rknn_init` 报找不到 `librknnrt.so`

**现象**

```
error while loading shared libraries: librknnrt.so: cannot open shared object file
```

**根因**

业务镜像里没有这个库，而宿主机的库也没被注入（或注入路径不在动态链接器的搜索路径里）。

**解法**

**推荐做法** —— 业务镜像自带：

```dockerfile
# 从 3588 节点取：scp root@<节点IP>:/usr/lib/librknnrt.so .
COPY librknnrt.so /usr/lib/
RUN ldconfig
```

**或**从官方 SDK 取：`airockchip/rknn-toolkit2` 仓库中的
`rknpu2/runtime/Linux/librknn_api/aarch64/librknnrt.so`。

---

### 3.3 能跑但结果错 / 频繁崩溃 / 玄学问题

**根因（高概率）**

**`librknnrt.so`（用户态运行时）与 `rknpu`（内核驱动）版本不匹配。**

RKNN 生态对版本匹配相当敏感。例如：
- `librknnrt.so` 2.x 的某些版本要求 `rknpu` 驱动 ≥ 0.9.x
- 业务镜像里自带的库版本可能与宿主机驱动版本不一致

**解法**

比对两侧版本，保持匹配：

```bash
# 内核驱动版本
cat /sys/module/rknpu/version
# 或
dmesg | grep -i "Initialized rknpu"

# 用户态运行时版本（容器内运行一次推理即可看到）
# I RKNN: RKNN Runtime Information, librknnrt version: 2.3.2 ...
# I RKNN: RKNN Driver Information, version: 0.9.8
```

**关键建议**：**不要既依赖插件注入宿主机库、又自带镜像内库** —— 两者混用极易因为加载到哪个版本不确定而产生玄学问题。二选一，推荐业务镜像自带。

---

### 3.4 容器内看不到 `/dev/dri/renderD129`

**排查**

1. Pod 是否声明了资源？

   ```yaml
   resources:
     limits:
       rk3588.ai/npu: "1"
   ```

   **只写 `requests` 不写 `limits` 是无效的** —— 扩展资源在 K8s 中 `requests` 与 `limits` 必须相等，通常只写 `limits` 即可（K8s 会自动补 `requests`）。

2. 确认容器确实经过了 Allocate：

   ```bash
   kubectl get pod <pod> -o jsonpath='{.metadata.annotations}'
   # 应含 rk3588.ai/npu-allocated: "true"
   ```

   如果注解不存在，说明 kubelet 没调用本插件的 `Allocate()`，问题在调度层。

3. 确认该 Pod 落在 RK3588 节点上：

   ```bash
   kubectl get pod <pod> -o wide
   ```

---

## 四、构建与镜像类问题

### 4.1 节点上 `exec format error`

**现象**

```
exec /usr/bin/rk3588-device-plugin: exec format error
```

**根因**

镜像架构不是 arm64。**这是最常见的交叉构建陷阱。**

`docker buildx build --platform linux/arm64` 只声明**目标**架构。如果 `FROM golang:...` 拉到的是 **amd64** 基础镜像，Go 会编译出 x86 二进制，而镜像 manifest 却标成 arm64。

**解法**

验证镜像架构：

```bash
docker image inspect <image>:<tag> | grep -i architecture
# 期望: "Architecture": "arm64"
```

若不对，检查基础镜像是否有多架构支持：

```bash
docker manifest inspect golang:1.24.2-alpine3.21 | grep -A2 '"architecture"'
```

本仓库的 `Dockerfile` 使用 `golang:1.24.2-alpine3.21` + `alpine:3.21.3`，两者均为多架构镜像，配合 `CGO_ENABLED=0` 可正常产出 arm64 二进制。

**替代方案**（交叉编译，构建更快）：

```bash
docker buildx build \
  --platform linux/arm64 \
  --build-arg TARGETARCH=arm64 \
  -t your-registry/rk3588-device-plugin:v1.0.0 --load .
```

配合 Dockerfile 中 `ARG TARGETARCH` + `GOARCH=$TARGETARCH`。

---

### 4.2 构建时拉不到依赖（内网 / 离线）

**根因**

`go mod download` 依赖 `proxy.golang.org`，国内不可达。

**解法**

本仓库已 `go mod vendor`，`go build -mod=vendor` **全程不需要网络**，直接构建即可。

若需要新增依赖、必须重新 vendor，在能联网的机器上执行：

```bash
docker run --rm -v "$(pwd)":/build -w /build \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.24.3 sh -c "go mod tidy && go mod vendor"
```

> 说明：vendor 操作只是下载源代码，与架构无关，**用 amd64 的 golang 容器即可**，不需要特意找 arm64 镜像。

---

### 4.3 编译报 `missing method GetPreferredAllocation`

**现象**

```
cannot use m (variable of type *RK3588NPUPlugin) as
v1beta1.DevicePluginServer value in argument to RegisterDevicePluginServer:
*RK3588NPUPlugin does not implement v1beta1.DevicePluginServer
(missing method GetPreferredAllocation)
```

**根因**

kubelet API v0.27+ 把 `GetPreferredAllocation` 加入了接口定义，**即使逻辑上用不到也必须实现**。

**解法**

本项目已提供空实现。若你裁剪了代码，记得补回：

```go
func (m *RK3588NPUPlugin) GetPreferredAllocation(
    ctx context.Context,
    req *pluginapi.PreferredAllocationRequest,
) (*pluginapi.PreferredAllocationResponse, error) {
    return &pluginapi.PreferredAllocationResponse{}, nil
}
```

---

## 五、插件与 kubelet 通信类问题

### 5.1 日志报 `address already in use` 或 `bind: address already in use`

**根因**

上一次插件异常退出，残留了 socket 文件 `/var/lib/kubelet/device-plugins/rk3588-npu.sock`。

**解法**

插件启动时的 `os.Remove(serverSock)` 已经处理了这种情况。如果仍然报错，说明该路径**没有写权限**或挂载有问题：

```bash
kubectl -n kube-system exec -it <plugin-pod> -- ls -la /var/lib/kubelet/device-plugins/
```

确认 `securityContext.privileged: true` 且 hostPath 挂载正确。

手动清理（在节点上）：

```bash
rm -f /var/lib/kubelet/device-plugins/rk3588-npu.sock
```

---

### 5.2 日志报 `connect kubelet: context deadline exceeded`

**根因**

容器内找不到 kubelet 的 socket，或路径不对。

**排查**

```bash
# 插件容器内
kubectl -n kube-system exec -it <plugin-pod> -- ls -la /var/lib/kubelet/device-plugins/
# 应能看到 kubelet.sock
```

**常见原因**

1. `hostNetwork` 没开——检查 DaemonSet 的 `spec.template.spec.hostNetwork: true`
2. Kubelet 的 `--kubelet-registration-path` 被自定义过，与默认路径不一致
3. 容器运行时把 `/var/lib/kubelet/device-plugins` 挂到了别处（如某些 k3s / 自定义 containerd 配置）

---

### 5.3 插件日志一切正常，但节点资源仍然是 0

**排查顺序**

1. **kubelet 是否重启过？** 插件注册是一次性的。kubelet 重启后需要重新注册。重启插件 Pod：

   ```bash
   kubectl -n kube-system rollout restart daemonset/rk3588-npu-device-plugin
   ```

2. **资源名是否一致？** 插件上报的 `resourceName` 与业务 YAML 里写的必须**完全一致**（含大小写）。用下面命令核对实际注册名：

   ```bash
   kubectl get node <节点名> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep -i npu
   ```

3. **kubelet 日志有没有报错**：

   ```bash
   journalctl -u kubelet -f | grep -i device
   ```

---

## 六、排错速查表

| 现象 | 最可能根因 | 一句话解法 |
|---|---|---|
| 找不到 `/dev/galcore` | 历史设备名，主线驱动不创建 | 用 `dmesg \| grep -i npu` 找 `minor N` |
| `renderD129` 不存在 | `minor` 编号不是 1 | 插件会自动适配，无需改配置 |
| 日志走 `static path` | `/sys/class/drm` 没挂载 | 补 hostPath 挂载 |
| 节点资源数 0 | 节点标签没打 / 设备没挂进去 | 打 `hardware-type=rk3588` 标签 |
| Pod 一直 Pending | 单元占满 / 无节点上报 | `kubectl describe node` 看已分配 |
| 非 3588 节点 Pod 循环重启 | 插件探测不到设备主动退出 | 用节点标签限制范围（预期行为） |
| SoC 检测失败 | `/proc` 被遮蔽 | 业务 Pod 开 `privileged: true` |
| 找不到 `librknnrt.so` | 业务镜像没带 | `COPY librknnrt.so /usr/lib/ && ldconfig` |
| 结果错 / 玄学崩溃 | 用户态库与内核驱动版本不匹配 | 比对版本，避免「自带 + 注入」混用 |
| `exec format error` | 镜像不是 arm64 | 检查基础镜像架构与 `--platform` |
| `missing GetPreferredAllocation` | kubelet API v0.27+ 接口变更 | 补空实现 |
| `address already in use` | socket 残留 | 插件已自处理；检查写权限 |
| `connect kubelet` 超时 | 没开 hostNetwork / 路径被改 | 检查 `hostNetwork: true` |
| 资源数正常但调度不生效 | kubelet 重启后插件未重新注册 | `rollout restart` DaemonSet |

---

## 七、提交 Issue 前请附上

为了能快速定位，请一并提供：

```bash
# 环境信息
uname -a
cat /etc/os-release
dmesg | grep -i rknpu
ls -la /dev/dri/
kubectl version --short
kubectl get nodes -o wide

# 插件状态
kubectl -n kube-system get pod -l name=rk3588-npu-device-plugin -o wide
kubectl -n kube-system logs daemonset/rk3588-npu-device-plugin --tail=100

# 资源与调度
kubectl get node <节点名> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep rk3588

# 业务 Pod（如适用）
kubectl describe pod <pod>
kubectl logs <pod>
```
