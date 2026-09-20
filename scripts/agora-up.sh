#!/usr/bin/env bash
# One-command container stack: Postgres + Logto + Agora Server.
#
# Uses only `docker run` / `podman run` (no compose). Volumes survive updates.
#
#   ./scripts/agora-up.sh           # pull, create or replace, bootstrap Logto
#   ./scripts/agora-up.sh status
#   ./scripts/agora-up.sh down      # stop containers, keep data
#   ./scripts/agora-up.sh purge     # stop containers and delete volumes
#
# Default bind is loopback HTTP. Set AGORA_DOMAIN and LOGTO_DOMAIN to put
# Caddy in front with Let's Encrypt and publish 80/443 instead:
#
#   AGORA_DOMAIN=agora.example.com LOGTO_DOMAIN=logto.example.com \
#     CADDY_EMAIL=you@example.com ./scripts/agora-up.sh
#
# Optional email sign-in: set AGORA_SMTP_HOST, AGORA_SMTP_USER,
# AGORA_SMTP_PASSWORD and AGORA_SMTP_FROM_EMAIL to provision an SMTP connector
# and let users sign in with an email verification code.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CMD="${1:-up}"

ENGINE="${ENGINE:-${CONTAINER_ENGINE:-}}"
if [ -z "$ENGINE" ]; then
  if command -v podman >/dev/null 2>&1; then
    ENGINE=podman
  elif command -v docker >/dev/null 2>&1; then
    ENGINE=docker
  else
    echo "error: podman or docker is required" >&2
    exit 1
  fi
fi

NETWORK="${AGORA_NETWORK:-agora}"
AGORA_NAME="${AGORA_CONTAINER:-agora}"
LOGTO_NAME="${LOGTO_CONTAINER:-agora-logto}"
PG_NAME="${LOGTO_POSTGRES_CONTAINER:-agora-logto-postgres}"
PROXY_NAME="${AGORA_OIDC_PROXY_CONTAINER:-agora-oidc-proxy}"
CADDY_NAME="${CADDY_CONTAINER:-agora-caddy}"

AGORA_IMAGE="${AGORA_IMAGE:-ghcr.io/delve8/agora:latest}"
LOGTO_IMAGE="${LOGTO_IMAGE:-svhd/logto:latest}"
LOGTO_POSTGRES_IMAGE="${LOGTO_POSTGRES_IMAGE:-postgres:17-alpine}"
SOCAT_IMAGE="${SOCAT_IMAGE:-docker.io/alpine/socat:1.8.0.0}"
CADDY_IMAGE="${CADDY_IMAGE:-docker.io/library/caddy:2}"

AGORA_VOLUME="${AGORA_VOLUME:-agora-data}"
PG_VOLUME="${LOGTO_POSTGRES_VOLUME:-agora-logto-postgres}"
CADDY_VOLUME="${CADDY_VOLUME:-agora-caddy-data}"
PG_PASSWORD="${LOGTO_POSTGRES_PASSWORD:-agora-logto-dev}"

AGORA_SMTP_HOST="${AGORA_SMTP_HOST:-}"
AGORA_SMTP_PORT="${AGORA_SMTP_PORT:-465}"
AGORA_SMTP_SECURE="${AGORA_SMTP_SECURE:-true}"
AGORA_SMTP_USER="${AGORA_SMTP_USER:-}"
AGORA_SMTP_PASSWORD="${AGORA_SMTP_PASSWORD:-}"
AGORA_SMTP_FROM_EMAIL="${AGORA_SMTP_FROM_EMAIL:-}"
AGORA_SMTP_REPLY_TO="${AGORA_SMTP_REPLY_TO:-}"
AGORA_ALLOW_REGISTRATION="${AGORA_ALLOW_REGISTRATION:-}"

AGORA_PORT="${AGORA_PORT:-8080}"
LOGTO_PORT="${LOGTO_PORT:-3003}"
LOGTO_ADMIN_PORT="${LOGTO_ADMIN_PORT:-3004}"
AGORA_BIND="${AGORA_BIND:-127.0.0.1}"

AGORA_DOMAIN="${AGORA_DOMAIN:-}"
LOGTO_DOMAIN="${LOGTO_DOMAIN:-}"
LOGTO_ADMIN_DOMAIN="${LOGTO_ADMIN_DOMAIN:-}"
CADDY_EMAIL="${CADDY_EMAIL:-}"

