# v1.0.0 — RK3588 NPU Device Plugin for Kubernetes

**发布日期 / Release date:** 2026-09-18

> 首个正式版本。把瑞芯微 RK3588 / RK3588S 的 NPU 注册为 Kubernetes 可调度资源 `rk3588.ai/npu`。
>
> First public release. Exposes the Rockchip RK3588 / RK3588S NPU as the schedulable Kubernetes resource `rk3588.ai/npu`.

---

## 为什么需要它 / Why this exists

瑞芯微官方仓库（`airockchip/rknn-toolkit2`、`rknn-llm`、`rknn_model_zoo`）只提供推理 SDK，**至今没有任何 Kubernetes Device Plugin**。社区中已有的几个相关实现，要么把设备节点路径硬编码成 `/dev/dri/renderD129`（换板卡/换内核就失效），要么只上报 1 个设备、`ListAndWatch` 发一次就不再上报，要么完全跳过健康检查。

本项目是为了填补这个空白而写的**最小、可读、可维护**的实现。

Rockchip's official repositories ship inference SDKs only — **no Kubernetes device plugin exists officially**. Existing community implementations either hard-code `/dev/dri/renderD129`, advertise a single device and never re-report it, or skip health checking entirely. This project fills that gap with a minimal, auditable implementation.

---

## 本版亮点 / Highlights

| 能力 | 说明 |
|---|---|
| **sysfs 动态设备发现** | 遍历 `/sys/class/drm/renderD*/device/driver` 软链接匹配 `rknpu`，**不硬编码 `renderD129`**；不同板卡、不同内核的 render node 编号不同也能正确识别，找不到才 fallback 到静态路径 |
| **真实健康检查** | `ListAndWatch` 每 30s 用 `os.Stat` 复查设备节点，设备消失时把设备标记为 `Unhealthy`，避免把 Pod 调度到已掉驱动的节点 |
| **上报 3 个可分配单元** | 对应 RK3588 的 3 个 NPU core，作为 K8s 侧的一层并发上限 |
| **完整 v1beta1 接口** | 5 个 gRPC 方法全部实现，含 kubelet v0.27+ 强制要求的 `GetPreferredAllocation`（缺失会直接编译报错） |
| **离线 vendor 构建** | 依赖已 `go mod vendor` 入库，`CGO_ENABLED=0` 静态编译，**内网 / 信创离线环境可直接构建**，不依赖 Go proxy 与公网 |
| **极小镜像** | 多阶段构建，最终镜像基于 `alpine:3.21.3`（arm64），约 15 MB |
| **单文件、零框架依赖** | 全部逻辑在 `main.go`，不引入 `device-plugin-manager` 等封装框架，便于二次开发与源码级排查 |
| **信创环境验证** | 在麒麟 V10 国防版 + openEuler 22.03 的异构 K8s 集群中完成落地验证 |

---

## 验证环境 / Verified on

| 组件 | 版本 / 说明 |
|---|---|
| SoC | Rockchip RK3588（3 × NPU core，6 TOPS） |
| 操作系统 | 麒麟 V10 国防版（瑞芯微定制） / openEuler 22.03 |
| Kubernetes | v1.23.17 |
| KubeSphere | 4.1.3 |
| 部署方式 | kt 离线部署，异构集群（RK3588 + 昇腾 310B 并存） |
| 设备节点 | `/dev/dri/renderD129`（由 sysfs 动态探测得出） |

---

## 仓库内容 / What's in the box

| 路径 | 说明 |
|---|---|
| `main.go` | 插件全部实现：设备发现、健康检查、gRPC 接口 |
| `Dockerfile` | 多阶段构建，产出 alpine arm64 镜像（约 15 MB） |
| `deploy/daemonset.yaml` | DaemonSet 清单，按节点标签 `hardware-type=rk3588` 选择目标节点 |
| `deploy/test-pod.yaml` | 验证 Pod |
| `deploy/app-example.yaml` | 业务 Pod 声明示例 |
| `deploy/build-on-node.sh` | 在 arm64 节点上本地构建镜像的脚本（适合无外网环境） |
| `docs/DESIGN.md` | 设计原理与取舍 |
| `docs/TROUBLESHOOTING.md` | 排错手册（现象 → 根因 → 解法） |

> **本仓库不提供预编译镜像。** 请按 [README 的「快速开始」](https://github.com/gjing1st/rk3588-device-plugin/blob/main/README.md#5-快速开始--quick-start) 自行构建，或使用 `deploy/build-on-node.sh` 在节点上构建。

---

## 快速开始 / Quick start

```bash
# 1. 构建（在 x86 机器上交叉构建 arm64 镜像）
docker buildx build --platform linux/arm64 \
  -t your-registry/rk3588-device-plugin:v1.0.0 --load .

# 2. 推送到你的镜像仓库
docker push your-registry/rk3588-device-plugin:v1.0.0

# 3. 给 RK3588 节点打标签（必须先打，否则会误部署到非 RK3588 节点）
kubectl label node <node-name> hardware-type=rk3588

# 4. 修改 deploy/daemonset.yaml 里的镜像地址，然后部署
kubectl apply -f deploy/daemonset.yaml

# 5. 验证节点已经上报资源
kubectl describe node <node-name> | grep -A3 rk3588
```

完整步骤见 [README](https://github.com/gjing1st/rk3588-device-plugin/blob/main/README.md)。

---

## 已知限制 / Known limitations

1. **3 个单元是软件并发上限，不是硬件隔离。** 3 个 core 共享同一个 render node，core 级调度由内核完成。与 HAMi 的 GPU 显存/算力切分**性质不同**，不能等同。
2. **必须用节点标签限制 DaemonSet 范围。** 插件在检测不到 NPU 时会 `os.Exit(0)` 正常退出（避免在非 RK3588 节点上报假资源），因此若不加节点限制，非 RK3588 节点上的 Pod 会被反复重启，产生无意义的 CrashLoop。
3. **业务容器需自带 `librknnrt.so`。** 插件探测到该库时会自动挂载给业务容器，但插件本身**不提供**它，请从 RKNPU2 SDK 获取并放入业务镜像。
4. **`rknn-toolkit2` 可能仍需 `privileged: true`。** 它读 `/proc/device-tree/compatible` 判断 SoC，而 K8s 默认遮蔽 `/proc`。此为社区经验，本项目**未在麒麟环境实测验证**。
5. **暂不支持 CDI**（Container Device Interface），设备注入依赖默认的 device plugin 挂载机制。
6. **`resourceName` / `maxDevices` 目前是编译期常量。** 修改资源名需要改代码并重新编译；改为命令行参数已列入 Roadmap。

---

## 兼容性 / Compatibility

- **Kubernetes：v1.23+**（依赖 device plugin `v1beta1` API；v0.27+ 要求实现 `GetPreferredAllocation`，本版已实现）
- **架构：linux/arm64**（RK3588 上运行的是 aarch64 内核）
- 仅支持内核已加载 `rknpu` 驱动的系统
- 前置检查：节点上执行 `dmesg | grep -i npu`，应能看到 `Initialized rknpu ... on minor N`

---

## 相关文章 / Related reading

《异构算力实战：RK3588 + 昇腾 310B 部署 K8s + KubeSphere 并实现 NPU 统一调度》
公众号「编码如写诗」：<https://mp.weixin.qq.com/s/eC7ou5RItM8Y5wVOfBbGIQ>

---

## 许可证 / License

[Apache-2.0](https://github.com/gjing1st/rk3588-device-plugin/blob/main/LICENSE) © 2026 天行1st
