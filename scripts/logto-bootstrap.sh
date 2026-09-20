#!/usr/bin/env bash
# Provision the local Logto default tenant (idempotent).
#
# Reads the pre-seeded `m-default` machine-to-machine secret from the Logto
# database, obtains a Management API access token for the default tenant, then
# creates or reuses:
#   - the Agora API resource (the audience/indicator for access tokens)
#   - the Agora Web SPA application (the client id for the frontend)
#   - a bootstrap user for local Agora sign-in
#
# Finally writes LOGTO_* / AGORA_LOGTO_* / VITE_LOGTO_* assignments to the env
# file consumed by the Makefile. Secrets (m-default secret, access token, user
# password) are never printed or logged.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PODMAN="${PODMAN:-podman}"
COMPOSE_FILE="${LOGTO_COMPOSE_FILE:-$ROOT/deploy/logto/compose.yaml}"
PROJECT="${LOGTO_PROJECT:-agora-logto}"

LOGTO_ENDPOINT="${LOGTO_ENDPOINT:-http://127.0.0.1:3003}"
LOGTO_ADMIN_ENDPOINT="${LOGTO_ADMIN_ENDPOINT:-http://127.0.0.1:3004}"

AGORA_SERVER_ADDR="${AGORA_SERVER_ADDR:-127.0.0.1:8080}"
SERVER_ORIGIN="${AGORA_SERVER_ADDR}"
case "$SERVER_ORIGIN" in
  http://*|https://*) ;;
  *) SERVER_ORIGIN="http://${SERVER_ORIGIN}" ;;
esac
SERVER_ORIGIN="${SERVER_ORIGIN%/}"
AUDIENCE="${AGORA_LOGTO_AUDIENCE:-${SERVER_ORIGIN}/api}"

STATE_DIR="${LOGTO_STATE_DIR:-$ROOT/.agora/logto}"
ENV_FILE="${LOGTO_ENV_FILE:-$STATE_DIR/env}"
PASSWORD_FILE="$STATE_DIR/bootstrap-password"

SPA_NAME="Agora Web"
RESOURCE_NAME="Agora API"
BOOTSTRAP_USERNAME="${AGORA_LOGTO_BOOTSTRAP_USERNAME:-agora_admin}"

# Optional email connector. Fill all four of host/user/password/from to provision
# an SMTP connector and switch the default tenant to email verification-code
# sign-in (username/password stays enabled as a fallback). Left unset, the tenant
# keeps username/password only.
SMTP_HOST="${AGORA_SMTP_HOST:-}"
SMTP_PORT="${AGORA_SMTP_PORT:-465}"
SMTP_SECURE="${AGORA_SMTP_SECURE:-true}"
SMTP_USER="${AGORA_SMTP_USER:-}"
SMTP_PASSWORD="${AGORA_SMTP_PASSWORD:-}"
SMTP_FROM_EMAIL="${AGORA_SMTP_FROM_EMAIL:-}"
SMTP_REPLY_TO="${AGORA_SMTP_REPLY_TO:-}"

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"

fail() { echo "error: $*" >&2; exit 1; }

# --- 1. m-default secret (never echoed) ---
# LOGTO_POSTGRES_CONTAINER lets `podman/docker run` stacks skip compose.
PG_CONTAINER="${LOGTO_POSTGRES_CONTAINER:-}"
if [ -z "$PG_CONTAINER" ]; then
  PG_CONTAINER="$("$PODMAN" compose -f "$COMPOSE_FILE" --project-name "$PROJECT" ps -q postgres 2>/dev/null | head -1)"
fi
[ -n "$PG_CONTAINER" ] || fail "Logto postgres container is not running (run scripts/logto-up.sh or scripts/agora-up.sh first)"
SECRET="$("$PODMAN" exec "$PG_CONTAINER" psql -U postgres -d logto -A -t \
  -c "select secret from applications where id='m-default';" 2>/dev/null | tr -d '[:space:]')"
[ -n "$SECRET" ] || fail "could not read m-default secret (is Logto seeded yet?)"