TLS=0
if [ -n "$AGORA_DOMAIN" ] || [ -n "$LOGTO_DOMAIN" ]; then
  TLS=1
  [ -n "$AGORA_DOMAIN" ] || fail_later="AGORA_DOMAIN"
  [ -n "$LOGTO_DOMAIN" ] || fail_later="${fail_later:+$fail_later and }LOGTO_DOMAIN"
  [ -n "$CADDY_EMAIL" ] || fail_later="${fail_later:+$fail_later and }CADDY_EMAIL"
  if [ -n "${fail_later:-}" ]; then
    echo "error: TLS mode needs $fail_later" >&2
    exit 1
  fi
  if [ -z "$LOGTO_ADMIN_DOMAIN" ]; then
    LOGTO_ADMIN_DOMAIN="admin.${LOGTO_DOMAIN}"
  fi
  AGORA_ORIGIN="https://${AGORA_DOMAIN}"
  LOGTO_ENDPOINT="https://${LOGTO_DOMAIN}"
  LOGTO_ADMIN_ENDPOINT="https://${LOGTO_ADMIN_DOMAIN}"
else
  LOGTO_ENDPOINT="${LOGTO_ENDPOINT:-http://127.0.0.1:${LOGTO_PORT}}"
  LOGTO_ADMIN_ENDPOINT="${LOGTO_ADMIN_ENDPOINT:-http://127.0.0.1:${LOGTO_ADMIN_PORT}}"
  AGORA_ORIGIN="${AGORA_ORIGIN:-http://127.0.0.1:${AGORA_PORT}}"
fi

STATE_DIR="${LOGTO_STATE_DIR:-$ROOT/.agora/logto}"
ENV_FILE="${LOGTO_ENV_FILE:-$STATE_DIR/env}"

fail() { echo "error: $*" >&2; exit 1; }

have() { "$ENGINE" inspect "$1" >/dev/null 2>&1; }

running() {
  [ "$("$ENGINE" inspect -f '{{.State.Running}}' "$1" 2>/dev/null || echo false)" = true ]
}

container_ip() {
  "$ENGINE" inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$1"
}

rm_container() {
  if have "$1"; then
    "$ENGINE" rm -f "$1" >/dev/null
  fi
}

ensure_network() {
  if ! "$ENGINE" network inspect "$NETWORK" >/dev/null 2>&1; then
    "$ENGINE" network create "$NETWORK" >/dev/null
  fi
}

ensure_volume() {
  if ! "$ENGINE" volume inspect "$1" >/dev/null 2>&1; then
    "$ENGINE" volume create "$1" >/dev/null
  fi
}

wait_http() {
  local url="$1" label="$2" n="${3:-90}"
  echo "waiting for $label: $url"
  local i
  for i in $(seq 1 "$n"); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      echo "$label ready"
      return 0
    fi
    sleep 2
  done
  fail "timed out waiting for $label ($url)"
}

wait_postgres() {
  echo "waiting for Postgres in $PG_NAME"
  local i
  for i in $(seq 1 60); do
    if "$ENGINE" exec "$PG_NAME" pg_isready -U postgres -d logto >/dev/null 2>&1; then
      echo "Postgres ready"
      return 0
    fi
    sleep 2
  done
  fail "timed out waiting for Postgres"
}

pull_images() {
  echo "pulling $AGORA_IMAGE"
  "$ENGINE" pull "$AGORA_IMAGE"
  echo "pulling $LOGTO_IMAGE"
  "$ENGINE" pull "$LOGTO_IMAGE"
  echo "pulling $LOGTO_POSTGRES_IMAGE"
  "$ENGINE" pull "$LOGTO_POSTGRES_IMAGE"
  if [ "$TLS" = 1 ]; then
    echo "pulling $CADDY_IMAGE"
    "$ENGINE" pull "$CADDY_IMAGE"
  else
    echo "pulling $SOCAT_IMAGE"
    "$ENGINE" pull "$SOCAT_IMAGE"
  fi
}

run_postgres() {
  rm_container "$PG_NAME"
  "$ENGINE" run -d --name "$PG_NAME" \
    --network "$NETWORK" \
    --network-alias postgres \
    --restart unless-stopped \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD="$PG_PASSWORD" \
    -e POSTGRES_DB=logto \
    -v "$PG_VOLUME":/var/lib/postgresql/data \
    "$LOGTO_POSTGRES_IMAGE" >/dev/null
}

