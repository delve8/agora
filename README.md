# Agora

Agora is a local-first console that adds Web/IM observation and input to native Claude Code sessions without replacing the native Claude Code experience.

## Architecture

In split deployment, the public `agora server` owns the HTTP API, serves the Web UI, and relays messages to registered Daemons. Each workstation runs `agora daemon`, which owns the local PTYs and Claude Code processes. `agora serve` remains available as an explicit local combined mode.

```text
Browser ──HTTP/SSE──→ Agora Server ──WebSocket──→ Agora Daemon ──PTY──→ Claude Code
                           └── static Web UI
```

Run `claude-wrapper` from a terminal on a Daemon machine. It connects to the local Daemon, which owns the PTY and launches the real Claude Code child process. The Server observes and relays session state to the Web UI.

JSONL observer 可以提供工具调用和工具结果历史；Web UI 现在也会读取 managed PTY 的只读 screen snapshot，用于显示原生 Claude TUI 的当前画面。Claude Code 实时 permission/approval prompt 仍属于 PTY 中的原生 TUI 状态，Agora 只做观察并提示“审批仍需在原生终端完成”，不会在 Web UI 中提供审批按钮或把普通文本当作审批按键注入。PTY attach 目前只广播实时 raw bytes；Web snapshot 接口提供当前屏幕的只读观察，但不提供 PTY 历史回放。

## Local trial

### 1. Start the Server and Web UI

```bash
cd /Users/wangtengfei/private_workspace/agora
make server
```

By default, `make server` starts a local Logto + PostgreSQL stack with Podman,
waits for Logto readiness, and idempotently provisions the Agora API resource,
Web SPA, and a bootstrap user in Logto's default tenant. It then builds the Web
bundle with the generated `VITE_LOGTO_*` settings and starts the Server in
`AGORA_AUTH_MODE=logto`. The Server listens on `http://127.0.0.1:8080` by
default; open that address in a browser.

The generated local state is kept under `.agora/logto/` (ignored by Git):

- `.agora/logto/env` contains the generated endpoint, issuer, audience, and SPA client ID;
- `.agora/logto/bootstrap-password` contains the bootstrap user's password with mode `0600`.

The local Logto admin console is available at `http://127.0.0.1:3004` and the
OIDC endpoint at `http://127.0.0.1:3003`. The default bootstrap username is
`agora_admin`.

Useful lifecycle commands:

```bash
make logto-status                    # show the local containers
make logto-down                      # stop containers, keep the database volume
make logto-purge                     # stop containers and delete the database volume
make server AGORA_AUTH_MODE=local    # skip Logto and use loopback trust-local mode
```

The local stack is for development only. Do not reuse its database, generated
password, or HTTP endpoints for production. For an external Logto deployment,
provide `AGORA_LOGTO_ISSUER`, `AGORA_LOGTO_AUDIENCE`, `VITE_LOGTO_ENDPOINT`, and
`VITE_LOGTO_APP_ID`; when all four are set, `make server` skips the local Podman
stack and uses those values.

For frontend development with Vite instead, run `make web` in another terminal; Vite proxies `/api` to `AGORA_WEB_PROXY` (default `http://127.0.0.1:8080`).

### 3. Start native Claude through the wrapper

Build Agora first if needed:

```bash
cd /Users/wangtengfei/private_workspace/agora
go build -o ./agora ./cmd/agora
```

Then, with Agora still running:

```bash
./agora wrap claude
```

The wrapper uses the current directory as the Claude workspace, creates a managed session in Agora, and attaches your terminal to its native TUI. Your keyboard and screen behave like normal Claude Code; Agora owns the underlying child process.

To attach to an existing managed session:

```bash
./agora wrap claude sess-<session-id>
# or
./agora attach sess-<session-id>

# Unified wrapper entry point
./agora wrap claude
./agora wrap pi
```

The unified wrapper also supports Pi, which is started as an interactive TUI
rather than RPC:

```bash
./agora wrap pi
./agora wrap pi daemon/<daemon-id>/pi://<pi-session-id>
```

To keep using the normal `pi` command while routing interactive sessions
through the local Daemon, install the generic wrapper under the agent name:

```bash
make install-wrapper
export PATH="$HOME/.local/bin:$PATH"
```

The generic CLI entry point is `agora wrap pi` or `agora wrap claude`; the
old `agora pi-wrapper` and `agora wrapper` commands remain compatibility
aliases. The wrapper infers `pi` or `claude` from its symlink name, or accepts
`AGORA_WRAPPER_AGENT=pi|claude`. Set `AGORA_BIN` if the `agora` binary is not
on `PATH`. The Daemon must use the real Pi executable in
`AGORA_PI_BINARY`—not the wrapper symlink—or it would recursively invoke the
wrapper, for example:

```bash
make start AGORA_PI_BINARY="$HOME/.nvm/versions/node/v24.13.0/bin/pi"
```

The wrapper calls `agora wrap <agent>`, creates a new Session in the current
directory and then attaches to the PTY created by the Daemon; an existing Agora
Session ID is attached directly. Positional arguments after a new Pi invocation are sent as initial
prompts. This wrapper intentionally exposes the managed interactive-session
surface, not every Pi administrative option such as `pi auth`, `pi install`,
or `pi --help`; use the real Pi binary for those commands.

For a Logto-protected Server, set `AGORA_ACCESS_TOKEN` (or `AGORA_TOKEN`) for
CLI API requests. The Pi process itself runs on the Daemon machine under a
real PTY with `pi --session <history-file>` when resuming an existing session.

