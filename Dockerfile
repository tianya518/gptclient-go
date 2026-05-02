# ─── Stage 1: Build ─────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS builder

WORKDIR /build

# 安装必要工具
RUN apk add --no-cache git ca-certificates

# 先复制依赖文件，利用缓存层
COPY go.mod go.sum ./
RUN go mod download

# 复制源码并构建
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /build/sentinel-server ./cmd/server/

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

# 设置时区
ENV TZ=Asia/Shanghai

WORKDIR /app

# 复制二进制文件
COPY --from=builder /build/sentinel-server .

# 创建数据目录
RUN mkdir -p /app/images

# 默认环境变量
ENV PORT=5005
ENV DEFAULT_MODEL=gpt-5-5-thinking
ENV TEMP_MODE=false
ENV IMAGE_DIR=/app/images
ENV TOKENS_FILE=/app/tokens.txt
ENV SESSION_TTL_MINUTES=120

EXPOSE 5005

ENTRYPOINT ["/app/sentinel-server"]
