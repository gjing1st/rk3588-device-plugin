package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceName = "rk3588.ai/npu"
	serverSock   = pluginapi.DevicePluginPath + "rk3588-npu.sock"

	// RK3588 NPU 调度并发数
	// 单颗 3588 有 3 个 NPU 核心, 通过单个 DRM render node 访问
	// 内核驱动负责核心调度, 这里设置 K8s 层面的并发限制
	maxDevices = 3
)

var (
	npuDevice     string // 运行环境下的 NPU render 节点路径
	npuLibPath    string // librknnrt.so 路径
)

// RK3588NPUPlugin implements the Kubernetes device plugin API
type RK3588NPUPlugin struct {
	server *grpc.Server
	stop   chan struct{}
}

// detectNPU 自动检测节点上的 NPU 设备路径
// RK3588 NPU 走 DRM 框架, 设备节点是 /dev/dri/renderD<minor+128>
// dmesg 中 "Initialized rknpu ... on minor N" → render 设备 = renderD(128+N)
func detectNPU() (devicePath string, ok bool) {
	// 方式 1: 通过 /sys/class/drm 精确匹配 rknpu 驱动的 render 节点
	if p := findRknpuRenderD(); p != "" {
		log.Printf("[INFO] NPU detected via sysfs: %s", p)
		return p, true
	}

	// 方式 2: 直接检查常见路径
	for _, p := range []string{
		"/dev/dri/renderD129", // minor=1, RK3588 最典型
		"/dev/dri/renderD128", // minor=0, 少数板子 NPU 在 minor=0
		"/dev/galcore",        // 旧版内核设备节点
	} {
		if _, err := os.Stat(p); err == nil {
			log.Printf("[INFO] NPU detected via static path: %s", p)
			return p, true
		}
	}

	return "", false
}

// findRknpuRenderD 遍历 /sys/class/drm/renderD*/device/driver 找 rknpu
func findRknpuRenderD() string {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return ""
	}

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

		// driver 路径中包含 "rknpu" 即为 NPU 设备
		if strings.HasPrefix(driverLink, "/") && strings.Contains(driverLink, "rknpu") {
			return filepath.Join("/dev/dri", name)
		}
		// 某些内核 driver 路径是 "../../../bus/platform/drivers/rknpu"
		if strings.Contains(driverLink, "rknpu") {
			return filepath.Join("/dev/dri", name)
		}
	}

	return ""
}

// findRknnLib 检测 librknnrt.so 路径
func findRknnLib() string {
	for _, p := range []string{
		"/usr/lib/librknnrt.so",
		"/usr/lib/aarch64-linux-gnu/librknnrt.so",
		"/usr/local/lib/librknnrt.so",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Start starts the gRPC server and registers with kubelet
func (m *RK3588NPUPlugin) Start() error {
	if err := os.Remove(serverSock); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sock: %w", err)
	}

	sock, err := net.Listen("unix", serverSock)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	m.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		if err := m.server.Serve(sock); err != nil {
			log.Printf("[ERROR] gRPC server error: %v", err)
		}
	}()

	// 自检连通
	conn, err := grpc.DialContext(context.Background(),
		serverSock,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", addr, 5*time.Second)
		}),
	)
	if err != nil {
		return fmt.Errorf("dial self: %w", err)
	}
	conn.Close()

	if err := m.Register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	log.Printf("[INFO] Plugin started | device=%s | lib=%s | count=%d",
		npuDevice, npuLibPath, maxDevices)
	return nil
}

// Register registers with kubelet
func (m *RK3588NPUPlugin) Register() error {
	conn, err := grpc.DialContext(context.Background(),
		pluginapi.KubeletSocket,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", addr, 5*time.Second)
		}),
	)
	if err != nil {
		return fmt.Errorf("connect kubelet: %w", err)
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     "rk3588-npu.sock",
		ResourceName: resourceName,
		Options: &pluginapi.DevicePluginOptions{
			PreStartRequired: true,
		},
	}

	if _, err := client.Register(context.Background(), req); err != nil {
		return fmt.Errorf("register with kubelet: %w", err)
	}
	return nil
}

