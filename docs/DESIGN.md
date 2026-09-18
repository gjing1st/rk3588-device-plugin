# 设计说明 / Design Notes

本文说明 `rk3588-device-plugin` 的实现原理与设计取舍，面向想要二次开发或做源码级排查的读者。

---

## 一、背景：RK3588 的 NPU 到底是个什么设备

在写任何代码之前，必须先把设备的物理形态搞清楚。这里有一个非常普遍的误区：

> 网上大量教程说设备节点是 `/dev/galcore`。

**这是错的**。RK3588 的 NPU 驱动（`rknpu`）挂在 Linux **DRM（Direct Rendering Manager）**框架下，以 **render node** 的形式暴露，主线驱动**根本不创建** `/dev/galcore` 这个节点——那是早期其他驱动或特定发行版遗留下来的名字。

正确的定位方法是**读内核日志**，而不是照抄博客：

```bash
dmesg | grep -i npu
```

```
[    4.254405] RKNPU fdab0000.npu: Adding to iommu group 0
[    4.254539] RKNPU fdab0000.npu: RKNPU: rknpu iommu is enabled, using iommu mode
[    4.256439] [drm] Initialized rknpu 0.9.8 20240828 for fdab0000.npu on minor 1
```

最后一行的 `on minor N` 是关键。DRM render node 的命名规则是：

```
设备路径 = /dev/dri/renderD(128 + minor)
        = /dev/dri/renderD(128 + 1)
        = /dev/dri/renderD129
```

交叉验证：

```bash
ls -la /dev/dri/
# crw-rw----  renderD128   ← GPU
# crw-rw----  renderD129   ← NPU

ls /dev | grep -E 'galcore|rknpu'
# 无输出 —— 这是正常的，不代表驱动没装
```

**结论**：`renderD129` 是最典型的情况，但 `minor` 编号会因板卡、内核版本、设备树配置而变化，**因此代码里绝不能硬编码**。

---

## 二、设备发现策略：sysfs 优先，静态兜底

`detectNPU()` 采用两级策略：

### 第 1 级：sysfs 动态发现（`findRknpuRenderD()`）

```go
entries, _ := os.ReadDir("/sys/class/drm")
for _, e := range entries {
    name := e.Name()
    if !strings.HasPrefix(name, "renderD") {
        continue
    }
    // 读 driver 符号链接指向
    driverLink, err := os.Readlink(filepath.Join("/sys/class/drm", name, "device/driver"))
    if err != nil {
        continue
    }
    if strings.Contains(driverLink, "rknpu") {
        return filepath.Join("/dev/dri", name)   // 命中
    }
}
```

原理：`/sys/class/drm/renderD*/device/driver` 是一个符号链接，指向该设备实际绑定的驱动目录。只要链接路径里出现 `rknpu`，就说明这个 render node 是 NPU 而不是 GPU。

**为什么同时判断 `strings.HasPrefix(driverLink, "/")` 又判断 `Contains`？**
因为不同内核导出的软链接目标形式不同：

- 绝对路径形式：`/sys/bus/platform/drivers/rknpu`
- 相对路径形式：`../../../bus/platform/drivers/rknpu`

两种都包含 `rknpu`，所以 `Contains` 才是真正的判据。

### 第 2 级：静态 fallback

按顺序检查：`/dev/dri/renderD129` → `/dev/dri/renderD128` → `/dev/galcore`。

**兜底路径的意义**：当 `/sys/class/drm` 没有被挂载进容器时（例如用户裁剪了 DaemonSet 的 volumeMounts），第 1 级会直接失败。此时至少保证最常见的 `renderD129` 仍可用，**降级而不是罢工**。

> ⚠️ 使用建议：让 DaemonSet 挂载 `/sys/class/drm`，走第 1 级。日志里出现 `NPU detected via static path` 就说明你正在依赖兜底，虽然能用但不推荐长期这样跑。

### 找不到设备时：`os.Exit(0)`

```go
npuDevice, ok = detectNPU()
if !ok {
    log.Println("[WARN] No RK3588 NPU device found, exiting gracefully (non-3588 node)")
    os.Exit(0)
}
```

