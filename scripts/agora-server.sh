#!/usr/bin/env bash
# `make server` driver.
#
# Modes, in order of precedence:
#   1. AGORA_AUTH_MODE=local  -> trust-local Agora Server (no Logto at all)
#   2. External Logto          -> AGORA_LOGTO_ISSUER + AGORA_LOGTO_AUDIENCE
#      + VITE_LOGTO_ENDPOINT + VITE_LOGTO_APP_ID all set by the caller
#   3. Default (dev)           -> local Podman Logto, provisioned on first run
#
# Mode 3 is what makes `make server` self-contained: it starts the Logto stack
# with Podman, provisions the API resource / SPA application / bootstrap admin,
# builds the web bundle with the Logto config baked in, then runs the Agora
# server in logto mode against it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN="${AGORA_BIN:-$ROOT/bin/agora}"
WEB_DIR="${WEB_DIR:-$ROOT/web}"
DIST_DIR="${AGORA_WEB_DIR:-$WEB_DIR/dist}"
SERVER_DB="${AGORA_SERVER_DB:-$ROOT/.agora/server.db}"
SERVER_ADDR="${AGORA_SERVER_ADDR:-127.0.0.1:8080}"
STATE_DIR="${LOGTO_STATE_DIR:-$ROOT/.agora/logto}"
ENV_FILE="${LOGTO_ENV_FILE:-$STATE_DIR/env}"
NPM="${NPM:-npm}"

mkdir -p "$(dirname "$SERVER_DB")"

# --- mode 1: local ---
if [ "${AGORA_AUTH_MODE:-}" = "local" ]; then
  echo "agora: AGORA_AUTH_MODE=local, starting trust-local server (no Logto)"
  exec env AGORA_SERVER_ADDR="$SERVER_ADDR" AGORA_SERVER_DB="$SERVER_DB" AGORA_WEB_DIR="$DIST_DIR" \
    "$BIN" server
fi

# --- mode 2: external Logto ---
EXTERNAL=0
if [ -n "${AGORA_LOGTO_ISSUER:-}" ] && [ -n "${AGORA_LOGTO_AUDIENCE:-}" ] && \
   [ -n "${VITE_LOGTO_ENDPOINT:-}" ] && [ -n "${VITE_LOGTO_APP_ID:-}" ]; then
  EXTERNAL=1
fi
if [ "$EXTERNAL" = "1" ]; then
  echo "agora: using external Logto (issuer=${AGORA_LOGTO_ISSUER} audience=${AGORA_LOGTO_AUDIENCE})"
else
  # --- mode 3: local Podman Logto ---
  export LOGTO_COMPOSE_FILE="${LOGTO_COMPOSE_FILE:-$ROOT/deploy/logto/compose.yaml}"
  export LOGTO_PROJECT="${LOGTO_PROJECT:-agora-logto}"
  export LOGTO_ENDPOINT="${LOGTO_ENDPOINT:-http://127.0.0.1:3003}"
  export LOGTO_ADMIN_ENDPOINT="${LOGTO_ADMIN_ENDPOINT:-http://127.0.0.1:3004}"
  export LOGTO_IMAGE="${LOGTO_IMAGE:-svhd/logto:latest}"
  export LOGTO_POSTGRES_IMAGE="${LOGTO_POSTGRES_IMAGE:-postgres:17-alpine}"
  export LOGTO_POSTGRES_PASSWORD="${LOGTO_POSTGRES_PASSWORD:-agora-logto-dev}"
  export AGORA_SERVER_ADDR="$SERVER_ADDR"
  export AGORA_LOGTO_AUDIENCE="${AGORA_LOGTO_AUDIENCE:-}"
  export VITE_LOGTO_ENDPOINT="${VITE_LOGTO_ENDPOINT:-}"
  export VITE_LOGTO_APP_ID="${VITE_LOGTO_APP_ID:-}"
  export VITE_LOGTO_AUDIENCE="${VITE_LOGTO_AUDIENCE:-}"
  export LOGTO_STATE_DIR="$STATE_DIR"
  export LOGTO_ENV_FILE="$ENV_FILE"

  ./scripts/logto-up.sh
  AGORA_LOGTO_BOOTSTRAP_USERNAME="${AGORA_LOGTO_BOOTSTRAP_USERNAME:-}" \
    AGORA_LOGTO_BOOTSTRAP_PASSWORD="${AGORA_LOGTO_BOOTSTRAP_PASSWORD:-}" \
    ./scripts/logto-bootstrap.sh
  set -a; . "$ENV_FILE"; set +a
fi

# --- build the web bundle with VITE_LOGTO_* in the environment ---
if [ ! -d "$WEB_DIR/node_modules" ]; then
  "$NPM" --prefix "$WEB_DIR" ci
fi
"$NPM" --prefix "$WEB_DIR" run build

exec env AGORA_AUTH_MODE=logto \
  AGORA_LOGTO_ISSUER="${AGORA_LOGTO_ISSUER:-}" \
  AGORA_LOGTO_AUDIENCE="${AGORA_LOGTO_AUDIENCE:-}" \
  AGORA_LOGTO_PROVISIONING="${AGORA_LOGTO_PROVISIONING:-enabled}" \
  AGORA_SERVER_ADDR="$SERVER_ADDR" \
  AGORA_SERVER_DB="$SERVER_DB" \
  AGORA_WEB_DIR="$DIST_DIR" \
  "$BIN" server
