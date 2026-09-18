#!/bin/bash
# ============================================
# 直接在 3588 节点上编译 + 部署 device plugin
# 前提: 节点上有 Go 1.20+ 和 Docker
#
# 用法: bash build-on-node.sh [镜像仓库地址]
# 示例: bash build-on-node.sh harbor.local/rknn/rk3588-device-plugin:v1.0.0
# ============================================

set -euo pipefail

IMAGE="${1:-rk3588-device-plugin:local}"

echo "==> 编译 device plugin (本地 aarch64) ..."
cd "$(dirname "$0")/.."

go mod tidy 2>/dev/null || true
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o rk3588-device-plugin .

file rk3588-device-plugin

echo "==> 构建 Docker 镜像: $IMAGE ..."

# 最小化 Dockerfile (不需要多阶段构建)
cat > Dockerfile.local <<'DOCKERFILE'
FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY rk3588-device-plugin /usr/bin/rk3588-device-plugin
ENTRYPOINT ["/usr/bin/rk3588-device-plugin"]
DOCKERFILE

docker build -f Dockerfile.local -t "$IMAGE" .
rm -f Dockerfile.local

echo "==> 推送镜像 (如不需要推送可跳过) ..."
docker push "$IMAGE" 2>/dev/null || echo "[INFO] 跳过推送, 使用本地镜像"

echo "==> 部署 DaemonSet ..."
# 临时替换镜像名
sed "s|your-registry/rk3588-device-plugin:v1.0.0|$IMAGE|g" deploy/daemonset.yaml | kubectl apply -f -

echo "==> 等待 Pod 就绪 ..."
kubectl -n kube-system rollout status daemonset/rk3588-npu-device-plugin --timeout=60s

echo "==> 检查日志 ..."
kubectl -n kube-system logs daemonset/rk3588-npu-device-plugin --tail=5

echo ""
echo "==> 验证 node capacity ..."
rk_node=$(kubectl get nodes -l hardware-type=rk3588 -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "$rk_node" ]; then
  kubectl describe node "$rk_node" | grep -A2 rk3588 || true
else
  echo "[INFO] 未找到带 hardware-type=rk3588 标签的节点"
  echo "       请先执行: kubectl label node <你的RK3588节点> hardware-type=rk3588"
  echo "       再手动验证: kubectl describe node <节点名> | grep rk3588"
fi