**为什么是退出而不是报错重试？** 因为最常见的「找不到设备」场景，就是**这个节点本来就没有 RK3588 NPU**。此时继续运行会向 kubelet 上报一个不存在的资源，把 Pod 调度到没有算力的机器上——这是更严重的错误。

**退出码用 `0` 而不是 `1`**，是为了在日志层面区分「节点没有这个硬件（预期内）」和「有硬件但启动失败（真故障）」。

**副作用**：在非 RK3588 节点上，该 Pod 会「退出 → 被 DaemonSet 重启 → 再退出」循环。这是有意接受的代价，通过 DaemonSet 的节点标签选择器（`hardware-type=rk3588`）从根上避免。

---

## 三、为什么上报 3 个设备单元

```go
const maxDevices = 3
```

### 硬件事实

RK3588 的 NPU 提供 **6 TOPS** 总算力，内部是 **3 个 NPU 计算核心**（每核约 2 TOPS / 3 TOPS 量级，取决于频率与量化精度）。

### 关键约束：3 个 core 共享同一个 render node

从用户态看，**3 个 core 只暴露为一个 DRM 设备节点**。K8s 的 Device Plugin 机制要求「一个 Device ID 对应一个可独立分配的硬件单元」，而这里并不存在 3 个可独立打开的设备文件。

### 设计取舍

| 方案 | 问题 |
|---|---|
| 上报 1 个设备 | 节点同时只能跑 1 个 NPU 容器，3 个 core 严重浪费 |
| 上报 3 个设备，但 ID 指向同一物理设备 | ✅ 采用。K8s 侧获得 3 个并发额度，实际 core 分配交给内核 |
| 上报 3 个设备，每个绑定独立 core | ❌ 需要 `rknn_set_core_mask()` 之类的 SDK 级操作，Device Plugin 层做不到 |

因此：**`maxDevices = 3` 本质是一个软件层面的并发限制，而非硬件隔离。**

三个 Device ID（`rk3588-npu-0/1/2`）只是三个「名额」，`Allocate()` 返回的挂载内容完全一致。内核驱动会在多个进程同时提交推理任务时做实际的 core 级调度与排队。

> ⚠️ 重要认知：一个容器拿到 `rk3588.ai/npu: 1`，**不等于独占 1 个 NPU core**，也不代表算力被隔离。这与 NVIDIA GPU 上 HAMi 做显存/算力切分是完全不同性质的事情，不要混淆。

### 数值选择的边界

- **不要调大于 3**：超过物理 core 数只会增加无意义的上下文切换与排队，不会提升吞吐。
- **可以调小**：如果业务本身对延迟敏感、希望独占算力，把 `maxDevices` 设为 `1` 反而更合理——牺牲并发换取可预期的性能。
- 如果只是单容器推理，`1` 是更诚实的选择：K8s 显示的资源数会与你的真实意图一致。

---

## 四、健康检查为什么重要

很多同类实现在 `ListAndWatch` 里发一次设备列表后就 `select{}` 永久阻塞，**从此不再上报**。这在生产环境是危险的：

- 驱动崩溃 / 模块被 `rmmod` / 设备节点被删除时，kubelet 依然认为节点有可用 NPU；
- 新 Pod 被调度上来，然后在 `Allocate()` 或运行时才失败——**故障暴露在业务层，而不是调度层**。

本项目的做法是保留原生的 ListAndWatch 语义：

```go
ticker := time.NewTicker(30 * time.Second)
for {
    select {
    case <-m.stop:
        return nil
    case <-ticker.C:
        health := pluginapi.Healthy
        if _, err := os.Stat(npuDevice); os.IsNotExist(err) {
            health = pluginapi.Unhealthy
            log.Printf("[WARN] %s disappeared, marking unhealthy", npuDevice)
        }
        for i := range devs {
            devs[i].Health = health
        }
        s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
    }
}
```

**30 秒**是一个折中：足够快地反映节点故障，又不会因为频繁 `Stat` 产生额外开销（`Stat` 本身极轻量，主要成本在 gRPC 消息往返）。

设备被标记为 `Unhealthy` 后，kubelet 会停止向该节点分配这个资源，**已有的 Pod 不会被驱逐**，但新 Pod 不会再被调度上来。这是符合 K8s 设备插件规范的预期行为。