// GetDevicePluginOptions returns device plugin options
func (m *RK3588NPUPlugin) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: true,
	}, nil
}

// GetPreferredAllocation 返回首选设备分配 (v1beta1 必须实现)
func (m *RK3588NPUPlugin) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// ListAndWatch returns a list of devices
func (m *RK3588NPUPlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	devs := make([]*pluginapi.Device, maxDevices)
	for i := 0; i < maxDevices; i++ {
		devs[i] = &pluginapi.Device{
			ID:     fmt.Sprintf("rk3588-npu-%d", i),
			Health: pluginapi.Healthy,
		}
	}

	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: devs}); err != nil {
		return err
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

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
			// 也检查库文件
			if npuLibPath != "" {
				if _, err := os.Stat(npuLibPath); os.IsNotExist(err) {
					log.Printf("[WARN] %s missing", npuLibPath)
				}
			}
			for i := range devs {
				devs[i].Health = health
			}
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: devs}); err != nil {
				return err
			}
		}
	}
}

// Allocate is called during pod admission to allocate devices
func (m *RK3588NPUPlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := &pluginapi.AllocateResponse{}

	// 解析设备文件的目录部分 (挂载整个 /dev/dri 而不是单个节点)
	npuDir := filepath.Dir(npuDevice)
	npuBase := filepath.Base(npuDevice)

	for _, req := range reqs.ContainerRequests {
		mounts := []*pluginapi.Mount{
			{
				// 挂载整个 /dev/dri 目录, 容器内可以访问 GPU + NPU 的 render 节点
				ContainerPath: "/dev/dri",
				HostPath:      npuDir,
				ReadOnly:      false,
			},
		}

		// 挂载库文件 (如果找到的话)
		if npuLibPath != "" {
			mounts = append(mounts, &pluginapi.Mount{
				ContainerPath: npuLibPath,
				HostPath:      npuLibPath,
				ReadOnly:      true,
			})
		}

		response := &pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{
				"RKNN_NPU_DEVICE":    fmt.Sprintf("/dev/dri/%s", npuBase),
				"RKNN_NPU_ENABLE":    "1",
				"RKNN_LOG_LEVEL":     "0",
				"RK3588_NPU_DEVICES": fmt.Sprintf("%v", req.DevicesIDs),
			},
			Mounts: mounts,
			Annotations: map[string]string{
				"rk3588.ai/npu-allocated": "true",
			},
		}

		responses.ContainerResponses = append(responses.ContainerResponses, response)
	}

	return responses, nil
}

// PreStartContainer is called before container start
func (m *RK3588NPUPlugin) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	log.Printf("[INFO] PreStartContainer for device IDs: %v", req.DevicesIDs)
	return &pluginapi.PreStartContainerResponse{}, nil
}

// Stop shuts down the plugin
func (m *RK3588NPUPlugin) Stop() error {
	close(m.stop)
	if m.server != nil {
		m.server.Stop()
	}
	return os.Remove(serverSock)
}

func main() {
	flag.Parse()

	log.Println("[INFO] RK3588 NPU Device Plugin starting...")

	var ok bool
	npuDevice, ok = detectNPU()
	if !ok {
		log.Println("[WARN] No RK3588 NPU device found, exiting gracefully (non-3588 node)")
		os.Exit(0)
	}

	npuLibPath = findRknnLib()
	if npuLibPath == "" {
		log.Println("[WARN] librknnrt.so not found, container will need it from its own image")
	}

	plugin := &RK3588NPUPlugin{
		stop: make(chan struct{}),
	}

	if err := plugin.Start(); err != nil {
		log.Fatalf("[FATAL] Failed to start: %v", err)
	}

	select {}
}