# The admin endpoint is used for the token exchange because m-default is a
# seeded management proxy client stored in the admin tenant. Its token is
# authorized for the default tenant's Management API resource; requests sent to
# the public endpoint are then routed to the default tenant.
DEFAULT_MANAGEMENT_RESOURCE="https://default.logto.app/api"

# --- 2. Management API access tokens ---
TOKEN="$(curl -fsS -X POST "${LOGTO_ADMIN_ENDPOINT}/oidc/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'client_id=m-default' \
  --data-urlencode "client_secret=${SECRET}" \
  --data-urlencode "resource=${DEFAULT_MANAGEMENT_RESOURCE}" \
  --data-urlencode 'scope=all' 2>/dev/null | jq -r '.access_token' || true)"
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || fail "failed to obtain default-tenant Management API access token"
unset SECRET

ADMIN_SECRET="$("$PODMAN" exec "$PG_CONTAINER" psql -U postgres -d logto -A -t \
  -c "select secret from applications where id='m-admin';" 2>/dev/null | tr -d '[:space:]')"
[ -n "$ADMIN_SECRET" ] || fail "could not read m-admin secret (is Logto seeded yet?)"
ADMIN_TOKEN="$(curl -fsS -X POST "${LOGTO_ADMIN_ENDPOINT}/oidc/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'client_id=m-admin' \
  --data-urlencode "client_secret=${ADMIN_SECRET}" \
  --data-urlencode 'resource=https://admin.logto.app/api' \
  --data-urlencode 'scope=all' 2>/dev/null | jq -r '.access_token' || true)"
[ -n "$ADMIN_TOKEN" ] && [ "$ADMIN_TOKEN" != "null" ] || fail "failed to obtain admin-tenant Management API access token"
unset ADMIN_SECRET

# --- helpers ---
REQUEST_CODE=0
REQUEST_BODY=""
request() { # method path [json-body]
  local method="$1" path="$2" body="${3:-}"
  local out
  out="$(mktemp)"
  if [ -n "$body" ]; then
    REQUEST_CODE="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${LOGTO_ENDPOINT}${path}" \
      -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' -d "$body" 2>/dev/null || echo 000)"
  else
    REQUEST_CODE="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${LOGTO_ENDPOINT}${path}" \
      -H "Authorization: Bearer ${TOKEN}" 2>/dev/null || echo 000)"
  fi
  REQUEST_BODY="$(cat "$out")"
  rm -f "$out"
}

request_admin() { # method path [json-body]
  local method="$1" path="$2" body="${3:-}"
  local out
  out="$(mktemp)"
  if [ -n "$body" ]; then
    REQUEST_CODE="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${LOGTO_ADMIN_ENDPOINT}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H 'Content-Type: application/json' -d "$body" 2>/dev/null || echo 000)"
  else
    REQUEST_CODE="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${LOGTO_ADMIN_ENDPOINT}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null || echo 000)"
  fi
  REQUEST_BODY="$(cat "$out")"
  rm -f "$out"
}

ok() { [ "${REQUEST_CODE}" -ge 200 ] && [ "${REQUEST_CODE}" -lt 300 ]; }

# --- 3. Agora API resource (audience) ---
request GET "/api/resources"
RESOURCE_ID="$(printf '%s' "$REQUEST_BODY" | jq -r --arg i "$AUDIENCE" '.[] | select(.indicator==$i) | .id' | head -1)"
if [ -z "$RESOURCE_ID" ]; then
  request POST "/api/resources" "$(jq -n --arg name "$RESOURCE_NAME" --arg indicator "$AUDIENCE" '{name:$name, indicator:$indicator}')"
  ok || fail "create API resource failed (HTTP ${REQUEST_CODE})"
  RESOURCE_ID="$(printf '%s' "$REQUEST_BODY" | jq -r '.id')"
  [ -n "$RESOURCE_ID" ] && [ "$RESOURCE_ID" != "null" ] || fail "API resource response missing id"
  echo "created API resource ${AUDIENCE}"
else
  echo "API resource ${AUDIENCE} already exists"
fi

