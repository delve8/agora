SHELL := /bin/sh

GO ?= go
NPM ?= npm
WEB_DIR ?= web
BIN_DIR ?= bin
AGORA_BIN ?= $(BIN_DIR)/agora
AGORA_ADDR ?= 127.0.0.1:8080
AGORA_DB ?= $(CURDIR)/.agora/agora.db
AGORA_SERVER_ADDR ?= $(AGORA_ADDR)
AGORA_SERVER_DB ?= $(CURDIR)/.agora/server.db
AGORA_WEB_DIR ?= $(WEB_DIR)/dist
AGORA_SERVER_URL ?= http://$(AGORA_SERVER_ADDR)
AGORA_WEB_PROXY ?= $(AGORA_SERVER_URL)
AGORA_CLAUDE_BINARY ?= $(CLAUDE_BINARY)
AGORA_PI_BINARY ?= $(if $(PI_BINARY),$(PI_BINARY),pi)
AGORA_PI_PROVIDER ?= $(if $(PI_PROVIDER),$(PI_PROVIDER),anthropic)
AGORA_PI_MODEL ?= $(PI_MODEL)
AGORA_PI_SESSION_DIR ?= $(PI_SESSION_DIR)
AGORA_DEVICE_CREDENTIAL ?=
AGORA_DEVICE_CREDENTIAL_PATH ?=
AGORA_DAEMON_SOCKET ?=
AGORA_WRAPPER_DIR ?= $(HOME)/.local/bin

# Logto dev stack (Podman). `make server` uses these by default; set
# AGORA_AUTH_MODE=local to skip Logto, or set AGORA_LOGTO_ISSUER/
# AGORA_LOGTO_AUDIENCE + VITE_LOGTO_* to use an external Logto instead.
AGORA_AUTH_MODE ?=
AGORA_LOGTO_ISSUER ?=
AGORA_LOGTO_AUDIENCE ?=
AGORA_LOGTO_PROVISIONING ?=
VITE_LOGTO_ENDPOINT ?=
VITE_LOGTO_APP_ID ?=
VITE_LOGTO_AUDIENCE ?=
AGORA_LOGTO_BOOTSTRAP_USERNAME ?=
AGORA_LOGTO_BOOTSTRAP_PASSWORD ?=
# Optional email sign-in (all four of host/user/password/from enable it).
AGORA_SMTP_HOST ?=
AGORA_SMTP_PORT ?= 465
AGORA_SMTP_SECURE ?= true
AGORA_SMTP_USER ?=
AGORA_SMTP_PASSWORD ?=
AGORA_SMTP_FROM_EMAIL ?=
AGORA_SMTP_REPLY_TO ?=
# Set to true to let anyone sign up with a verified email address.
AGORA_ALLOW_REGISTRATION ?=
PODMAN ?= podman
LOGTO_COMPOSE_FILE ?= deploy/logto/compose.yaml
LOGTO_PROJECT ?= agora-logto
LOGTO_ENDPOINT ?= http://127.0.0.1:3003
LOGTO_ADMIN_ENDPOINT ?= http://127.0.0.1:3004
LOGTO_STATE_DIR ?= $(CURDIR)/.agora/logto
LOGTO_ENV_FILE ?= $(LOGTO_STATE_DIR)/env
LOGTO_IMAGE ?= svhd/logto:latest
LOGTO_POSTGRES_IMAGE ?= postgres:17-alpine
LOGTO_POSTGRES_PASSWORD ?= agora-logto-dev

.PHONY: all build build-go build-web web-install test e2e vet fmt clean \
        local server server-local daemon web start \
        logto-up logto-bootstrap logto-down logto-purge logto-status \
        agora-up agora-status agora-down agora-purge install-wrapper

all: build

build: build-go build-web

build-go:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(AGORA_BIN)" ./cmd/agora

web-install:
	@if [ ! -d "$(WEB_DIR)/node_modules" ]; then \
		$(NPM) --prefix "$(WEB_DIR)" ci; \
	fi

build-web: web-install
	$(NPM) --prefix "$(WEB_DIR)" run build

test:
	$(GO) test ./...

e2e:
	$(GO) test -timeout 60s ./e2e/...
vet:
	$(GO) vet ./...
	GOOS=darwin go vet ./...

fmt:
	$(GO) fmt ./...

local: build-go build-web
	@mkdir -p "$$(dirname "$(AGORA_DB)")"
	AGORA_ADDR="$(AGORA_ADDR)" AGORA_DB="$(AGORA_DB)" AGORA_WEB_DIR="$(AGORA_WEB_DIR)" \
	 AGORA_CLAUDE_BINARY="$(AGORA_CLAUDE_BINARY)" AGORA_PI_BINARY="$(AGORA_PI_BINARY)" AGORA_PI_PROVIDER="$(AGORA_PI_PROVIDER)" \
	 AGORA_PI_MODEL="$(AGORA_PI_MODEL)" AGORA_PI_SESSION_DIR="$(AGORA_PI_SESSION_DIR)" \
	 "$(AGORA_BIN)" serve

