# Agora

Agora is a local-first console that adds Web/IM observation and input to native Claude Code sessions without replacing the native Claude Code experience.

## Architecture

Run `claude-wrapper` from a terminal. It connects to `agora serve`, which owns the PTY and launches the real Claude Code child process. The terminal still shows the full native Claude TUI; Agora observes the same session JSONL and can write Web/IM input into the same PTY.

```text
claude-wrapper → Unix socket → Agora PTYManager → claude TUI child
                                          ├→ JSONL observer → Web/SSE/IM
                                          └→ raw PTY attach stream
```

JSONL observer 可以提供工具调用和工具结果历史；Web UI 现在也会读取 managed PTY 的只读 screen snapshot，用于显示原生 Claude TUI 的当前画面。Claude Code 实时 permission/approval prompt 仍属于 PTY 中的原生 TUI 状态，Agora 只做观察并提示“审批仍需在原生终端完成”，不会在 Web UI 中提供审批按钮或把普通文本当作审批按键注入。PTY attach 目前只广播实时 raw bytes；Web snapshot 接口提供当前屏幕的只读观察，但不提供 PTY 历史回放。

## Local trial

### 1. Start Agora

```bash
cd /Users/wangtengfei/private_workspace/agora
AGORA_DB="$PWD/.agora.db" go run ./cmd/agora serve
```

The API listens on `http://127.0.0.1:8080` by default.

### 2. Start the Web UI

```bash
cd /Users/wangtengfei/private_workspace/agora/web
npm install       # first time only
npm run dev
```

Open **http://localhost:5173**. Vite proxies `/api` to Agora on `8080`.

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
- The JSONL observer stores the resulting user/assistant/tool events and pushes them to the event stream.

The wrapper is the only session entry point in this architecture. Agora does not import independent external Claude processes.

## Environment isolation

Agora removes inherited Claude/Cursor child-session variables before launching Claude. This is required to avoid Claude displaying `Transcript saving is off` and disabling JSONL persistence.

## Restart behavior

On Agora restart, persisted managed sessions are relaunched with `claude --resume <session-id>`, reattached to a fresh PTY, and observed from the existing JSONL history.

## Tests

```bash
go test ./...
cd web && npm run build
```