# --- 4. Agora Web SPA application ---
request GET "/api/applications?types=SPA"
SPA_ID="$(printf '%s' "$REQUEST_BODY" | jq -r --arg n "$SPA_NAME" '.[] | select(.name==$n) | .id' | head -1)"
if [ -z "$SPA_ID" ]; then
  request POST "/api/applications" "$(jq -n --arg name "$SPA_NAME" --arg origin "$SERVER_ORIGIN" \
    '{name:$name, type:"SPA", oidcClientMetadata:{redirectUris:[$origin], postLogoutRedirectUris:[$origin]}}')"
  ok || fail "create SPA application failed (HTTP ${REQUEST_CODE})"
  SPA_ID="$(printf '%s' "$REQUEST_BODY" | jq -r '.id')"
  [ -n "$SPA_ID" ] && [ "$SPA_ID" != "null" ] || fail "SPA application response missing id"
  echo "created SPA application ${SPA_NAME}"
else
  echo "SPA application ${SPA_NAME} already exists"
fi

# --- 5. bootstrap users (idempotent; password saved, never echoed) ---
BOOTSTRAP_PASSWORD="${AGORA_LOGTO_BOOTSTRAP_PASSWORD:-}"
if [ -z "$BOOTSTRAP_PASSWORD" ] && [ -f "$PASSWORD_FILE" ]; then
  BOOTSTRAP_PASSWORD="$(cat "$PASSWORD_FILE")"
fi

# The public/default tenant user signs into the Agora SPA. A separate user in
# the admin tenant is needed for the Logto console at ADMIN_ENDPOINT. They use
# the same locally generated password so the bootstrap instructions stay
# simple, while each user remains scoped to its own tenant.
request GET "/api/users?search=${BOOTSTRAP_USERNAME}"
DEFAULT_USER_ID="$(printf '%s' "$REQUEST_BODY" | jq -r --arg u "$BOOTSTRAP_USERNAME" '.[] | select(.username==$u) | .id' | head -1)"
if [ -z "$DEFAULT_USER_ID" ]; then
  if [ -z "$BOOTSTRAP_PASSWORD" ]; then
    BOOTSTRAP_PASSWORD="$(openssl rand -hex 24)"
  fi
  request POST "/api/users" "$(jq -n --arg u "$BOOTSTRAP_USERNAME" --arg p "$BOOTSTRAP_PASSWORD" '{username:$u, password:$p}')"
  ok || fail "create default-tenant bootstrap user failed (HTTP ${REQUEST_CODE})"
  DEFAULT_USER_ID="$(printf '%s' "$REQUEST_BODY" | jq -r '.id')"
  printf '%s\n' "$BOOTSTRAP_PASSWORD" > "$PASSWORD_FILE"
  chmod 600 "$PASSWORD_FILE"
  echo "created default-tenant bootstrap user ${BOOTSTRAP_USERNAME}"
else
  echo "default-tenant bootstrap user ${BOOTSTRAP_USERNAME} already exists"
fi

request_admin GET "/api/users?search=${BOOTSTRAP_USERNAME}"
ADMIN_USER_ID="$(printf '%s' "$REQUEST_BODY" | jq -r --arg u "$BOOTSTRAP_USERNAME" '.[] | select(.username==$u) | .id' | head -1)"
if [ -z "$ADMIN_USER_ID" ]; then
  if [ -z "$BOOTSTRAP_PASSWORD" ]; then
    BOOTSTRAP_PASSWORD="$(openssl rand -hex 24)"
    printf '%s\n' "$BOOTSTRAP_PASSWORD" > "$PASSWORD_FILE"
    chmod 600 "$PASSWORD_FILE"
  fi
  request_admin POST "/api/users" "$(jq -n --arg u "$BOOTSTRAP_USERNAME" --arg p "$BOOTSTRAP_PASSWORD" '{username:$u, password:$p}')"
  ok || fail "create admin-tenant bootstrap user failed (HTTP ${REQUEST_CODE})"
  ADMIN_USER_ID="$(printf '%s' "$REQUEST_BODY" | jq -r '.id')"
  echo "created admin-tenant bootstrap user ${BOOTSTRAP_USERNAME}"
else
  echo "admin-tenant bootstrap user ${BOOTSTRAP_USERNAME} already exists"
