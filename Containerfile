# Agora Server image: the HTTP API, the Web UI, and the Daemon relay.
#
# Agents themselves do not run here. Each workstation runs `agora daemon`, which
# owns the local Agent processes and connects to this Server (see
# docs/architecture.md). Containerising the Server is therefore the useful unit:
# it holds no PTYs and no provider history.

# syntax=docker/dockerfile:1

# Build metadata. The Server and the daemon artifacts it publishes are built from
# the same value, so /api/version, /download/version.txt and the running daemon
# agree. CI passes the tag or commit.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# ---------------------------------------------------------------- Web bundle
FROM node:24-alpine AS web
WORKDIR /src/web
# No VITE_LOGTO_* build arguments: Vite would inline them into the JavaScript and
# tie the image to one tenant. The Server serves those settings at /api/config
# instead, so this bundle runs against any Logto deployment.
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --------------------------------------------------------------- Go binaries
FROM golang:1.25-alpine AS build
# Declared per stage: build args do not carry into a stage on their own.
ARG VERSION
ARG COMMIT
ARG BUILD_DATE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so CGO can stay off and the result is a static
# binary that runs on any architecture in the manifest.
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /out/agora ./cmd/agora

# ------------------------------------------------ Daemon binaries (download)
# Workstations get a prebuilt daemon from the Server, so they need no Go
# toolchain. Cross-compile every supported platform and publish the results
# under /download (served by internal/server/download.go).
FROM golang:1.25-alpine AS daemons
ARG VERSION
ARG COMMIT
ARG BUILD_DATE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eux; \
    mkdir -p /out/download; \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      os=${target%/*}; arch=${target#*/}; \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath \
          -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
          -o "/out/download/agora-$os-$arch" ./cmd/agora; \
    done; \
    cp scripts/agora-wrapper.sh /out/download/agora-wrapper.sh; \
    printf '%s\n' "${VERSION}" > /out/download/version.txt; \
    cd /out/download && sha256sum agora-* > checksums.txt

# ------------------------------------------------------------------- Runtime
FROM alpine:3
# ca-certificates: the Server talks to Logto over HTTPS and to webhooks.
# tzdata: event and session timestamps are rendered in the local zone.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 agora

COPY --from=build /out/agora /app/agora
COPY --from=daemons /out/download /app/download
COPY --from=web /src/web/dist /app/web

ENV AGORA_WEB_DIR=/app/web \
    AGORA_SERVER_DB=/data/server.db \
    AGORA_SERVER_ADDR=0.0.0.0:8080 \
    AGORA_DOWNLOAD_DIR=/app/download \
    AGORA_AUTH_MODE=logto

# AGORA_SERVER_ADDR is a non-loopback address, so local trust mode refuses to
# start (see config.ValidateServerAddress): a published container must be
# authenticated. Provide AGORA_LOGTO_ISSUER, AGORA_LOGTO_AUDIENCE and
# AGORA_LOGTO_APP_ID at run time; /api/config hands the client settings to the
# Web UI, so no rebuild is needed for another tenant.
RUN mkdir -p /data && chown -R agora:agora /app /data
USER agora
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/app/agora"]
CMD ["server"]