server: build-go
	@AGORA_AUTH_MODE="$(AGORA_AUTH_MODE)" \
	 AGORA_LOGTO_ISSUER="$(AGORA_LOGTO_ISSUER)" \
	 AGORA_LOGTO_AUDIENCE="$(AGORA_LOGTO_AUDIENCE)" \
	 AGORA_LOGTO_PROVISIONING="$(AGORA_LOGTO_PROVISIONING)" \
	 VITE_LOGTO_ENDPOINT="$(VITE_LOGTO_ENDPOINT)" VITE_LOGTO_APP_ID="$(VITE_LOGTO_APP_ID)" \
	 VITE_LOGTO_AUDIENCE="$(VITE_LOGTO_AUDIENCE)" \
	 AGORA_LOGTO_BOOTSTRAP_USERNAME="$(AGORA_LOGTO_BOOTSTRAP_USERNAME)" \
	 AGORA_LOGTO_BOOTSTRAP_PASSWORD="$(AGORA_LOGTO_BOOTSTRAP_PASSWORD)" \
	 AGORA_SMTP_HOST="$(AGORA_SMTP_HOST)" AGORA_SMTP_PORT="$(AGORA_SMTP_PORT)" \
	 AGORA_SMTP_SECURE="$(AGORA_SMTP_SECURE)" AGORA_SMTP_USER="$(AGORA_SMTP_USER)" \
	 AGORA_SMTP_PASSWORD="$(AGORA_SMTP_PASSWORD)" AGORA_SMTP_FROM_EMAIL="$(AGORA_SMTP_FROM_EMAIL)" \
	 AGORA_SMTP_REPLY_TO="$(AGORA_SMTP_REPLY_TO)" \
	 AGORA_ALLOW_REGISTRATION="$(AGORA_ALLOW_REGISTRATION)" \
	 AGORA_SERVER_ADDR="$(AGORA_SERVER_ADDR)" \
	 AGORA_SERVER_DB="$(AGORA_SERVER_DB)" \
	 AGORA_WEB_DIR="$(AGORA_WEB_DIR)" \
	 WEB_DIR="$(WEB_DIR)" NPM="$(NPM)" AGORA_BIN="$(AGORA_BIN)" \
	 PODMAN="$(PODMAN)" LOGTO_COMPOSE_FILE="$(LOGTO_COMPOSE_FILE)" LOGTO_PROJECT="$(LOGTO_PROJECT)" \
	 LOGTO_ENDPOINT="$(LOGTO_ENDPOINT)" LOGTO_ADMIN_ENDPOINT="$(LOGTO_ADMIN_ENDPOINT)" \
	 LOGTO_IMAGE="$(LOGTO_IMAGE)" LOGTO_POSTGRES_IMAGE="$(LOGTO_POSTGRES_IMAGE)" \
	 LOGTO_POSTGRES_PASSWORD="$(LOGTO_POSTGRES_PASSWORD)" \
	 LOGTO_STATE_DIR="$(LOGTO_STATE_DIR)" LOGTO_ENV_FILE="$(LOGTO_ENV_FILE)" \
	 ./scripts/agora-server.sh

server-local:
	@$(MAKE) server AGORA_AUTH_MODE=local

daemon: build-go
	AGORA_DAEMON_ID="$(AGORA_DAEMON_ID)" AGORA_CONFIG_PATH="$(AGORA_CONFIG_PATH)" AGORA_SERVER_URL="$(AGORA_SERVER_URL)" \
	 AGORA_CLAUDE_BINARY="$(AGORA_CLAUDE_BINARY)" AGORA_DEVICE_CREDENTIAL="$(AGORA_DEVICE_CREDENTIAL)" AGORA_DEVICE_CREDENTIAL_PATH="$(AGORA_DEVICE_CREDENTIAL_PATH)" \
	 AGORA_DAEMON_SOCKET="$(AGORA_DAEMON_SOCKET)" AGORA_PI_BINARY="$(AGORA_PI_BINARY)" AGORA_PI_PROVIDER="$(AGORA_PI_PROVIDER)" \
	 AGORA_PI_MODEL="$(AGORA_PI_MODEL)" AGORA_PI_SESSION_DIR="$(AGORA_PI_SESSION_DIR)" \
	 "$(AGORA_BIN)" daemon

web: web-install
	@set -a; if [ -f "$(LOGTO_ENV_FILE)" ]; then . "$(LOGTO_ENV_FILE)"; fi; set +a; \
	 AGORA_WEB_PROXY="$(AGORA_WEB_PROXY)" $(NPM) --prefix "$(WEB_DIR)" run dev -- --host 127.0.0.1

