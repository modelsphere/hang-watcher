# hang-watcher:引擎 hang 检测 sidecar。纯 Go(CGO off)静态二进制,无第三方依赖(免 go.sum)。
# 构建上下文 = 仓库根目录。CI 打 tag 触发(见 .gitlab-ci.yml)。
FROM harbor.4pd.io/hardcore-tech/golang:1.24-bookworm AS build
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOSUMDB=off GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go build -ldflags="-s -w" -o /hang-watcher .

FROM harbor.4pd.io/hardcore-tech/debian:12-slim
COPY --from=build /hang-watcher /usr/local/bin/hang-watcher
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/hang-watcher"]