fi

[ -n "$DEFAULT_USER_ID" ] && [ "$DEFAULT_USER_ID" != "null" ] || fail "default-tenant bootstrap user response missing id"
[ -n "$ADMIN_USER_ID" ] && [ "$ADMIN_USER_ID" != "null" ] || fail "admin-tenant bootstrap user response missing id"

# --- 6. grant admin console access (best-effort) ---
request_admin POST "/api/organizations/t-admin/users" "$(jq -n --arg id "$ADMIN_USER_ID" '{userIds:[$id]}')" || true
[ "${REQUEST_CODE}" -lt 300 ] || echo "note: could not add admin bootstrap user to admin organization (HTTP ${REQUEST_CODE})"
request_admin POST "/api/organizations/t-admin/users/roles" "$(jq -n --arg id "$ADMIN_USER_ID" '{userIds:[$id], organizationRoleIds:["admin"]}')" || true
[ "${REQUEST_CODE}" -lt 300 ] || echo "note: could not assign admin organization role (HTTP ${REQUEST_CODE})"

# The OSS admin console runs against the default tenant, so the admin user also
# has to be a member of the "t-default" organization. Without it the console
# fails right after sign-in with "user is not a member of the organization".
request_admin POST "/api/organizations/t-default/users" "$(jq -n --arg id "$ADMIN_USER_ID" '{userIds:[$id]}')" || true
[ "${REQUEST_CODE}" -lt 300 ] || echo "note: could not add admin bootstrap user to t-default organization (HTTP ${REQUEST_CODE})"
request_admin POST "/api/organizations/t-default/users/roles" "$(jq -n --arg id "$ADMIN_USER_ID" '{userIds:[$id], organizationRoleIds:["admin"]}')" || true
[ "${REQUEST_CODE}" -lt 300 ] || echo "note: could not assign t-default organization role (HTTP ${REQUEST_CODE})"

request_admin GET "/api/roles?type=User"
ROLE_IDS="$(printf '%s' "$REQUEST_BODY" | jq -r '.[] | .id' | paste -sd, -)"
if [ -n "$ROLE_IDS" ]; then
  request_admin POST "/api/users/${ADMIN_USER_ID}/roles" "$(jq -n --arg ids "$ROLE_IDS" '{roleIds:($ids|split(","))}')" || true
  [ "${REQUEST_CODE}" -lt 300 ] || echo "note: could not assign admin user roles (HTTP ${REQUEST_CODE})"
fi

# --- 7. close registration and optionally enable email sign-in ---
# Logto flips a tenant's sign-in mode during its interactive first-admin
# registration. Users created through the Management API bypass that flow, so
# set the mode explicitly: sign-in only, self-registration stays closed.
request_admin PATCH "/api/sign-in-exp" '{"signInMode":"SignIn"}'
ok || fail "set admin tenant sign-in mode failed (HTTP ${REQUEST_CODE})"
request PATCH "/api/sign-in-exp" '{"signInMode":"SignIn"}'
ok || fail "set default tenant sign-in mode failed (HTTP ${REQUEST_CODE})"
echo "disabled self-registration on both tenants"