> 一个小细节：设备恢复健康后，同一段逻辑会自动把状态改回 `Healthy`、重新上报，无需重启插件。

---

## 五、Allocate 阶段挂了什么、为什么

```go
mounts := []*pluginapi.Mount{
    {
        ContainerPath: "/dev/dri",
        HostPath:      filepath.Dir(npuDevice),   // 即 /dev/dri
        ReadOnly:      false,
    },
}
```

### 为什么挂整个 `/dev/dri` 而不是只挂 `renderD129`

因为 `librknnrt.so` 在初始化和运行时**可能同时访问 GPU 的 card 节点与 NPU 的 render 节点**（例如做内存分配、DMA buffer 管理、或某些平台检测逻辑）。只挂单个 render node 会导致一部分场景在 `rknn_init()` 阶段直接失败，且报错信息通常很隐晦。

代价是理论上容器也能访问 GPU 设备节点——但在 NPU 推理这个场景下，这个「过度授权」是可以接受的工程取舍。

### 挂 `librknnrt.so`

```go
if npuLibPath != "" {
    mounts = append(mounts, &pluginapi.Mount{
        ContainerPath: npuLibPath,
        HostPath:      npuLibPath,
        ReadOnly:      true,
    })
}
```

探测顺序：`/usr/lib/librknnrt.so` → `/usr/lib/aarch64-linux-gnu/librknnrt.so` → `/usr/local/lib/librknnrt.so`。

**这是一个便利性设计，不是必需项**。标准做法仍然是业务镜像自带该库（见 README 6.3）。插件顺带挂载的好处是：如果宿主机的 `librknnrt.so` 版本与驱动（`rknpu 0.9.8`）匹配得当，容器内可以直接省掉一份拷贝。

**注意版本耦合风险**：`librknnrt.so`（用户态运行时）与 `rknpu`（内核驱动）存在版本匹配要求。宿主机上的库版本是确定的，但业务镜像里自带的版本不一定——**两者混用容易出现玄学问题**。建议二选一，不要既自带又依赖注入。

### 环境变量的作用

| 变量 | 用途 |
|---|---|
| `RKNN_NPU_DEVICE` | 渲染节点在容器内的路径。因为挂载的是整个目录，容器内路径是稳定的 `/dev/dri/renderD129`；给出这个变量让业务代码不必自己猜 |
| `RKNN_NPU_ENABLE` | 业务侧做「有没有 NPU」的开关判断 |
| `RKNN_LOG_LEVEL` | RKNN 运行时日志级别，默认 `0`（安静）。会被 Pod spec 中的同名 env 覆盖 |
| `RK3588_NPU_DEVICES` | 本次分配到的 Device ID 列表，便于在日志里做分配追踪 |

另外注入了注解 `rk3588.ai/npu-allocated: "true"`，便于排错时确认这个容器确实经过了本插件。

---

## 六、gRPC 接口的实现要点

| 方法 | 实现 | 备注 |
|---|---|---|
| `GetDevicePluginOptions` | 返回 `PreStartRequired: true` | 声明需要 `PreStartContainer` 回调 |
| `GetPreferredAllocation` | 返回空响应 | **kubelet API v0.27+ 强制要求实现**，缺失会编译不过（不是运行时报错） |
| `ListAndWatch` | 首帧上报 3 个设备，之后 30s 一次 | 必须保持流不断开 |
| `Allocate` | 按 `ContainerRequests` 逐个构造响应 | 一个 Pod 多容器时会被调用多次 |
| `PreStartContainer` | 打日志后返回空 | 本插件没有启动前需要做的准备工作 |

### 关于 `GetPreferredAllocation` 的坑

这是最容易卡住新手的地方。报错长这样：

```
*RK3588NPUPlugin does not implement v1beta1.DevicePluginServer
(missing method GetPreferredAllocation)
```

原因是 `k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1` 在较新版本（kubelet v0.27+）中把该方法加入了接口定义。**即使你的逻辑完全用不到它，也必须实现**：

