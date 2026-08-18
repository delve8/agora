# Agora 架构（产品化一期）

> 状态：Design / 下一阶段实现目标
>
> 本文描述产品化一期的 Server / Daemon / Wrapper 三角色架构、数据边界与断连行为。Web UI、事件规范化模型和通知 adapter 基础已在当前仓库实现；Server–Daemon 拆分、设备配对和远程 relay 是下一阶段实现目标。

## 1. 角色与职责

```text
Web Browser ───────────────┐
                           ▼
                    Agora Server（远程控制面）
              REST + SSE + Daemon WebSocket
              控制面状态（设备/配对/路由）
              notification policy + IM adapters
                           ▲
                           │ outbound 长连接（自动重连）
                    Agora Daemon（用户电脑）
              PTY Manager + JSONL Observer
              managed-session registry + outbox
                           ▲
                           │ Unix socket / loopback 控制
                    Agora Wrapper（包装原 Agent 启动命令）
                           │
                    Claude Code 原生 TUI
```

### Server

- 提供 Web REST + SSE；
- 提供 Daemon WebSocket 接入；
- 保存设备、配对状态、在线路由和必要配置元数据；
- 将 Web 请求路由给已配对 Daemon；
- 基于 Daemon 实时上报的规范化状态/摘要向 IM webhook 投递；
- **不持久化业务数据**：transcript、Event、Message、PTY snapshot、通知正文均不落库。

### Daemon

- 运行在用户电脑；
- 持有 PTY，启动/恢复 Claude Code；
- 观察被 Agora 纳管 Session 的 JSONL；
- 维护终端 snapshot 并按需响应；
- 通过出站连接自动连接 Server；
- 断线继续运行本地 Agent，在有限 outbox 内缓存关键状态。

### Wrapper

- 本地轻量入口，包装原有 Claude 启动命令；
- 通过 Daemon 的 Unix socket 或 loopback 控制 API 创建/选择 Session 并 attach PTY；
- 不再直接依赖远程 Server 地址。

## 2. 数据边界

### Server 数据

只保存或暂存在内存：

- `daemon_id`、设备名称、在线/离线状态、最后心跳时间；
- pairing code 的哈希、device credential 的哈希/撤销状态；
- `session_id -> daemon_id` 当前路由；
- WebSocket 连接和 request correlation；
- 可选的 webhook target 配置元数据（URL 不进入普通日志，必要时加密存储）；
- Server 公开 Web base URL 等基础配置。

不保存：

- Claude transcript、原始 JSONL、完整规范化 Event；
- Message 内容；
- PTY raw bytes 或终端 snapshot；
- 完整 tool input/output；
- IM 通知正文和投递历史。

### Daemon 数据

只保存纳管 Session 的运行态：

- `agora_session_id -> claude_session_id` 映射；
- workspace、PID、PTY/socket 映射；
- 对应 JSONL history path；
- observer byte cursor / line / last external ID；
- 有界断线 outbox；
- pending command 和重连状态。

完整历史继续由 Claude 原始 JSONL 作为本地事实源。Daemon 响应 Web 的 history 请求时按需读取对应纳管 Session 的 JSONL，再通过 Server 流式转发，不复制完整 Event 表。

### 纳管范围

Daemon 不扫描 `~/.claude/projects/` 或 `~/.claude/sessions/` 下所有会话。只有以下来源进入 Agora 管理范围：

- `agora wrapper` 启动的 Session；
- Web 通过 Server 请求 Daemon 创建的 Session；
- 用户显式执行注册命令的 Session（如一期需要）。

## 3. 隐私边界

一期采用“Server 可转发、不可持久化业务内容”：

```text
Claude → Daemon → Server（短暂处理明文） → Web / IM
```

- Server 可以在转发、SSE fan-out 和 IM payload 构造时短暂看到规范化状态、短摘要和必要消息内容；
- 不把请求体或事件内容写入业务数据库；
- 不默认记录完整请求日志；
- 不在 IM 通知中发送原始 transcript、PTY 内容或完整工具参数。

未来如需 Server 完全看不到业务内容，再单独设计端到端加密，不在一期混入。

## 4. 断连与恢复语义

- Daemon 断线：继续运行本地 Agent；关键状态与错误进入有限 outbox；重连后按游标或稳定事件 ID 补发。
- Server 重启：不承诺恢复业务历史；只恢复控制面并等待 Daemon 重新注册。
- outbox 超上限：优先保留状态变化和错误；普通高频 assistant 文本可丢弃，但 Web 必须展示“离线期间存在未完整同步内容”。

## 5. 流量路径

```text
claude-wrapper / Web UI
        │  Unix socket / REST（经 Server）
        ▼
Agora Daemon PTY Manager（持有 master）
        │  键盘或消息写入 PTY master
        ▼
原生 Claude Code TUI 子进程
        │  写 sessions metadata + project JSONL
        ▼
JSONL Observer → Daemon → Server 内存 SSE/通知 → Web
                         └→ 归一化状态/attention → Notification policy → IM 摘要 + Web deep link
```

## 6. 技术选型

- 后端：Go；
- 前端：TypeScript + React + Ant Design 5；
- Server ↔ Daemon：出站 WebSocket，设备配对、心跳、自动重连、resync、有限 outbox；
- Daemon ↔ Wrapper：Unix domain socket / loopback 控制面；
- Server 持久化：当前实现中的控制面和 session metadata；不保存 Daemon 的完整业务历史；
- Daemon identity：首次运行生成 UUID 并写入 `~/.agora/config.json`；该配置文件只保存设备身份，不保存 Session runtime、PID、PTY、cursor 或 history；
- Daemon 启动时从 `~/.claude/projects/*/*.jsonl` 重新建立内存中的 history index；这些条目可 resume，但不恢复 PID、PTY、cursor 或 observer；
- Claude 历史事实源：用户电脑原有项目 JSONL；
- 前端实时更新：WebSocket（Daemon）+ SSE（Web）。

## 7. 一期不包含

- IM 入站消息写入 Agent；
- IM 审批按钮或普通文本解释为审批按键；
- 多 Agent 自动交汇、自动转发、Thread/Topic；
- 多租户、复杂 RBAC、可靠分布式消息队列；
- Server 完全盲转发的端到端加密；
- OpenCode、Codex、PI 等其他 Agent adapter。
