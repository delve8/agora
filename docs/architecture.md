# Agora 架构（产品化一期）

> 状态：Design / 下一阶段实现目标
>
> 本文描述产品化一期的 Server / Daemon / Agent Runtime 架构、数据边界与断连行为。Web UI、事件规范化模型和通知 adapter 基础已在当前仓库实现；Server–Daemon 拆分、设备配对、远程 relay 与按需历史仍是下一阶段实现目标。Claude Code 的 PTY 是当前 provider 实现，Pi 的 stdio RPC/JSONL 是下一接入实现；二者都不是 Server 协议的前提。

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
              provider registry + Agent Runtime
              live observer + history reader + outbox
                           ▲
                           │ PTY / stdio RPC / HTTP / ACP
                    外部 Agent Session
```

### Server

- 提供 Web REST + SSE；
- 提供 Daemon WebSocket 接入；
- 保存用户身份映射、设备归属、配对状态、在线路由和必要配置元数据；
- 将 Web 请求路由给已配对 Daemon；
- 基于 Daemon 实时上报的规范化状态/摘要向 IM webhook 投递；
- 认证分层：Logto/OIDC（或其他受信身份服务）证明 Web 用户是谁，Server 将稳定 `provider + subject` 映射为内部 `user_id`；Daemon 使用独立的 per-device credential 证明设备，不在协议中传递 Web 用户身份；Server 依据 `device_id -> user_id` 和 `session_id -> daemon_id` 执行 Session 授权；
- trust-local 模式是本地默认：Server 只监听 `127.0.0.1`，自动使用 `local` 用户，不要求 Web 登录或 Daemon credential；若配置非 loopback 地址，Server 必须拒绝启动。远程/跨设备部署使用显式认证模式和 TLS；
- IM 通知链接可以兑换为指定 Session 的短期 `read_observation` capability，但该 capability 只进入 history、SSE 和只读 PTY snapshot 路径，不进入 input、stop、resume、attach 或设备管理路径；
- **不持久化业务数据**：transcript、Event、Message、PTY snapshot、通知正文均不落库。

### Daemon

- 运行在用户电脑；
- 通过 provider registry 选择 Agent Runtime，启动/恢复目标 Agent；
- 使用对应的 control transport、live observer 和 history reader；
- 维护可选的 terminal snapshot/attach（当前主要由 Claude PTY 使用）；
- 通过出站连接自动连接 Server；
- 断线继续运行本地 Agent，在有限 outbox 内缓存关键状态。

### Wrapper

- 本地轻量入口，连接或启动某个 provider 的 Agent Runtime；
- Claude Code 可以通过 Unix socket attach PTY；Pi 等非终端 Agent 不要求 wrapper 提供 raw terminal；
- 不把 provider 原始协议直接暴露给远程 Server。

## 2. 数据边界

### Server 数据

只保存或暂存在内存（控制面数据，Server 是唯一持久化所有者；SQLite 或等价存储）：

- `users`：归一化用户身份（`id`、`display_name`、`email`、`status`）；
- `user_identities`：`(provider, subject)` 到 `user_id` 的稳定映射，`unique(provider, subject)`；
- `devices`：`device_id -> user_id` 归属关系、device credential 哈希、撤销状态、最后心跳；
- `pair_codes`：一次性 pairing code 的哈希、签发用户、过期与消费状态；
- `link_grants`：IM 通知链接 token 哈希、绑定 `session_id`、`scope`、签发用户、过期与撤销状态；
- `web_auth_sessions`：BFF/session-cookie 演进下的 HttpOnly Web 会话；
- `session_id -> daemon_id` 当前路由；
- WebSocket 连接和 request correlation；
- 可选的 webhook target 配置元数据（URL 不进入普通日志，必要时加密存储）；
- Server 公开 Web base URL 等基础配置。

认证与授权运行边界：

- trust-local（`AGORA_AUTH_MODE=local`，默认）：强制监听 `127.0.0.1`，所有请求视为 `local` 用户，Daemon 无需 credential；配置非 loopback 地址则拒绝启动。
- auth（`AGORA_AUTH_MODE=logto`）：Logto 授权码 + PKCE 登录，Daemon 必须配对；非 loopback 部署必须 TLS。
- 每个 Session 的读取或控制请求都校验 `session.daemon_id -> devices.user_id == principal.user_id`。

不保存：

- Claude transcript、原始 JSONL、完整规范化 Event；
- Message 内容；
- PTY raw bytes 或终端 snapshot；
- 完整 tool input/output；
- IM 通知正文和投递历史。

### Daemon 数据

只保存纳管 Session 的运行态：

- `agora_session_id -> agent + agent_session_id` 映射；
- workspace、provider、PID、transport 映射；
- provider-owned history locator；
- observer byte cursor / line / last external ID，或 provider-specific cursor；
- 有界断线 outbox；
- pending command 和重连状态。

完整历史继续由 Agent provider 的本地或远端事实源提供。Daemon 响应 Web 的 history 请求时按需读取对应 provider 的 history，再通过 Server 流式转发，不复制完整 Event 表。

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
claude-wrapper / Web UI / Pi RPC driver
        │  provider-specific control transport
        ▼
外部 Agent Session
        │  live events + provider-owned history
        ▼
Observation / History adapters → Daemon → Server 内存 SSE/通知 → Web
                              └→ 归一化状态/attention → Notification policy → IM 摘要 + Web deep link
```