start: build-go
	AGORA_DAEMON_ID="$(AGORA_DAEMON_ID)" AGORA_CONFIG_PATH="$(AGORA_CONFIG_PATH)" AGORA_SERVER_URL="$(AGORA_SERVER_URL)" \
	 AGORA_CLAUDE_BINARY="$(AGORA_CLAUDE_BINARY)" AGORA_DEVICE_CREDENTIAL="$(AGORA_DEVICE_CREDENTIAL)" AGORA_DEVICE_CREDENTIAL_PATH="$(AGORA_DEVICE_CREDENTIAL_PATH)" \
	 AGORA_DAEMON_SOCKET="$(AGORA_DAEMON_SOCKET)" AGORA_PI_BINARY="$(AGORA_PI_BINARY)" AGORA_PI_PROVIDER="$(AGORA_PI_PROVIDER)" \
	 AGORA_PI_MODEL="$(AGORA_PI_MODEL)" AGORA_PI_SESSION_DIR="$(AGORA_PI_SESSION_DIR)" \
	 "$(AGORA_BIN)" daemon

logto-up:
	@LOGTO_COMPOSE_FILE="$(LOGTO_COMPOSE_FILE)" LOGTO_PROJECT="$(LOGTO_PROJECT)" \
	 LOGTO_ENDPOINT="$(LOGTO_ENDPOINT)" LOGTO_IMAGE="$(LOGTO_IMAGE)" \
	 LOGTO_POSTGRES_IMAGE="$(LOGTO_POSTGRES_IMAGE)" LOGTO_POSTGRES_PASSWORD="$(LOGTO_POSTGRES_PASSWORD)" \
	 PODMAN="$(PODMAN)" ./scripts/logto-up.sh

logto-bootstrap:
	@LOGTO_COMPOSE_FILE="$(LOGTO_COMPOSE_FILE)" LOGTO_PROJECT="$(LOGTO_PROJECT)" \
	 LOGTO_ENDPOINT="$(LOGTO_ENDPOINT)" LOGTO_ADMIN_ENDPOINT="$(LOGTO_ADMIN_ENDPOINT)" \
	 AGORA_SERVER_ADDR="$(AGORA_SERVER_ADDR)" AGORA_LOGTO_AUDIENCE="$(AGORA_LOGTO_AUDIENCE)" \
	 LOGTO_STATE_DIR="$(LOGTO_STATE_DIR)" LOGTO_ENV_FILE="$(LOGTO_ENV_FILE)" PODMAN="$(PODMAN)" \
	 ./scripts/logto-bootstrap.sh

logto-down:
	$(PODMAN) compose -f "$(LOGTO_COMPOSE_FILE)" --project-name "$(LOGTO_PROJECT)" down

logto-purge:
	$(PODMAN) compose -f "$(LOGTO_COMPOSE_FILE)" --project-name "$(LOGTO_PROJECT)" down -v

logto-status:
	$(PODMAN) compose -f "$(LOGTO_COMPOSE_FILE)" --project-name "$(LOGTO_PROJECT)" ps

# Container-only stack (docker/podman run, no compose). Replaces Agora Server,
# Logto, and Logto's Postgres while keeping named volumes.
agora-up:
	ENGINE="$(PODMAN)" ./scripts/agora-up.sh up

agora-status:
	ENGINE="$(PODMAN)" ./scripts/agora-up.sh status

agora-down:
	ENGINE="$(PODMAN)" ./scripts/agora-up.sh down

agora-purge:
	ENGINE="$(PODMAN)" ./scripts/agora-up.sh purge

install-wrapper: build-go
	@mkdir -p "$(AGORA_WRAPPER_DIR)"
	@agora_path="$(AGORA_BIN)"; case "$$agora_path" in /*) ;; *) agora_path="$(CURDIR)/$$agora_path" ;; esac; \
		ln -sf "$$agora_path" "$(AGORA_WRAPPER_DIR)/agora"
	@ln -sf "$(CURDIR)/scripts/agora-wrapper.sh" "$(AGORA_WRAPPER_DIR)/pi"
	@ln -sf "$(CURDIR)/scripts/agora-wrapper.sh" "$(AGORA_WRAPPER_DIR)/claude"
	@echo "installed Agora wrappers: $(AGORA_WRAPPER_DIR)/pi and $(AGORA_WRAPPER_DIR)/claude"
	@echo "Agora CLI linked at $(AGORA_WRAPPER_DIR)/agora"
	@echo "ensure $(AGORA_WRAPPER_DIR) is before the real agent binaries and AGORA_PI_BINARY points to the real pi binary"

clean:
	rm -rf "$(BIN_DIR)" "$(WEB_DIR)/dist"
