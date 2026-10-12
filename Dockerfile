# syntax=docker/dockerfile:1
# CI reads this version from go.mod and passes it as GO_VERSION as well.
ARG GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/index.html web/vite.config.js ./
COPY web/src ./src
RUN npm run build && test -s dist/index.html

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# 版本号来自 version.json，编译时嵌入程序
COPY version.json version.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/embed.go
COPY --from=frontend /src/web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test "$TARGETOS" = linux && \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac && \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o /out/pve-forward ./cmd/pve-forward && \
    GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o /out/pve-forward-agent-linux-amd64 ./cmd/pve-forward-agent && \
    GOOS=linux GOARCH=arm64 go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o /out/pve-forward-agent-linux-arm64 ./cmd/pve-forward-agent

FROM debian:bookworm-slim AS runtime
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --gid 10001 pve-forward && \
    useradd --uid 10001 --gid 10001 --no-create-home --home-dir /var/lib/pve-forward --shell /usr/sbin/nologin pve-forward && \
    install -d -m 0700 -o 10001 -g 10001 /var/lib/pve-forward
COPY --from=builder --chmod=0755 /out/pve-forward /out/pve-forward-agent-linux-amd64 /out/pve-forward-agent-linux-arm64 /usr/local/bin/
LABEL org.opencontainers.image.source="https://github.com/chunyu-zhou/pve-forward" \
      org.opencontainers.image.title="PVE Forward"
# 控制台据此显示 Docker 更新方式；在线更新的程序保存在数据卷 updates/ 中
ENV PVE_FORWARD_RUNTIME=docker
USER 10001:10001
WORKDIR /var/lib/pve-forward
VOLUME ["/var/lib/pve-forward"]
EXPOSE 5000
STOPSIGNAL SIGTERM
# Keep application defaults (HTTPS with a self-signed certificate on :5000).
# No shell wrapper, systemd, elevated capabilities or privileged container required.
ENTRYPOINT ["/usr/local/bin/pve-forward"]
CMD ["run"]
