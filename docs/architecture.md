# Agora 架构（产品化一期）

> 状态：Design / 下一阶段实现目标
>
> 本文描述产品化一期的 Server / Daemon / Agent Runtime 架构、数据边界与断连行为。Web UI、事件规范化模型和通知 adapter 基础已在当前仓库实现；Server–Daemon 拆分、设备配对、远程 relay 与按需历史仍是下一阶段实现目标。Claude Code 的 PTY 是当前 provider 实现，Pi 的 stdio RPC/JSONL 是下一接入实现；二者都不是 Server 协议的前提。Daemon 重启期间保持 Agent 运行态的 per-session `session-host` 目标设计见 [session-host.md](session-host.md)。

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
              provider registry + Host clients
              live observer + history reader + outbox
                           │ control socket
                           ▼
                 Session Host（每个 Session 一个）
                           │
                           │ PTY / stdio RPC / HTTP / ACP
                           ▼
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
- 通过 provider registry 选择 Agent Runtime 和 Session Host；
- 通过 Host control socket 管理目标 Agent，不直接拥有跨 Daemon 重启所需的 Agent process/PTY；
- 使用对应的 live observer 和 history reader；
- 通过出站连接自动连接 Server；
- 断线继续连接 Session Host，在有限 outbox 内缓存关键状态；
- 重启后扫描本地 runtime registry，重新接管仍存活的 Session Host。

### Session Host（目标架构）

- 一个 managed Agora Session 对应一个独立 Host；
- 持有 Agent child process、PTY/stdio/HTTP-ACP transport 及可选 attach surface；
- Daemon 断线或重启时继续运行，不把 disconnect 当作 stop；
- Agent 退出后发送退出结果、清理 socket/metadata/PTY 并自行退出；
- 负责本地 control socket 的 handshake、token 校验和运行态 identity；
- 详细的进程拓扑、接管、清理、rebind 和安全约束见 [session-host.md](session-host.md)。

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
pi / claude wrapper
        │  local Unix socket (~/.agora/daemon.sock)
        ▼
Agora Daemon ── device credential / WebSocket ──→ Agora Server
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
Native wrapper（无浏览器 token）
        │  local daemon IPC
        ▼
Daemon device credential
        │  Server 校验设备身份
        ▼
Web principal（Logto 登录或 local 用户）
        │  Server 校验 session ownership
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
- Daemon ↔ Agent Runtime/Wrapper：Unix domain socket / loopback 控制面；Claude 和 Pi wrapper 都通过 Daemon 返回的 PTY attach socket 使用原生 TUI；
- Server 持久化：当前实现中的控制面和 session metadata；不保存 Daemon 的完整业务历史；
- Daemon identity：首次运行生成 UUID 并写入 `~/.agora/config.json`；该配置文件只保存设备身份，不保存 Session runtime、PID、transport、cursor 或 history；
- Daemon 按 provider 的 HistoryCatalog 持续建立和刷新内存中的 history index；Claude 使用 `~/.claude/projects/*/*.jsonl`，Pi 使用 `~/.pi/agent/sessions`，因此 Daemon 启动后新建的外部会话也能被发现并通过 resync 上报；这些条目可 resume，但不恢复 PID、transport、cursor 或 observer；
- Claude 历史事实源：用户电脑原有项目 JSONL；Pi 历史事实源：`~/.pi/agent/sessions` 下的 session JSONL；OpenCode 未来可使用 SQLite/export；
- 前端实时更新：WebSocket（Daemon）+ SSE（Web）；

### 6.1 Daemon 部署边界（产品化一期目标）

Daemon 只部署在运行 Agent 的用户工作站上。Server 是远程控制面，不属于本节的工作站安装范围；用户不需要在 Daemon 工作站上安装或启动 Server。

一期只规划 Linux 和 macOS 的原生 Daemon 部署：

| 平台 | 用户级后台托管 | 状态 |
|---|---|---|
| Linux | `systemd --user` service | 目标支持 |
| macOS | `launchd` `LaunchAgent` | 目标支持 |
| Windows | 无 | 暂不支持 |

产品化安装入口由 Server Web UI 提供。用户在 Web UI 添加设备后，获得固定 HTTPS 安装脚本、短期一次性 pairing code 和类似下面的命令：

```bash
curl -fsSL https://agora.example.com/download/install.sh \\
  | sh -s -- \\
  --server https://agora.example.com \\
  --pair <one-time-code>
```

计划中的脚本负责检测平台/架构、下载并校验发行包、完成配对、写入当前用户的 Server URL 和 device credential，并注册和启动对应的用户级服务。它不默认提权、不写系统级服务，长期 credential 不进入命令行参数、服务环境变量或普通日志。`agora daemon --pair <code>` 仍可作为开发/调试底层入口，但 `agora daemon install` 不作为一期终端用户入口。

上述安装脚本和服务注册属于产品化目标，不代表当前仓库已经提供下载端点或安装器。当前源码树中的 `make server`、`make start` 和 `make local` 仍是开发/试跑流程。Daemon 自更新、Windows Daemon、以及在工作站安装 Server 均不属于一期范围。

## 7. 一期不包含

- IM 入站消息写入 Agent；
- IM 审批按钮或普通文本解释为审批按键；
- 多 Agent 自动交汇、自动转发、Thread/Topic；
- 多租户、复杂 RBAC、admin 角色与跨用户管理；
- Server 完全盲转发的端到端加密；
- OpenCode、Codex adapter 的实现；Pi adapter 仍按 [pi-integration.md](pi-integration.md) 的独立里程碑推进。
