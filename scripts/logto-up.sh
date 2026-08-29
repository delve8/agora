#!/usr/bin/env bash
# Bring up the local Logto dev stack (PostgreSQL + Logto) with Podman and wait
# until Logto is ready. Idempotent: reuses the named PostgreSQL volume so the
# provisioned Logto tenant survives restarts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PODMAN="${PODMAN:-podman}"
COMPOSE_FILE="${LOGTO_COMPOSE_FILE:-$ROOT/deploy/logto/compose.yaml}"
PROJECT="${LOGTO_PROJECT:-agora-logto}"
LOGTO_ENDPOINT="${LOGTO_ENDPOINT:-http://127.0.0.1:3003}"

if ! command -v "$PODMAN" >/dev/null 2>&1; then
  echo "error: $PODMAN not found on PATH" >&2
  exit 1
fi
if ! "$PODMAN" compose version >/dev/null 2>&1; then
  echo "error: 'podman compose' is unavailable (install docker-compose or podman-compose)" >&2
  exit 1
fi

"$PODMAN" compose -f "$COMPOSE_FILE" --project-name "$PROJECT" up -d

echo "waiting for Logto OIDC endpoint: ${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration"
for _ in $(seq 1 150); do
  if curl -fsS "${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration" >/dev/null 2>&1; then
    echo "Logto ready at ${LOGTO_ENDPOINT}"
    exit 0
  fi
  sleep 2
done
echo "error: timed out waiting for Logto to become ready" >&2
echo "       inspect logs: $PODMAN compose -f $COMPOSE_FILE --project-name $PROJECT logs --tail 80 logto" >&2
exit 1