### 4. Use Web and terminal together

- Type normally in the wrapper terminal to use native Claude Code.
- Open the same session in the Web UI and send a message.
- Web input is written into the same PTY and submitted with Enter.
- The JSONL observer parses newly appended records and pushes them directly to Web/SSE/IM without copying transcript events into SQLite.
- Activity history is parsed from Claude's original JSONL whenever it is requested; SQLite stores session metadata, message delivery state, and the observer cursor only.
- Agora also discovers top-level Claude conversations under `~/.claude/projects/*/*.jsonl`. Conversations without a currently active Agora process remain available in the Session selector and can be resumed with Claude's original Session ID.
- Discovered Sessions are ephemeral until resumed and never insert transcript Events into SQLite. When resumed, Agora stores only operational Session metadata and launches `claude --resume <session-id>`; the original JSONL remains authoritative.
- A JSONL that already belongs to a persisted Session is merged into that same entry, preserving its Agora ID and custom display name. Managed, Proxy, and External describe provenance only; terminal, input, attach, and live-stream capabilities depend on whether a process is currently active.
- Nested `subagents/**` JSONLs are intentionally excluded from the Session selector. The discovery index caches unchanged file summaries and parses the complete transcript only when the Session is opened.

The wrapper remains the entry point for creating fresh Sessions that Agora can manage. Existing Claude conversations can be resumed from their compact inactive-session control.

### Productized Daemon installation (planned)

The productized Daemon installation is a workstation-only flow. The Server is the
remote control plane; users do not install or start the Server on the workstation
that runs the Daemon.

The planned first-party entry point is generated by the Server Web UI after the
user clicks **Add device**. The UI provides a short-lived, one-time pairing code
and an HTTPS installer command similar to:

```bash
curl -fsSL https://agora.example.com/download/install.sh \\
  | sh -s -- \\
  --server https://agora.example.com \\
  --pair <one-time-code>
```

The installer will detect the platform and architecture, download and verify the
Daemon package, complete pairing, save the Server URL and device credential, and
configure a user-level background service. Linux will use `systemd --user` and
macOS will use a `launchd` `LaunchAgent`. Windows Daemon installation is not
supported in the first phase.

This installer flow is a productization target and is not yet provided by the
repository. `make server`, `make start`, and `make local` remain source-tree
development/trial commands, not the end-user installation path. The low-level
`agora daemon --pair <code>` entry point may remain useful for development and
automation; `agora daemon install` is not a first-phase end-user command. See
[the deployment boundary in the specification](docs/spec.md#72-daemon-部署与安装边界)
and [the pairing contract](docs/authentication.md#61-安装与配对流程) for details.

## Environment isolation

Agora removes inherited Claude/Cursor child-session variables before launching Claude. This is required to avoid Claude displaying `Transcript saving is off` and disabling JSONL persistence.

## Restart behavior

On Agora restart, persisted managed sessions are relaunched with `claude --resume <session-id>`, reattached to a fresh PTY, and observed from the existing JSONL history.

## Makefile commands

The repository includes a Makefile for building and running the split deployment or an explicit local combined server:

```bash
make build        # build Go binaries and the Web UI
make test         # run Go tests
make e2e          # run Server/Daemon end-to-end tests
make vet          # run go vet
make start        # start only the local Daemon
make local        # start combined Server + Daemon + Web UI
make server       # start the standalone Server + Web UI + local Logto by default
make server-local # explicit trust-local Server mode without Logto
make daemon       # start the local Daemon
make web          # start the Vite development server only
make logto-up     # start the local Logto + PostgreSQL stack
make logto-status # show local Logto container status
make logto-down   # stop local Logto, retaining its database volume
make logto-purge  # stop local Logto and remove its database volume
```

`make server` is the deployable browser entry point: it builds `web/dist` and serves the Web UI together with the API and daemon WebSocket endpoint. The browser should open the Server URL directly.

The Server does not persist Daemon transcript or history data. It keeps live Daemon routes and resync summaries in memory, and those are rebuilt after each Daemon reconnect. Claude's local JSONL files remain the history source.

The Daemon reindexes meaningful Claude JSONL files from `~/.claude/projects/*/*.jsonl` into memory at startup. These appear as stopped, resumable history sessions using IDs such as `daemon/<daemon-uuid>/claude://<claude-session-id>`. No PID, PTY, cursor, transcript copy, or Daemon database is restored; resuming one history entry starts a fresh PTY.

`make start` does not start a Server or Vite. It starts only the local Daemon and expects `AGORA_SERVER_URL` to point to an already-running Server. On the first run, Agora generates a UUID Daemon identity and stores it in `~/.agora/config.json` with restrictive permissions; later starts reuse it. Set `AGORA_CONFIG_PATH` to choose another config file, or set `AGORA_DAEMON_ID` for an explicit test/development override. The Daemon keeps runtime state in memory only; after a Daemon restart, its old PID/PTY/observer state is gone and a later resume starts a new PTY from the canonical agent URI. The Server is the only SQLite owner.

```bash
make server
make start
```

For a separately deployed Server, run the Daemon locally with:

```bash
make start AGORA_SERVER_URL=https://agora.example.com AGORA_DAEMON_ID=workstation-1
```

Use `make web AGORA_WEB_PROXY=https://agora.example.com` only for local frontend development against an existing Server. Use `make local` only when the combined local `agora serve` mode is desired. The static directory can be overridden with `AGORA_WEB_DIR`.

## Tests

```bash
go test ./...
cd web && npm run build
```
