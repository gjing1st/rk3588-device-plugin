# ============================================
# 构建命令 (在 rk3588-device-plugin 目录下执行):
#   docker buildx build --platform linux/arm64 -t rk3588-device-plugin:v1.0.0 --load .
#
# 镜像说明:
#   - 构建阶段: 使用本地 arm64 golang:1.24.2-alpine3.21, 产出原生 arm64 二进制
#   - 运行阶段: 使用本地 arm64 alpine:3.21.3, 最终镜像约 15MB
#   - 依赖: 已 vendor 到 vendor/ 目录, 构建全程无需联网
# ============================================

FROM golang:1.24.2-alpine3.21 AS builder

WORKDIR /build

COPY go.mod go.sum ./
COPY main.go .
COPY vendor/ vendor/

RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o rk3588-device-plugin .

FROM alpine:3.21.3

RUN apk add --no-cache ca-certificates

COPY --from=builder /build/rk3588-device-plugin /usr/bin/rk3588-device-plugin

ENTRYPOINT ["/usr/bin/rk3588-device-plugin"]
