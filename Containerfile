# Agora Server image: the HTTP API, the Web UI, and the Daemon relay.
#
# Agents themselves do not run here. Each workstation runs `agora daemon`, which
# owns the local Agent processes and connects to this Server (see
# docs/architecture.md). Containerising the Server is therefore the useful unit:
# it holds no PTYs and no provider history.

# syntax=docker/dockerfile:1

# ---------------------------------------------------------------- Web bundle
FROM node:24-alpine AS web
WORKDIR /src/web
# Vite inlines these at build time, so a Logto deployment must pass them here.
# Left empty, the bundle falls back to whatever /api/me reports.
ARG VITE_LOGTO_ENDPOINT=""
ARG VITE_LOGTO_APP_ID=""
ARG VITE_LOGTO_AUDIENCE=""
ENV VITE_LOGTO_ENDPOINT=${VITE_LOGTO_ENDPOINT} \
    VITE_LOGTO_APP_ID=${VITE_LOGTO_APP_ID} \
    VITE_LOGTO_AUDIENCE=${VITE_LOGTO_AUDIENCE}
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --------------------------------------------------------------- Go binaries
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so CGO can stay off and the result is a static
# binary that runs on any architecture in the manifest.
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/agora ./cmd/agora

# ------------------------------------------------------------------- Runtime
FROM alpine:3
# ca-certificates: the Server talks to Logto over HTTPS and to webhooks.
# tzdata: event and session timestamps are rendered in the local zone.
# busybox already provides the wget used by the healthcheck.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 agora

COPY --from=build /out/agora /app/agora
COPY --from=web /src/web/dist /app/web

ENV AGORA_WEB_DIR=/app/web \
    AGORA_SERVER_DB=/data/server.db \
    AGORA_SERVER_ADDR=0.0.0.0:8080 \
    AGORA_AUTH_MODE=logto

# AGORA_SERVER_ADDR is a non-loopback address, so local trust mode refuses to
# start (see config.ValidateServerAddress): a published container must be
# authenticated. Provide AGORA_LOGTO_ISSUER and AGORA_LOGTO_AUDIENCE, and build
# the bundle with the matching VITE_LOGTO_* build arguments.
RUN mkdir -p /data && chown -R agora:agora /app /data
USER agora
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/agora"]
CMD ["server"]
