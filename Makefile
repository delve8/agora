SHELL := /bin/sh

GO ?= go
NPM ?= npm
WEB_DIR ?= web
BIN_DIR ?= bin
AGORA_BIN ?= $(BIN_DIR)/agora
PROXY_BIN ?= $(BIN_DIR)/agora-claude-proxy

AGORA_ADDR ?= 127.0.0.1:8080
AGORA_DB ?= $(CURDIR)/.agora/agora.db
AGORA_SERVER_ADDR ?= $(AGORA_ADDR)
AGORA_SERVER_DB ?= $(CURDIR)/.agora/server.db
AGORA_WEB_DIR ?= $(WEB_DIR)/dist
AGORA_SERVER_URL ?= http://$(AGORA_SERVER_ADDR)
AGORA_WEB_PROXY ?= $(AGORA_SERVER_URL)

.PHONY: all build build-go build-web web-install test e2e vet fmt clean \
        local server daemon web start

all: build

build: build-go build-web

build-go:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(AGORA_BIN)" ./cmd/agora
	$(GO) build -o "$(PROXY_BIN)" ./cmd/agora-claude-proxy

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

fmt:
	$(GO) fmt ./...

local: build-go build-web
	@mkdir -p "$$(dirname "$(AGORA_DB)")"
	AGORA_ADDR="$(AGORA_ADDR)" AGORA_DB="$(AGORA_DB)" AGORA_WEB_DIR="$(AGORA_WEB_DIR)" "$(AGORA_BIN)" serve

server: build-go build-web
	@mkdir -p "$$(dirname "$(AGORA_SERVER_DB)")"
	AGORA_SERVER_ADDR="$(AGORA_SERVER_ADDR)" AGORA_SERVER_DB="$(AGORA_SERVER_DB)" AGORA_WEB_DIR="$(AGORA_WEB_DIR)" "$(AGORA_BIN)" server

daemon: build-go
	AGORA_DAEMON_ID="$(AGORA_DAEMON_ID)" AGORA_CONFIG_PATH="$(AGORA_CONFIG_PATH)" AGORA_SERVER_URL="$(AGORA_SERVER_URL)" "$(AGORA_BIN)" daemon

web: web-install
	AGORA_WEB_PROXY="$(AGORA_WEB_PROXY)" $(NPM) --prefix "$(WEB_DIR)" run dev -- --host 127.0.0.1

start: build-go
	AGORA_DAEMON_ID="$(AGORA_DAEMON_ID)" AGORA_CONFIG_PATH="$(AGORA_CONFIG_PATH)" AGORA_SERVER_URL="$(AGORA_SERVER_URL)" "$(AGORA_BIN)" daemon

clean:
	rm -rf "$(BIN_DIR)" "$(WEB_DIR)/dist"
