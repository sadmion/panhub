# syntax=docker/dockerfile:1

# ---------- 1. 前端：Vue3 + Vite ----------
FROM node:22-alpine AS web
WORKDIR /src/web
# 国内网络更稳；海外构建可删掉这行
ENV npm_config_registry=https://registry.npmmirror.com
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# vite.config.ts 的 outDir 是 ../internal/webui/dist → /src/internal/webui/dist
RUN npm run build

# ---------- 2. 后端：Go 静态编译 ----------
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=https://goproxy.cn,direct \
    GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用刚才构建的前端产物覆盖占位 dist，go:embed 会把它打进二进制
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN go build -trimpath -ldflags "-s -w" -o /out/panhub ./cmd/panhub

# ---------- 3. 运行镜像 ----------
FROM alpine:3.20

LABEL org.opencontainers.image.title="panhub" \
      org.opencontainers.image.description="多网盘聚合挂载：WebDAV + 浏览 + STRM + 定时任务" \
      org.opencontainers.image.source="https://github.com/YOUR_NAME/panhub" \
      org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

COPY --from=build /out/panhub /usr/local/bin/panhub
COPY config.example.yaml /app/config.example.yaml

ENV TZ=Asia/Shanghai
EXPOSE 5211
VOLUME ["/app/data", "/app/strm"]

# 没有 /app/config.yaml 也能启动（用默认值：:5211 / 密码 admin / ./data）
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
    CMD wget -q -O /dev/null http://127.0.0.1:5211/ || exit 1

ENTRYPOINT ["panhub"]
CMD ["-config", "/app/config.yaml"]
