# Claude Code Integration

状态：`claude-wrapper` / managed PTY
最后更新：2026-08-06

## 当前模型

用户通过 `claude-wrapper` 进入 Claude Code。wrapper 不是另一个 Agent，也不替代 Claude Code：它只是 Agora 持有 PTY 的终端渲染端。

```text
claude-wrapper（用户终端）
        │ Unix socket（raw terminal I/O）
        ▼
agora serve / PTYManager（持有 PTY master）
        │
        ▼
claude（Agora 子进程，完整原生 TUI）
        │
        ├── ~/.claude/sessions/<pid>.json（状态与 session id）
        └── ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl（历史）
```

Agora 负责启动、持有和观察 Claude；用户仍然看到并操作原生 Claude Code TUI。Web 消息写入同一个 PTY master；IM 第一阶段发送状态通知并链接回 Web，不直接写入 PTY。

## 环境隔离

启动子进程前，Agora 会剥离 Cursor/Claude 父会话注入的环境变量，包括：

- `CLAUDE_CODE_CHILD_SESSION`
- `CLAUDE_CODE_SESSION_ID`
- `CLAUDE_PID`
- `CLAUDE_CODE_ENTRYPOINT`
- `CLAUDE_AGENT_SDK_VERSION`
- `CLAUDE_CODE_EXECPATH`
- `CURSOR_SPAWNED_BY_EXTENSION_ID`
- `CURSOR_SPAWN_CHAIN`
- `AI_AGENT`
- 其他 Claude 子会话/扩展标记

否则 Claude 会显示 `Transcript saving is off`，并停止写入会话历史。

## 会话与观察

- `POST /api/coordinations/{id}/sessions` 创建一个 managed 会话并在 Agora 持有的 PTY 中启动 Claude；
- `GET /api/sessions/{id}/attach` 返回 wrapper 连接用的 Unix socket；
- `claude-wrapper [session-id]` 连接 socket，raw mode 双向转发键盘和屏幕；
- JSONL observer 轮询会话文件，解析 user/assistant/tool/result 内容，入库并通过 SSE 推送到 Web；
- PTY reader 维护当前 VT screen snapshot，Web 通过只读接口观察 native TUI；
- 归一化的 Session 状态/attention 可以进入独立通知策略，向 IM 发送摘要和 Session deep link；
- `POST /api/sessions/{id}/messages` 将消息写入 PTY master（使用 `\r` 提交 TUI 输入）；
- Agora 重启后对持久化 managed session 使用 `claude --resume <id>` 重新拉起，并继续观察同一 JSONL。

## Human-in-the-Loop 与 PTY 可观测性

当前已确认：Claude 的工具调用记录和 Human-in-the-Loop 审批提示不在同一条数据通道上。

```text
Claude tool_use
      ├── 项目 JSONL ──→ Agora history observer ──→ event/SSE/Web
      └── 原生 TUI ────→ PTY master ──→ serveAttach/raw socket
```

JSONL observer 可以看到 `tool_use`、工具参数以及后续 `tool_result`，所以 Agora 能显示“Claude 准备调用某个工具”。但是 Claude Code 随后显示的 permission/approval prompt 属于原生 TUI 的实时终端画面；在批准或拒绝之前，当前 JSONL 没有可靠的 `approval_required` 记录、审批 ID、选项或 pending 状态可供 Agora 读取。

因此当前行为是：

- 原始终端可以看到并操作 approval prompt；
- Agora 的事件流可以看到工具调用，但看不到“正在等待批准”的语义；
- `waiting` 只是通用会话状态，不能等同于 Human-in-the-Loop；
- `can_approve` 当前为 `false`；
- PTY attach socket 目前是实时 raw bytes 广播，不保存当前终端屏幕，也不向后来连接的 client 回放历史输出；
- Web UI 消费 JSONL/SSE，并通过只读 PTY snapshot 观察 native TUI；
- IM 第一阶段只接收聚合的 Session 状态/attention 通知，并通过 Session deep link 引导用户打开 Web；
- IM 不复制 transcript、PTY raw bytes 或 ANSI/VT 画面；即使通知显示观察到 HIL，审批仍必须在原生终端完成，`can_approve` 保持 `false`。

2026-08-06 对现有 managed PTY 做只读抓取时，连接 socket 成功但收到 0 字节。这不能证明 PTY 没有审批画面，只说明当前 `serveAttach` 没有 screen snapshot/ring buffer：审批画面如果在 client 连接前已经输出，后来连接者无法补读。

### 后续 PTY 层改造方向

下一阶段先做只读观测，不自动发送批准/拒绝按键：

1. 在 PTY reader 中为每个 session 保存最近 raw output，并维护当前终端屏幕快照；
2. 增加只读的 terminal snapshot/observation 接口，供调试和 Web 消费；
3. 采集真实 approval prompt 的文本和 ANSI/VT100 控制序列；
4. 在确认屏幕格式后，再增加 approval detector，产出结构化 `approval_required`/`approval_resolved` 事件；
5. 最后才设计安全的 approve/reject API，避免让 Web 端直接注入任意 PTY 字节。

JSONL 仍作为工具名称、参数和历史结果的辅助数据源；PTY/虚拟终端状态负责实时判断当前是否停在 Human-in-the-Loop 界面。除非另有明确授权，探测和第一阶段改造不得启动新的真实 Claude 会话或自动操作现有审批界面。

## 能力矩阵

| 能力 | 状态 | 说明 |
|---|---|---|
| start | 可用 | Agora/`claude-wrapper` 创建并持有 PTY 子进程 |
| native TUI | 可用 | wrapper 渲染真实 Claude Code TUI |
| send input | 可用 | Web 写入同一个 PTY master；第一阶段 IM 只发通知，不直接输入 |
| observe | 可用 | observer 读取 JSONL |
| stream | 可用 | JSONL observer → SSE |
| resume | 可用 | Agora 重启后 `--resume <id>` |
| attach | 可用 | `claude-wrapper`/`agora attach` 经 Unix socket |
| discover/import | 关闭 | wrapper 是唯一入口，不支持独立外部会话导入 |
| approve | 关闭 | 审批 UI 尚未接入 |

## 验证结论

2026-08-06 实测：

1. `agora pty claude` 在剥离子会话环境后写入 `sessions/*.json` 与项目 JSONL；
2. PTY socket 双向中继成立；
3. Web API 写入 PTY 后，原生 TUI 收到消息，JSONL observer 摄取回复；
4. Agora 重启后 `--resume` 恢复上下文，继续写入同一会话。