run_logto() {
  rm_container "$LOGTO_NAME"
  local extra=()
  extra=(--network-alias logto)
  if [ "$TLS" != 1 ]; then
    extra+=(-p "${AGORA_BIND}:${LOGTO_PORT}:3001" -p "${AGORA_BIND}:${LOGTO_ADMIN_PORT}:3002")
  fi
  "$ENGINE" run -d --name "$LOGTO_NAME" \
    --network "$NETWORK" \
    --restart unless-stopped \
    "${extra[@]}" \
    -e TRUST_PROXY_HEADER=1 \
    -e "DB_URL=postgres://postgres:${PG_PASSWORD}@postgres:5432/logto" \
    -e "ENDPOINT=${LOGTO_ENDPOINT}" \
    -e "ADMIN_ENDPOINT=${LOGTO_ADMIN_ENDPOINT}" \
    --entrypoint sh \
    "$LOGTO_IMAGE" \
    -c "npm run cli db seed -- --swe && npm start" >/dev/null
}

write_caddyfile() {
  mkdir -p "$STATE_DIR"
  cat > "$STATE_DIR/Caddyfile" <<EOF
{
	email ${CADDY_EMAIL}
}

${AGORA_DOMAIN} {
	reverse_proxy agora:8080
}

${LOGTO_DOMAIN} {
	reverse_proxy logto:3001
}

${LOGTO_ADMIN_DOMAIN} {
	reverse_proxy logto:3002
}
EOF
}

run_caddy() {
  write_caddyfile
  rm_container "$CADDY_NAME"
  ensure_volume "$CADDY_VOLUME"
  "$ENGINE" run -d --name "$CADDY_NAME" \
    --network "$NETWORK" \
    --restart unless-stopped \
    -p "80:80" \
    -p "443:443" \
    -p "443:443/udp" \
    -e "CADDY_EMAIL=${CADDY_EMAIL}" \
    -v "$CADDY_VOLUME":/data \
    -v "$STATE_DIR/Caddyfile":/etc/caddy/Caddyfile:ro \
    "$CADDY_IMAGE" >/dev/null
}

run_agora() {
  rm_container "$PROXY_NAME"
  rm_container "$AGORA_NAME"
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
  local extra=()
  if [ "$TLS" = 1 ]; then
    local caddy_ip
    extra=(--network-alias agora)
    caddy_ip="$(container_ip "$CADDY_NAME")"
    [ -n "$caddy_ip" ] || fail "Caddy has no IP on network $NETWORK"
    extra+=(--add-host "${LOGTO_DOMAIN}:${caddy_ip}" --add-host "${LOGTO_ADMIN_DOMAIN}:${caddy_ip}" --add-host "${AGORA_DOMAIN}:${caddy_ip}")
  else
    extra=(-p "${AGORA_BIND}:${AGORA_PORT}:8080")
  fi
  "$ENGINE" run -d --name "$AGORA_NAME" \
    --network "$NETWORK" \
    --restart unless-stopped \
    "${extra[@]}" \
    -v "$AGORA_VOLUME":/data \
    -e AGORA_AUTH_MODE=logto \
    -e AGORA_SERVER_ADDR=0.0.0.0:8080 \
    -e AGORA_SERVER_DB=/data/server.db \
    -e AGORA_PUBLIC_URL="$AGORA_ORIGIN" \
    -e AGORA_LOGTO_ISSUER="${AGORA_LOGTO_ISSUER}" \
    -e AGORA_LOGTO_AUDIENCE="${AGORA_LOGTO_AUDIENCE}" \
    -e AGORA_LOGTO_ENDPOINT="${AGORA_LOGTO_ENDPOINT:-${VITE_LOGTO_ENDPOINT:-}}" \
    -e AGORA_LOGTO_APP_ID="${AGORA_LOGTO_APP_ID:-${VITE_LOGTO_APP_ID:-}}" \
    -e AGORA_LOGTO_PROVISIONING="${AGORA_LOGTO_PROVISIONING:-enabled}" \
    "$AGORA_IMAGE" >/dev/null
}

run_oidc_proxy() {
  rm_container "$PROXY_NAME"
  # Agora validates JWKS over HTTP only for loopback. The Server container
  # therefore needs 127.0.0.1:3003 to reach Logto's published OIDC port.
  "$ENGINE" run -d --name "$PROXY_NAME" \
    --network "container:$AGORA_NAME" \
    --restart unless-stopped \
    "$SOCAT_IMAGE" \
    TCP-LISTEN:3003,bind=127.0.0.1,fork,reuseaddr TCP:"${LOGTO_NAME}":3001 >/dev/null
}

