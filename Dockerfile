# 构建：一个镜像里放网站、后台任务和几个运维命令，都是静态链接的单个文件。
# 模板、静态文件、表结构都编进了二进制，运行时不需要别的文件。
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for name in web worker migrate-legacy mailcheck tgcheck hongguo-seed; do \
      CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/$name ./cmd/$name || exit 1; \
    done

# 运行：非 root 用户，只带证书和时区数据。
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S moovie && adduser -S -G moovie moovie
WORKDIR /app
COPY --from=builder /out/ ./
ENV PORT=5008 APP_ENV=production TZ=Asia/Shanghai
EXPOSE 5008
USER moovie
ENTRYPOINT ["./web"]
