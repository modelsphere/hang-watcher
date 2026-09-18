# hang-watcher: engine hang-detection sidecar. Pure Go (CGO off), statically
# linked, no third-party dependencies (hence no go.sum).
#
# Build context is the repository root:
#     docker build -t hang-watcher:dev .
#
# The base images are overridable so that a build behind a corporate firewall
# can point them at an internal mirror without editing this file:
#     docker build --build-arg GO_IMAGE=<mirror>/golang:1.24-bookworm \
#                  --build-arg RUNTIME_IMAGE=<mirror>/debian:12-slim .
ARG GO_IMAGE=docker.io/library/golang:1.24-bookworm
ARG RUNTIME_IMAGE=docker.io/library/debian:12-slim

FROM ${GO_IMAGE} AS build
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOSUMDB=off GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go build -ldflags="-s -w" -o /hang-watcher .

FROM ${RUNTIME_IMAGE}
COPY --from=build /hang-watcher /usr/local/bin/hang-watcher
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/hang-watcher"]