```go
func (m *RK3588NPUPlugin) GetPreferredAllocation(
    ctx context.Context,
    req *pluginapi.PreferredAllocationRequest,
) (*pluginapi.PreferredAllocationResponse, error) {
    return &pluginapi.PreferredAllocationResponse{}, nil
}
```

---

## 七、启动流程与自检

`Start()` 的顺序是有讲究的：

1. **删除残留 socket** —— 上次崩溃可能留下 `/var/lib/kubelet/device-plugins/rk3588-npu.sock`，不删会 `address already in use`
2. **监听 Unix Socket**
3. **起 gRPC server（goroutine）**
4. **自检连通性** —— 用 `grpc.DialContext` + `WithBlock` 拨自己，确认 server 真的起来了
5. **向 kubelet 注册**（`RegistrationClient.Register`）
6. **阻塞等待**

第 4 步的「自检」很多实现会省掉。它的价值在于：把「server 没起来」和「kubelet 连不上」两类故障在启动阶段就区分开，日志里能立刻看出是哪一步挂了，而不是等到 kubelet 报错再去猜。

Socket 路径来自 `pluginapi.DevicePluginPath`（即 `/var/lib/kubelet/device-plugins/`），kubelet socket 路径来自 `pluginapi.KubeletSocket`——两者都由 kubelet 官方 SDK 提供常量，不要自己拼字符串。

---

## 八、构建侧的两个关键决策

### 8.1 为什么 vendor

信创环境的典型特征是**内网 / 离线**。`go mod download` 依赖 `proxy.golang.org`，在国内基本不可达。把依赖 `go mod vendor` 进仓库后：

```dockerfile
COPY vendor/ vendor/
RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o rk3588-device-plugin .
```

构建全程零网络依赖。代价是仓库体积变大（约 700 个文件），这是**有意付出的成本**。

### 8.2 为什么 `CGO_ENABLED=0`

静态编译，产出的二进制不依赖容器内的 glibc 版本，可以直接跑在 `alpine` 上（`alpine` 用 musl 而非 glibc）。最终镜像约 15 MB。

### 8.3 交叉构建的陷阱

```
❌ docker buildx build --platform linux/arm64 ...   ← 如果 FROM 是 amd64 的 golang 镜像
```

`--platform` 只声明**目标**架构。如果基础镜像是 amd64 的，Go 会编译出 x86 二进制，镜像架构却是 arm64 —— 拉到 3588 节点上直接 `exec format error`。

**正确做法**（本项目采用）：确保 `FROM` 的基础镜像本身有 arm64 版本（`golang:1.24.2-alpine3.21`、`alpine:3.21.3` 都是多架构的），配合 `CGO_ENABLED=0`，让 buildx 在目标架构下构建。

**替代做法**：`--platform=$BUILDPLATFORM` + 显式 `GOOS=linux GOARCH=arm64` 交叉编译，速度更快，但需要额外的 `ARG TARGETARCH` 参数传递。

验证产物架构：

```bash
docker image inspect your-registry/rk3588-device-plugin:v1.0.0 | grep -i architecture
# 应为 "Architecture": "arm64"
```

---

## 九、已知不足与改进方向

| 不足 | 影响 | 改进方向 |
|---|---|---|
| 未实现 CDI | 必须挂载设备节点，无法走 CDI 的现代化注入路径 | 生成 `/var/run/cdi/*.yaml`，由 containerd 自动注入 |
| 不自动打节点标签 | 需人工 `kubectl label` | 接入 Node Feature Discovery (NFD) |
| 资源名 / 单元数为编译期常量 | 改配置要重新编译 | 改为 `flag` 命令行参数 |
| 无单元测试 | 回归依赖人工 | 补充 `detectNPU()` 等纯函数的表驱动测试 |
| 只覆盖 RK3588 | RK3576 / RK3568 未验证 | 抽取 SoC 适配层 |

---

## 参考

- [Kubernetes Device Plugin 官方文档](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)
- [`k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1`](https://pkg.go.dev/k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1)
- Rockchip RKNN 官方仓库：[`airockchip/rknn-toolkit2`](https://github.com/airockchip/rknn-toolkit2)、[`airockchip/rknn-llm`](https://github.com/airockchip/rknn-llm)
