# syntax=docker/dockerfile:1

# 在构建机的原生架构上交叉编译,而不是靠 QEMU 模拟目标架构:
# 纯 Go 代码关掉 CGO 就能直接产出目标架构的静态二进制,多架构构建因此快一个数量级。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# 依赖描述单独成层,依赖没变时这一层的缓存可以复用
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS TARGETARCH
# VERSION 由 CI 从 tag 注入;本地构建保持 dev,与 go install 装出来的行为一致
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/refurb-sentry ./cmd/refurb-sentry

FROM alpine:3.22

# ca-certificates:抓 Apple 站点要校验 TLS 证书,缺了根本连不上。
# tzdata:日志时间戳跟随 TZ 环境变量,否则只有 UTC。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 app

WORKDIR /app

# 状态目录归 app 所有。compose 用命名卷时 Docker 会带着这个属主初始化卷,
# 用户不必自己 chown;换成 bind mount 就得自己保证宿主目录属于 uid 1000。
RUN mkdir -p /app/data /app/configs && chown -R app:app /app

COPY --from=build /out/refurb-sentry /usr/local/bin/refurb-sentry

USER app

# -config 的默认值就是相对 WORKDIR 的 configs/config.yaml,不必再传参。
# ENTRYPOINT 而非 CMD:这样 docker run ... -once -dry-run 能直接把参数接上去。
ENTRYPOINT ["refurb-sentry"]
