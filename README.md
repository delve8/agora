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

The Server builds and serves the Web UI from `web/dist`. It listens on `http://127.0.0.1:8080` by default. Open **http://127.0.0.1:8080** in a browser.

For frontend development with Vite instead, run `make web` in another terminal; Vite proxies `/api` to `AGORA_WEB_PROXY` (default `http://127.0.0.1:8080`).

### 3. Start native Claude through the wrapper

Build Agora first if needed:

```bash
cd /Users/wangtengfei/private_workspace/agora
go build -o ./agora ./cmd/agora
```

Then, with Agora still running:

```bash
./agora wrapper
```

The wrapper uses the current directory as the Claude workspace, creates a managed session in Agora, and attaches your terminal to its native TUI. Your keyboard and screen behave like normal Claude Code; Agora owns the underlying child process.

To attach to an existing managed session:

```bash
./agora wrapper sess-<session-id>
# or
./agora attach sess-<session-id>
```

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
make server       # build and start the standalone Server + Web UI
make daemon       # start the local Daemon
make web          # start the Vite development server only
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
