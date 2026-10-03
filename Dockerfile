# 构建阶段：纯静态、无 cgo（SQLite 用纯 Go 驱动，避免 glibc 依赖）
FROM golang:1.26-bookworm AS build
WORKDIR /src

# 先只拷依赖清单，利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/tg-gatekeeper ./cmd/tg-gatekeeper

# 运行阶段
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --create-home --home-dir /app app

WORKDIR /app
COPY --from=build /out/tg-gatekeeper /usr/local/bin/tg-gatekeeper
COPY configs /app/configs
# 数据集与素材由卷挂载；这里给一个目录骨架，避免首次启动缺目录
RUN mkdir -p /app/data/logs /app/data/cache/images && chown -R app:app /app

USER app
ENV TZ=Asia/Shanghai
# 配置里的相对路径（./data、./configs）以 WORKDIR 为基准
ENTRYPOINT ["tg-gatekeeper"]
CMD ["run", "-c", "configs/gatekeeper.yaml"]