if [ -n "$SMTP_HOST" ] && [ -n "$SMTP_USER" ] && [ -n "$SMTP_PASSWORD" ] && [ -n "$SMTP_FROM_EMAIL" ]; then
  # AGORA_SMTP_SECURE and AGORA_SMTP_PORT have to be JSON literals for jq
  # ("true"/"false" and a number).
  EMAIL_CONFIG="$(jq -nc \
    --arg host "$SMTP_HOST" \
    --argjson port "$SMTP_PORT" \
    --argjson secure "$SMTP_SECURE" \
    --arg user "$SMTP_USER" \
    --arg pass "$SMTP_PASSWORD" \
    --arg from "$SMTP_FROM_EMAIL" \
    --arg reply "$SMTP_REPLY_TO" \
    '{
      host: $host,
      port: $port,
      secure: $secure,
      auth: { type: "login", user: $user, pass: $pass },
      fromEmail: $from,
      templates: [
        { usageType: "SignIn", contentType: "text/plain", subject: "Agora 登录验证码", content: "你的 Agora 登录验证码是 {{code}}，10 分钟内有效。请勿泄露给他人。" },
        { usageType: "Register", contentType: "text/plain", subject: "Agora 注册验证码", content: "你的 Agora 注册验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "ForgotPassword", contentType: "text/plain", subject: "Agora 密码重置验证码", content: "你的 Agora 密码重置验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "OrganizationInvitation", contentType: "text/plain", subject: "Agora 邀请验证码", content: "你的 Agora 邀请验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "Generic", contentType: "text/plain", subject: "Agora 验证码", content: "你的 Agora 验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "UserPermissionValidation", contentType: "text/plain", subject: "Agora 权限校验验证码", content: "你的 Agora 权限校验验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "BindNewIdentifier", contentType: "text/plain", subject: "Agora 绑定邮箱验证码", content: "你的 Agora 绑定新邮箱验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "MfaVerification", contentType: "text/plain", subject: "Agora 两步验证码", content: "你的 Agora 两步验证码是 {{code}}，10 分钟内有效。" },
        { usageType: "BindMfa", contentType: "text/plain", subject: "Agora 两步验证绑定码", content: "你的 Agora 两步验证绑定码是 {{code}}，10 分钟内有效。" }
      ]
    } + (if $reply == "" then {} else { replyTo: $reply } end)')"

  request GET "/api/connectors"
  EMAIL_CONNECTOR_ID="$(printf '%s' "$REQUEST_BODY" | jq -r '.[] | select(.connectorId=="simple-mail-transfer-protocol") | .id' | head -1)"
  if [ -z "$EMAIL_CONNECTOR_ID" ]; then
    request POST "/api/connectors" "$(jq -nc --argjson config "$EMAIL_CONFIG" '{connectorId:"simple-mail-transfer-protocol", config:$config, metadata:{name:{en:"SMTP Email","zh-CN":"SMTP 邮件"}}}')"
    ok || fail "create email connector failed (HTTP ${REQUEST_CODE})"
    echo "created email connector (simple-mail-transfer-protocol)"
  else
    request PATCH "/api/connectors/${EMAIL_CONNECTOR_ID}" "$(jq -nc --argjson config "$EMAIL_CONFIG" '{config:$config}')"
    ok || fail "update email connector failed (HTTP ${REQUEST_CODE})"
    echo "updated email connector ${EMAIL_CONNECTOR_ID}"
  fi

  request PATCH "/api/sign-in-exp" '{"signIn":{"methods":[{"identifier":"email","password":false,"verificationCode":true,"isPasswordPrimary":false},{"identifier":"username","password":true,"verificationCode":false,"isPasswordPrimary":true}]}}'
  ok || fail "enable email sign-in failed (HTTP ${REQUEST_CODE})"
  echo "enabled email verification-code sign-in (username/password kept as fallback)"
else
  echo "note: AGORA_SMTP_* not fully set; skipped email connector (username/password sign-in only)"
fi

# --- 8. write env file for the Makefile ---
cat > "$ENV_FILE" <<EOF
LOGTO_ENDPOINT=${LOGTO_ENDPOINT}
LOGTO_ADMIN_ENDPOINT=${LOGTO_ADMIN_ENDPOINT}
AGORA_LOGTO_ISSUER=${LOGTO_ENDPOINT}/oidc
AGORA_LOGTO_AUDIENCE=${AUDIENCE}
VITE_LOGTO_ENDPOINT=${LOGTO_ENDPOINT}
VITE_LOGTO_APP_ID=${SPA_ID}
VITE_LOGTO_AUDIENCE=${AUDIENCE}
EOF
chmod 600 "$ENV_FILE"

unset TOKEN ADMIN_TOKEN REQUEST_BODY

echo "Logto bootstrap complete: endpoint=${LOGTO_ENDPOINT} admin=${LOGTO_ADMIN_ENDPOINT} audience=${AUDIENCE}"
echo "Logto admin console: ${LOGTO_ADMIN_ENDPOINT}  username=${BOOTSTRAP_USERNAME}  (password in ${PASSWORD_FILE})"