bootstrap_logto() {
  command -v jq >/dev/null 2>&1 || fail "jq is required to provision Logto"
  command -v curl >/dev/null 2>&1 || fail "curl is required to provision Logto"
  ENGINE="$ENGINE" PODMAN="$ENGINE" \
    LOGTO_POSTGRES_CONTAINER="$PG_NAME" \
    LOGTO_ENDPOINT="$LOGTO_ENDPOINT" \
    LOGTO_ADMIN_ENDPOINT="$LOGTO_ADMIN_ENDPOINT" \
    AGORA_SERVER_ADDR="$AGORA_ORIGIN" \
    AGORA_LOGTO_AUDIENCE="${AGORA_LOGTO_AUDIENCE:-}" \
    LOGTO_STATE_DIR="$STATE_DIR" \
    LOGTO_ENV_FILE="$ENV_FILE" \
    AGORA_LOGTO_BOOTSTRAP_USERNAME="${AGORA_LOGTO_BOOTSTRAP_USERNAME:-}" \
    AGORA_LOGTO_BOOTSTRAP_PASSWORD="${AGORA_LOGTO_BOOTSTRAP_PASSWORD:-}" \
    AGORA_SMTP_HOST="$AGORA_SMTP_HOST" \
    AGORA_SMTP_PORT="$AGORA_SMTP_PORT" \
    AGORA_SMTP_SECURE="$AGORA_SMTP_SECURE" \
    AGORA_SMTP_USER="$AGORA_SMTP_USER" \
    AGORA_SMTP_PASSWORD="$AGORA_SMTP_PASSWORD" \
    AGORA_SMTP_FROM_EMAIL="$AGORA_SMTP_FROM_EMAIL" \
    AGORA_SMTP_REPLY_TO="$AGORA_SMTP_REPLY_TO" \
    AGORA_ALLOW_REGISTRATION="$AGORA_ALLOW_REGISTRATION" \
    "$ROOT/scripts/logto-bootstrap.sh"
}

print_status() {
  printf '%-22s %s\n' "engine" "$ENGINE"
  printf '%-22s %s\n' "network" "$NETWORK"
  if [ "$TLS" = 1 ]; then
    printf '%-22s %s\n' "tls" "caddy ${AGORA_DOMAIN} / ${LOGTO_DOMAIN}"
  else
    printf '%-22s %s\n' "tls" "off (loopback HTTP)"
  fi
  for name in "$PG_NAME" "$LOGTO_NAME" "$AGORA_NAME" "$PROXY_NAME" "$CADDY_NAME"; do
    if running "$name"; then
      printf '%-22s running\n' "$name"
    elif have "$name"; then
      printf '%-22s stopped\n' "$name"
    else
      printf '%-22s missing\n' "$name"
    fi
  done
}

stack_down() {
  rm_container "$PROXY_NAME"
  rm_container "$CADDY_NAME"
  rm_container "$AGORA_NAME"
  rm_container "$LOGTO_NAME"
  rm_container "$PG_NAME"
}

stack_purge() {
  stack_down
  "$ENGINE" volume rm -f "$AGORA_VOLUME" >/dev/null 2>&1 || true
  "$ENGINE" volume rm -f "$PG_VOLUME" >/dev/null 2>&1 || true
  "$ENGINE" volume rm -f "$CADDY_VOLUME" >/dev/null 2>&1 || true
  "$ENGINE" network rm "$NETWORK" >/dev/null 2>&1 || true
}

stack_up() {
  command -v curl >/dev/null 2>&1 || fail "curl is required"
  ensure_network
  ensure_volume "$AGORA_VOLUME"
  ensure_volume "$PG_VOLUME"
  pull_images
  run_postgres
  wait_postgres
  run_logto
  if [ "$TLS" = 1 ]; then
    run_caddy
    wait_http "${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration" "Logto (HTTPS)" 120
  else
    wait_http "${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration" "Logto"
  fi
  bootstrap_logto
  run_agora
  if [ "$TLS" = 1 ]; then
    wait_http "${AGORA_ORIGIN}/healthz" "Agora (HTTPS)" 120
  else
    run_oidc_proxy
    wait_http "${AGORA_ORIGIN}/healthz" "Agora"
  fi
  echo
  echo "Agora:  ${AGORA_ORIGIN}"
  echo "Logto:  ${LOGTO_ENDPOINT}"
  echo "Admin:  ${LOGTO_ADMIN_ENDPOINT}"
  if [ -f "$STATE_DIR/bootstrap-password" ]; then
    echo "Login:  username=${AGORA_LOGTO_BOOTSTRAP_USERNAME:-agora_admin}  (password in ${STATE_DIR}/bootstrap-password)"
  fi
}

case "$CMD" in
  up|update|"")
    stack_up
    ;;
  status)
    print_status
    ;;
  down)
    stack_down
    echo "stopped containers; volumes kept"
    ;;
  purge)
    stack_purge
    echo "removed containers, volumes, and network $NETWORK"
    ;;
  *)
    echo "usage: $0 [up|update|status|down|purge]" >&2
    exit 2
    ;;
esac