认证模式下的请求路径：

```text
Web principal（Logto 登录或 local 用户）
        │  Server 校验身份
        ▼
Server 校验 session ownership
        │  session.daemon_id → device.user_id == principal.user_id
        ▼
Server 向目标 Daemon 发起 relay
        │  device credential（Web 身份不进入 Daemon 协议）
        ▼
Daemon PTY Manager → Claude Code
```

通知链接（`read_observation` capability）只进入只读路径：metadata / history / SSE / 只读 PTY snapshot，不进入 input、stop、resume、attach 或设备管理路径。

## 6. 技术选型

- 后端：Go；
- 前端：TypeScript + React + Ant Design 5；
- Server ↔ Daemon：出站 WebSocket，设备配对、心跳、自动重连、resync、有限 outbox；
- Web 认证：Logto OIDC（授权码 + PKCE）为推荐身份服务；`TokenValidator` / `UserLookup` 保持接口抽象，可替换为其他 OIDC provider；
- 运行模式：`AGORA_AUTH_MODE=local`（默认 trust-local，强制 loopback）或 `AGORA_AUTH_MODE=logto`（显式认证 + TLS）；
- 设备归属：已认证用户签发一次性 pairing code，Daemon 换取 per-device credential，归属在配对时决定；
- Daemon ↔ Agent Runtime/Wrapper：Unix domain socket / loopback 控制面；Claude wrapper 使用 PTY attach，Pi 由 Daemon 内 stdio RPC driver 控制；
- Server 持久化：当前实现中的控制面和 session metadata；不保存 Daemon 的完整业务历史；
- Daemon identity：首次运行生成 UUID 并写入 `~/.agora/config.json`；该配置文件只保存设备身份，不保存 Session runtime、PID、transport、cursor 或 history；
- Daemon 启动时按 provider 的 HistoryCatalog 重新建立内存中的 history index；Claude 使用 `~/.claude/projects/*/*.jsonl`，Pi 使用 `~/.pi/agent/sessions`；这些条目可 resume，但不恢复 PID、transport、cursor 或 observer；
- Claude 历史事实源：用户电脑原有项目 JSONL；Pi 历史事实源：`~/.pi/agent/sessions` 下的 session JSONL；OpenCode 未来可使用 SQLite/export；
- 前端实时更新：WebSocket（Daemon）+ SSE（Web）；

## 7. 一期不包含

- IM 入站消息写入 Agent；
- IM 审批按钮或普通文本解释为审批按键；
- 多 Agent 自动交汇、自动转发、Thread/Topic；
- 多租户、复杂 RBAC、admin 角色与跨用户管理；
- Server 完全盲转发的端到端加密；
- OpenCode、Codex adapter 的实现；Pi adapter 仍按 [pi-integration.md](pi-integration.md) 的独立里程碑推进。
