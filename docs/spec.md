# Agora Specification

> 状态：Draft / productization baseline
>
> 本文档描述 Agora 当前认可的产品边界、已验证原型能力和产品化一期方向。已验证内容以真实 Agent 探测和现有代码为准；Server–Daemon 拆分、设备配对、远程 relay 与按需历史仍是下一阶段设计，不视为已实现。

## 1. 产品概述

Agora 是一个本地优先的多 Agent 协作控制台。它把多个已经存在的 Agent Session——当前实现优先是 Claude Code，下一接入对象是 Pi——组织到一个可观察、可通信、可人工干预的协作群组中。

Agora 不是新的 Agent SDK、LLM 编排框架，也不是单纯的终端窗口聚合器。它提供位于人类和多个外部 Agent 之间的协作控制层：

- 接入和管理外部 Agent Session；
- 在一个群组中观察多个 Session；
- 为 Session 设置当前群组中的角色标签；
- 让人类明确地向某个 Session 发送输入；
- 允许人类编辑并转发一个 Session 的结果给另一个 Session；
- 记录事件、消息和控制操作的本地来源；产品化一期 Server 不持久化完整业务数据，完整 Agent 历史仍由各 provider 的本地或远端事实源提供。

第一版的核心原则是：

> **共享信息流可以观察，但不会自动扩散；只有明确指定的消息才进入目标 Session。**

## 2. 产品目标与非目标

### 2.1 当前目标

> 产品化一期以 Server–Daemon–Agent 为主线；本节保留原型阶段的 Coordination 能力，具体是否在一期继续保留，以产品化 API 契约为准。

1. 用户可以创建一个 Coordination 群组。
2. 用户可以在 Agora 中查看并纳管由用户启动的真实 Agent Session。
3. 用户可以在一个界面中查看这些 Session 的状态和输出。
4. 用户可以为每个成员设置名称和角色，例如 reviewer、implementer 或 tester。
5. 用户可以明确选择一个 Session 查看其观察流；输入仍由用户直接在 Agent 原生界面或 Agora 明确支持的 control transport 中完成。
6. 用户可以使用简单的 `@成员名称` 语法将输入发送给指定成员；具体输入能力由 Agent Capabilities 决定。
7. 用户可以从一个 Session 的输出中选择内容，编辑后发送给另一个 Session。
8. Session 输出默认只进入共享观察流，不自动注入其他 Session。
9. 消息和事件可以持久化，并在 Agora 重启后查看历史。
10. 发送失败和 Adapter 不支持的能力必须在 UI 中明确显示。
11. IM 可以接收聚合后的 Session 关注通知，并通过普通登录 Session URL 或短期 read-only notification capability link 打开 Agora Web 查看指定 Session；认证模式下有效 capability link 不要求用户再次输入登录信息，但不授予控制权限。
12. Server 维护归一化用户 principal 与设备归属：默认 trust-local 模式使用 `local` 用户；显式 auth 模式推荐使用 Logto 认证 Web 用户，由已认证用户签发 pairing code 将 Daemon 绑定到该用户。

### 2.2 第一阶段非目标

第一阶段不做：

- 云端部署、多人协作和多租户；
- 一次性支持所有 Agent 或桌面工具；
- 自动 Agent-to-Agent 消息转发；
- 自动 Driver、自动任务分解或复杂工作流；
- `@role`、`@group`、`@all` 等群组广播；
- 独立 Thread/Topic 实体、用户级订阅和 Agora 内部通知中心；
- 复杂的 ACK/NACK、优先级队列、消息幂等协议或分布式消息总线；
- Agent 风暴检测、发言令牌和自动断路器；
- 自动学习审批策略；
- 让共享时间线自动进入所有 Agent 的上下文；
- 用 Mock Agent 的假行为定义核心产品抽象；
- 自建密码、社交登录或多 provider 登录页面；认证由 Logto/OIDC 等身份服务提供；
- admin 角色、复杂 RBAC 或跨用户管理；v1 只做用户自身设备与 Session 的授权；
- 在 trust-local 模式下把 Server 暴露到非 loopback 地址；

这些能力可以在真实使用证明有需要之后再设计。

## 3. 核心概念

### 3.1 Coordination

`Coordination` 是用户可见的群组或协作空间。一个 Coordination 可以包含多个 Agent Session，也可以包含来自不同 Workspace 的 Session。

```text
Coordination
├── Claude Code Session A
├── Claude Code Session B
└── Future Agent Session C
```

Coordination 的生命周期独立于 Session：

- Session 结束，不代表 Coordination 被删除；
- Session 断开，不代表群组历史丢失；
- 删除 Coordination，不应默认删除外部 Agent 的会话历史或 Workspace 文件。

### 3.2 CoordinationContext

`CoordinationContext` 是 Coordination 的运行时协作上下文。它保存当前群组的成员、角色、共享观察流和消息历史，但不等于任何一个 Agent 的上下文窗口。

消息的发送状态和控制结果由 Daemon/Claude 本地运行态及实时 Server relay 反映；一期不要求 Server 保存完整 Message 历史。

- 成员列表和成员状态；
- 成员名称和角色；
- 共享观察流；
- 明确发送的消息；
- 消息与事件的简单关联；
- 人类控制操作历史。

CoordinationContext 不负责自动判断一个会话属于哪个 Mission，也不负责把所有共享内容编译成所有 Session 的 Prompt。

### 3.3 Session

`Session` 是一个外部 Agent 的真实会话，是 Agora 接入、观察和控制的基本运行单元。

Session 至少应记录：

- Agora 内部 Session ID；
- Agent 类型，例如 `claude-code`；
- 外部 Session ID（如果存在）；
- 使用的 Adapter；
- Workspace；
- 当前状态；
- Adapter 支持的能力；
- 原始事件和规范化事件；
- 最近连接和错误信息。

Agora 采用 **claude-wrapper + managed PTY** 模式：Claude Code 仍以原生 TUI 运行，但由 Agora 持有其 PTY；wrapper 只是终端渲染和输入转发层。Agora 同时观察该子进程写入的 sessions metadata 与项目 JSONL，并把规范化事件提供给 Web；状态/attention 变化可以经过独立通知策略发送到 IM。

关键环境边界：启动 Claude 子进程前，Agora 必须剥离 Cursor/Claude 父会话注入的 `CLAUDE_CODE_CHILD_SESSION`、`CLAUDE_CODE_SESSION_ID`、`CLAUDE_PID`、`CLAUDE_CODE_ENTRYPOINT`、`CURSOR_*`、`AI_AGENT` 等变量，否则 Claude 会关闭 transcript/JSONL 持久化。

第一版定义一种核心 Session：

- **Managed Session（托管型）**：由 `claude-wrapper` 或 Web UI 请求创建。Agora 在自有 PTY 中启动原生 Claude TUI，用户通过 wrapper 获得完整原生体验；Web 消息写入同一个 PTY master，JSONL observer 摄取同一会话的 user/assistant/tool 事件。IM 第一阶段接收状态通知并链接回 Web；未来若允许 IM 输入，必须经过独立授权。Agora 重启后用存储的 session id 通过 `claude --resume <id>` 重新拉起。

早期的独立外部会话 import 方案已废弃；wrapper 是唯一入口。

### 3.4 Workspace

`Workspace` 是 Session 的工作目录、代码仓库或其他执行环境。Workspace 与 Coordination 正交：

- 一个 Coordination 可以包含不同 Workspace 的 Session；
- 同一 Workspace 可以被多个 Session 使用；
- Workspace 不决定 Session 的角色；
- 不同 Workspace 之间默认只共享用户明确发送的消息文本，不自动共享文件内容、绝对路径或操作权限。

### 3.5 CoordinationMembership

`CoordinationMembership` 表示一个 Session 在某个 Coordination 中的成员身份。

第一版只保留三个主要属性：

- `session_id`；
- `display_name`；
- `role`。

Role 目前只是成员标签，用于 UI 识别和人类理解，不参与自动路由、自动响应或权限提升。同一个 Session 未来可以在不同 Coordination 中拥有不同角色。

为了避免多个群组同时向同一 Session 注入消息，MVP 运行时暂时限制一个 Session 同时只属于一个 active Coordination。

### 3.6 Mission / Task

Mission 或 Task 是对用户目标的可选语义描述，不是 Session 的强制父对象。

一个 Claude Code Session 可能包含多个问题；一个用户目标也可能涉及多个 Session。因此第一版不自动推断 Mission 边界，也不要求每个 Session 必须属于一个 Mission。未来可以在 Coordination 之上增加任务归类层。

## 4. 信息流模型

第一版只保留三条清晰的路径。

### 4.1 Session Event → Observation Stream

Session 产生的原始输出和状态变化被转换为事件，进入 Coordination 的共享观察流：

```text
Claude Code Session
        │
        ▼
      Event
        │
        ├── Daemon 本地原始 JSONL（事实来源）
        └── Server 内存转发 / Web SSE
```

事件包括：

- 文本输出；
- 工具调用和工具结果（如果 Adapter 能识别）；
- Session 启动、等待、结束和错误；
- 审批或确认请求（如果 Adapter 能识别）；
- Session 启动、等待、结束和错误；
- 审批或确认请求（如果 Adapter 能识别）；
- 人类控制操作。

第一版事件主要用于 Daemon 本地观察和 Server 内存转发；完整历史由 Claude 原始 JSONL 保留，不在 Server 形成业务事件库。

### 4.2 Session Attention → IM Notification → Web

Session 的规范化状态或 attention 变化可以经过通知策略发送到外部 IM：

```text
Session state / PTY attention
        │
        ▼
通知策略（聚合、去重、debounce）
        │
        ▼
IM 摘要 + Session deep link
        │
        ▼
Web history/SSE/PTY snapshot
```

IM 在第一阶段是低噪音的通知和入口，不是完整 transcript mirror。通知只包含 Session/Coordination 标识、状态、短摘要、时间和打开 Agora Web 的链接；普通 token、每个 JSONL event、PTY raw bytes、ANSI/VT 内容和完整工具参数不逐条推送。通知投递失败不应改变 Session 执行结果，也不应重复发送 Agent input。

Web 仍然是完整观察界面，负责显示 JSONL/history、事件流和当前 native Claude TUI 的只读 PTY snapshot。通知链接只打开指定 Session，不授予额外写入、attach 或审批权限。

第一阶段建议先支持出站 generic webhook 或单个 IM provider adapter。核心通知接口与 Session/Message 分离：通知不是 `Message`，不会进入 Session 上下文，也不会自动广播给其他 Session。IM 入站消息若未来需要作为明确 Session input，必须另行设计身份、授权和投递边界。

### 4.2 Human Message → Session Input

人类向一个 Session 发送消息时：

```text
用户输入
   │
   ├── 解析可选的 @成员名称
   ├── 确定唯一目标 Session
   ├── 保存 Coordination Message
   ├── 调用 ClaudeCodeAdapter 发送输入
   └── 显示发送成功或失败
```

- **Managed Session**：Agora 将消息写入该会话的 PTY master，等价于在原生 Claude TUI 中提交输入；输出由 JSONL observer 摄取，并经事件流/SSE 推送。

如果没有 `@成员名称`，消息发送给当前 UI 选中的 Session。

### 4.5 Human-in-the-Loop 与终端状态

Claude Code 的工具调用和审批提示目前通过不同通道到达 Agora：

```text
assistant/tool_use → 项目 JSONL → history observer → normalized event/SSE
permission prompt  → 原生 TUI → PTY master → attach client
```

因此，Agora 当前可以观察到工具名称、工具参数和后续工具结果，但不能仅凭 JSONL 判断 Claude 是否正在等待用户批准。`waiting` 只表示通用等待状态，不表示 pending approval；当前 `can_approve` 必须保持为 `false`，直到审批识别和控制链路经过真实环境验证。

PTY attach 当前是实时 raw terminal I/O 广播，不是可回放的终端状态接口：

- `serveAttach` 消费 PTY master 输出并广播给已连接 client；
- 不保存当前虚拟终端屏幕或输出 ring buffer；
- 晚连接的 client 看不到已经输出的 approval prompt；
- Web UI 当前消费 JSONL/SSE，不直接消费 Unix PTY socket。

后续 PTY 改造必须先以只读观测为目标：保存最近 raw bytes、维护 terminal snapshot、记录 ANSI/VT100 控制序列，并验证真实 permission prompt 的屏幕格式。只有在屏幕识别稳定后，才能增加结构化 `approval_required`/`approval_resolved` 事件和受控的 approve/reject API。不得把任意 Web 文本直接当作审批按键注入 PTY。

在该能力完成前，产品必须明确显示“可观察工具调用，但不支持 Agora 审批”，不能把普通 waiting 状态渲染成已识别的 Human-in-the-Loop。

### 4.3 Manual Forward → Session Input

用户可以从某个 Session 的输出或事件中选择内容，编辑后发送给另一个 Session：

```text
Session A 的输出
        │
        ▼
用户选择并编辑
        │
        ▼
发送给 Session B
        │
        ▼
新的 Coordination Message + Session Input
```

转发必须是用户明确操作，不能由 Agora 因为某个角色或某种事件自动触发。

### 4.4 共享观察与输入注入的边界

共享观察流中的内容**不应自动全部注入每个 Session 的上下文**。第一版不实现自动 Context Compiler，也不实现隐式消息广播。

唯一进入目标 Session 的协作输入是用户通过 Web UI 发送的消息；未来如果允许 IM 客户端发送消息，也必须经过单独的身份、授权和输入仲裁设计：

- **Managed Session**：Agora 将 Web 消息写入该会话的 PTY master，追加到同一个原生 TUI 会话；JSONL observer 摄取后进入共享观察流并推送到 SSE。未来经过授权的 IM input 若实现，也必须走同一明确 Session input 路径；通知本身不进入会话。会话由 Agora/claude-wrapper 创建，不存在向外部进程注入的问题。

## 5. 第一版消息模型

### 5.1 Message

第一版消息模型保持简单：

```go
type Message struct {
    ID             string
    CoordinationID string
    Sender         Endpoint
    Recipient      Endpoint
    Content        string
    ReplyTo        *string
    CreatedAt      time.Time
    Status         MessageStatus
}

type Endpoint struct {
    Type string // human | session
    ID   string
}

type MessageStatus string

const (
    MessagePending MessageStatus = "pending"
    MessageSent    MessageStatus = "sent"
    MessageFailed  MessageStatus = "failed"
)
```

具体实现可以调整字段类型，但第一版必须能够追踪：谁发送、发给谁、内容是什么、何时发送、是否成功。

### 5.2 接收者范围

第一版的 Recipient 只支持一个明确的 Session：

```text
session:<session-id>
```

暂不支持：

```text
role:<role>
group:<group>
all:<coordination>
topic:<topic>
```

### 5.3 @成员语法

`@成员名称` 只是用户输入的便利语法，解析后立即变成一个具体 Session ID，不是复杂的底层寻址协议。

第一版支持：

```text
@reviewer 请检查这个接口
@claude-b 请根据上面的结果继续处理
```

解析规则：

1. 只匹配当前 Coordination 中的成员；
2. 名称必须唯一；
3. 无法匹配或匹配多个成员时，不发送并提示用户；
4. 解析结果保存为具体 `recipient`；
5. Agent 输出中的普通文本不会因为包含 `@名称` 而自动产生转发。

第一版不支持：

- `@role`；
- `@group`；
- `@all`；
- 静默提及；
- 自动响应提及；
- 提及触发的通知等级。

### 5.4 ReplyTo

第一版只保留可选的 `ReplyTo` 字段，用于表示一条消息回复了哪一条消息或事件：

```text
Message A: Reviewer 发现认证问题
Message B: Implementer 已完成修复
          ReplyTo = Message A
```

不引入独立 Thread 或 Topic 实体。未来如果实际消息数量和协作方式证明需要，再基于 `ReplyTo` 演进。

### 5.5 发送失败

第一版采用简单的发送状态：

- `pending`：已保存，等待发送；
- `sent`：Adapter 已接受发送请求；
- `failed`：发送失败，保存错误信息并在 UI 展示。

第一版不设计复杂重试协议。是否重试由用户重新发送或后续基于真实失败模式决定。

## 6. Agent Adapter

### 6.1 Agent 接入分层

Agora 接入外部 Agent 时，不把 PTY、JSONL 或某个厂商的命令行当成产品前提。通用接入边界、能力语义和渐进抽象见 [agent-integration.md](agent-integration.md)。Claude Code 的 PTY 方案是一个 provider-specific 实现，Pi 的 RPC/JSONL 方案是下一份验证不同 transport 的实现。

目标分层包括：

- ProviderDescriptor：版本探测、provider 能力和组件 factory；
- SessionDriver：start、resume、send、interrupt、stop 和 process state；
- ControlTransport：PTY、stdio RPC、HTTP/ACP 等控制通道；
- HistoryReader/HistoryCatalog：provider-owned history locator、cursor 和按需读取；
- LiveEventParser/HistoryEventParser：将原始 live/history 记录归一化为 `event.Event`；
- TerminalSurface：可选的 PTY attach、raw frame 和 snapshot，不属于所有 Agent 的必需能力。

### 6.2 Claude Code 与 Pi 的验证顺序

Claude Code 保持现有真实探测、managed PTY、原始 JSONL observer 和 `--resume` 路径。Pi 采用单独的 provider adapter：

```text
Pi 0.84.2
  ├── --mode rpc  → prompt / steer / follow_up / abort / state
  ├── --mode json → live JSONL observation
  └── session JSONL → history catalog + cursor + resume
```

Pi 不默认启动 PTY，不把 `--approve` 视为结构化审批，不把 delta 事件和最终消息重复发布。详细实测基线、事件映射、能力矩阵和实施阶段见 [pi-integration.md](pi-integration.md)。本次文档更新不代表 Pi adapter 已经在 Go 代码中实现。

### 6.3 Adapter 能力

外部 Agent 的能力可能不同，Adapter 必须声明实际支持的能力，UI 根据能力显示操作：

```go
type Capabilities struct {
    CanStart       bool
    CanDiscover    bool
    CanAttach      bool
    CanSendInput   bool
    CanStream      bool
    CanInterrupt   bool
    CanResume      bool
    CanApprove     bool
    CanReadHistory bool
}
```

某个 Adapter 只能观察而不能控制时，UI 应明确显示降级状态，不伪造成功。

### 6.4 Mock 与 Replay

不采用 Mock-first 开发。先运行真实 provider 并采集输出、状态和 Session 历史，再将真实记录保存为 fixture，用于 parser、状态机和 UI 回放测试。

未来可以实现薄的 ReplayAdapter，但它只能回放真实采集记录，不能成为产品抽象的主要来源。

## 7. 初步系统架构

产品化一期采用 **Server / Daemon / Agent Runtime 三角色拆分**：Server 是远程控制面和统一入口，Daemon 在用户电脑上运行并自动连接 Server，Agent Runtime/Wrapper 负责 provider-specific 的启动、控制和观察。Web 与 IM 都只面向 Server。

认证与授权模式：

- 默认 `AGORA_AUTH_MODE=local`：Server 强制监听 `127.0.0.1`，自动使用 `local` 用户，不要求 Web 登录或 Daemon credential；若配置非 loopback 地址，Server 拒绝启动。
- 显式 `AGORA_AUTH_MODE=logto`：Web 通过 Logto 授权码 + PKCE 获取身份，Server 校验 JWT 的签名、issuer、audience 和 expiry，并将稳定 `(provider, subject)` 映射为内部用户；非 loopback 部署必须 TLS。
- 已认证用户签发短期 pairing code，Daemon 换取 per-device credential；`device_id -> user_id` 归属在配对时决定。
- Web 请求授权链为 `principal.user_id -> device.user_id -> session.daemon_id`；Daemon 协议只验证设备，不传递 Web 用户身份。
- IM 普通链接按正常 Web 登录处理；notification capability link 可免再次登录，但只授权指定 Session 的 `read_observation`。

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
              Agent Runtime / Control Transport
              provider-specific driver + observer
                           ▲
                           │ PTY / RPC / HTTP / ACP
                    外部 Agent Session
```

职责与数据边界：

- **Server**：保存用户身份映射、设备归属、配对状态、在线路由和必要的配置元数据；向 Web 提供 REST/SSE；接收 Daemon 上报；将 Web 请求路由给已配对 Daemon；基于实时上报的规范化状态/摘要向 IM webhook 投递。**Server 不持久化业务数据**：transcript、Event、Message、PTY snapshot、通知正文均不落库，转发过程中短暂可见但不落盘。
- **Daemon**：仅运行在用户电脑；通过 provider-specific driver 启动/恢复 Agent，使用对应的 control transport、live observer 和 history reader；PTY、终端 snapshot 和 attach 只是 Claude 等少数 Agent 的可选 surface。Daemon 不扫描用户全部 Agent 会话，只管理通过 Agora Wrapper/Web/RPC 显式纳管的 Session；完整历史继续由各 provider 的原始事实源提供。
- **Agent Runtime/Wrapper**：本地 provider-specific 入口，可能是 Claude PTY wrapper、Pi stdio RPC driver 或未来 OpenCode HTTP/ACP driver；不把某个 Agent 的原始协议暴露给 Server。

Managed 会话的消息路径：

```text
Agent Runtime / Control Transport
        │  PTY / stdio RPC / HTTP / ACP
        ▼
外部 Agent Session
        │  provider-owned live events + history
        ▼
Observation / History adapters → Daemon → Server 内存 SSE/通知 → Web
                              └→ 归一化状态/attention → Notification policy → IM 摘要 + Web deep link
```

认证模式下，Web 请求先由 Server 解析 principal，再检查 `principal.user_id -> device.user_id -> session.daemon_id`，通过后才向目标 Daemon relay。Daemon 协议只携带设备身份，不携带 Web 用户身份。notification capability link 只进入 history、SSE 和只读 PTY snapshot 路径，不进入 Session input、resume、stop、attach 或设备管理路径。

### 7.1 初步技术选型

- 后端：Go；
- 前端：TypeScript + React + Ant Design 5；
- 开发运行方式：本地开发可由兼容的 `agora serve` 同时启动 Server/Daemon；产品化部署使用 `agora server` + `agora daemon`；
- Server ↔ Daemon：出站 WebSocket 长连接，设备配对、心跳、自动重连、resync 和有限 outbox；
- Web 认证：Logto OIDC（授权码 + PKCE）为推荐身份服务；JWT 由 Server 校验签名、issuer、audience 和 expiry，稳定 `(provider, subject)` 映射到内部用户；
- 运行模式：`AGORA_AUTH_MODE=local`（默认 trust-local，强制 loopback）或 `AGORA_AUTH_MODE=logto`（显式认证 + TLS）；
- 设备认证：已认证用户签发短期 pairing code 换取可撤销的 per-device credential，设备归属关系由 Server 保存；
- IM 入口：普通 Session URL 走正常登录；短期 notification capability link 兑换为绑定单个 Session 的 `read_observation` cookie；
- Daemon ↔ Agent Runtime/Wrapper：Unix domain socket / loopback 控制面（Claude PTY wrapper 当前实现），Pi 等无 TUI Agent 由 Daemon 内的 stdio RPC driver 承担；
- Server：REST + SSE + Daemon WebSocket relay；只保存控制面状态，不保存完整业务历史；
- Daemon：当前实现为 Claude 原生 TUI + `creack/pty` + Unix domain socket + JSONL Observer；接入多 Agent 后按 [agent-integration.md](agent-integration.md) 的 SessionDriver/Transport/History 分层扩展；只保存纳管 Session 的映射、游标、进程状态和有限 outbox；
- Claude 历史事实源：用户电脑上 Claude 原有项目 JSONL；Pi 历史事实源为 `~/.pi/agent/sessions` 下的 session JSONL，不复制到 Server；
- 初期 IM：Server 侧 provider adapter，支持飞书、钉钉、企业微信和 generic HTTP webhook 的简单文本请求；
- 设备认证：已认证用户签发短期 pairing code 换取可撤销的 per-device credential；
- 用户认证：默认 local trust-local；远程/跨设备采用 Logto/OIDC；
- 持久化：Server 只保存用户、设备、配对、路由等控制面数据；Daemon 只保存本地运行态；Event、Message、transcript、PTY snapshot、通知正文不作为一期 Server 业务数据持久化；
- 前端实时更新：WebSocket（Daemon）+ SSE（Web）；
- 会话驱动：provider-specific——Claude 为原生 TUI + `creack/pty` + Unix domain socket，Pi 为 stdio RPC，OpenCode 未来为 HTTP/ACP；
- 环境隔离：Claude 启动前剥离 Claude/Cursor 子会话变量，确保 JSONL 持久化；Pi 等无 TUI Agent 按各自 provider 的隔离约定处理；
- `agora wrapper` / `agora attach`：Claude PTY 的终端渲染端，不是所有 Agent 的必需 surface；
- 暂不引入 NATS、RabbitMQ 或其他分布式消息基础设施；
- A2A 和 MCP 暂不作为 MVP 的前置依赖。

## 8. 用户界面

Coordination 主视图只需要支持最小闭环：

- **成员面板**：成员名称、Agent 类型、Workspace、角色和状态；
- **共享观察流**：显示各 Session 的事件和消息；
- **Session 详情**：查看单个 Session 的完整输出；
- **输入框**：向当前选中的 Session 发送消息；
- **@成员解析**：将文本中的唯一成员名称解析为目标 Session；
- **转发操作**：选择事件或输出，编辑后发送给另一个 Session；
- **错误提示**：显示发送失败、连接断开和能力不支持。

第一版不需要独立通知中心、复杂群组广播 UI、Thread 导航或自动协调面板。

## 9. 安全与审计

即使第一版保持简单，也必须保证消息边界清楚：

- 明确显示消息发送者和接收者；
- 共享观察不等于 Session 输入；
- 不允许因角色标签自动获得执行权限；
- 不同 Workspace 之间不自动共享文件内容或操作权限；
- 发送、转发和控制操作在 Daemon 本地或 Claude 原始历史中保留；Server 不持久化完整 Message 内容；
- Adapter 不支持的控制能力必须明确报错；
- Server 必须把已验证的 Web 身份归一化为稳定的 user principal，不信任前端或 Daemon 自报的 `user_id`、email、`daemon_id` 或 Session 所有权；
- 显式认证模式下，Session 读取和控制必须经过 `principal.user_id → device.user_id → session.daemon_id` 的归属检查；跨用户、跨设备和不存在的路由统一拒绝；
- trust-local 模式默认使用 `local` 用户，Server 只监听 `127.0.0.1`；配置非 loopback 监听时必须拒绝启动，不能通过关闭认证把本地服务暴露到网络；
- Logto/OIDC 只负责证明 Web 用户身份，设备归属由已认证用户签发的一次性 pairing code 和可撤销的 per-device credential 建立；v1 不提供 admin 角色或复杂 RBAC；
- IM notification capability link 必须是绑定单个 Session、短期有效的 `read_observation` bearer capability，不得等同于完整登录凭证；兑换出的会话只能访问目标 Session 的 metadata、history、SSE 和只读 PTY snapshot；
- capability link 不得调用消息输入、resume、stop、PTY attach 写入、审批、设备管理或 webhook 配置接口；token 不写入日志、错误响应或普通业务持久化；
- 通知正文不得包含完整 transcript、PTY raw bytes、workspace 私密路径、凭据或完整工具参数；共享 IM target 视为 capability link 的可信接收边界。

产品化一期不要求 Server 重启后恢复完整业务历史；Server 只恢复控制面并等待 Daemon 重新注册。Daemon 本地历史和 Claude 原始 JSONL 仍然是纳管 Session 的事实来源。

## 10. 开发阶段

### Phase 0：Claude Code 现实探测

产物：

- Claude Code 集成笔记；
- 启动、输入、输出、结束和错误的真实样本；
- Session 历史样本；
- 能力矩阵；
- 可行的控制和恢复路径；
- 已知版本耦合和降级方案。

### Phase 1：单 Session 纵向闭环（原型验证，已完成）

```text
一个 Coordination
└── 一个真实 Claude Code Session
```

实现：

- 创建或打开 Coordination；
- 启动或附加 Claude Code；
- 指定 Workspace；
- Web UI 显示实时事件；
- 用户发送输入；
- 用户通过 Web 或 Agent Runtime 发送输入；Daemon/Agent 原始 history 保留本地事实，Server 只转发请求和实时结果；
- 显示 Session 状态；
- Agora 重启后查看历史或明确显示无法恢复的原因。

### Phase 2：多个真实 Claude Code Session（原型能力，部分已完成）

```text
一个 Coordination
├── Claude Code Session A
└── Claude Code Session B
```

实现：

- 管理多个成员；
- 为成员设置名称和角色；
- 显示共享观察流；
- 向当前成员发送输入；
- 使用 `@成员名称` 指定目标；
- 人工编辑并转发 A 的结果给 B；
- 记录消息来源、目标和发送结果。

### Phase 2.5：claude-wrapper + Managed PTY 纵向闭环（原型验证，已完成）

```text
claude-wrapper（用户终端，原生 TUI）
        │ Unix socket
        ▼
Agora PTY Manager（持有 PTY master）
        │
        ▼
Claude 子进程 → JSONL Observer → Web/SSE
                         └→ 状态/attention → IM 摘要 + Web deep link
```

实现：

- `claude-wrapper` 按当前 workspace 创建 managed session 并连接其 PTY；
- Agora 启动 Claude 前剥离子会话环境变量，确保 sessions metadata 与 JSONL 持久化；
- wrapper 终端保留完整原生 Claude Code TUI；
- Web 消息写入同一个 PTY master，终端和 Web 操作同一个会话；
- JSONL Observer 摄取 user/assistant/tool/result 事件并通过 SSE 发布；
- Agora 重启后 `claude --resume <id>` 重建 PTY，wrapper 可重新连接；
- IM SDK 接入（钉钉/企微/飞书）时，第一阶段优先作为出站通知和 Session deep link 入口，不复制完整 transcript 或 PTY 画面；
- 如果未来允许 IM 入站消息作为 Session input，必须与出站通知分开设计权限、审计和输入仲裁。

> PTY/socket 双向中继已实测通过；环境变量隔离后原生 TUI JSONL 持久化已实测通过。

### Phase 3：Server–Daemon–Agent 产品化一期（下一阶段）

一期目标是先打通 IM/Server/Daemon/Agent Runtime 的可观察链路，优化单 Agent 体验，并以 Claude Code 现有路径和 Pi adapter 验证不同 transport：

```text
IM webhook / Web
        │
        ▼
Agora Server（转发、SSE、通知适配，不落业务数据）
        │ WebSocket
        ▼
Agora Daemon（provider registry + runtime + history/observation）
        │
        ├── Claude Code（PTY + JSONL）
        └── Pi（stdio RPC + JSONL）
```

一期包含：

- Server / Daemon / Agent Runtime 角色拆分；
- 设备配对、自动重连、心跳、resync 和有限断线 outbox；
- Web 通过 Server 查看纳管 Session 的实时事件和按需历史；
- Web 输入经 Server → Daemon → provider-specific transport（Claude PTY、Pi RPC 等）；
- Server 侧飞书、钉钉、企业微信、generic webhook 出站通知；
- Server 不持久化 transcript、Event、Message、PTY snapshot 或通知正文；
- Web 根据事件类型差异化呈现 text、thinking、tool call、tool result、status 和 error；
- 先保持 Claude Code 兼容路径，并以 Pi parser/driver 作为下一单 Agent 里程碑；两者均默认 `can_approve=false`，IM 只通知和 deep link，不接收入站文本。

一期不包含：

- IM 入站消息写入 Agent；
- IM 审批按钮或普通文本解释为审批按键；
- 多 Agent 自动交汇、自动转发、Thread/Topic；
- 多租户、复杂 RBAC、可靠分布式消息队列；
- Server 完全盲转发的端到端加密；
- OpenCode/Codex adapter 的实现（但其设计须遵循通用 Agent 接入规范）。

### Phase 4：多 Agent 交汇与扩展 Adapter（后续）

Pi adapter 先按 P0 parser/fixture、P1 RPC smoke test、P2 单 Session managed loop、P3 history/resume 推进；随后再评估 Session-to-Session 人工转发、多 Agent 交汇、Thread/Topic、OpenCode/Codex adapter、IM 入站输入的身份授权和 Server 完全盲转发的端到端加密模式。

## 11. MVP 验收标准

### Coordination 与 Session

- 用户可以创建一个 Coordination；
- 用户可以启动或附加一个真实 Agent Session；
- Session 的 Agent 类型、Workspace、状态和能力可见；
- Session 结束或断开后，Daemon 保留 provider 本地历史；Server 重启后不承诺恢复完整业务历史，只恢复控制面并等待 Daemon 重新注册。

### 事件观察

- 实时输出可以显示在 UI 中；
- 普通输出、工具活动、状态和错误（如果 Adapter 能识别）可以区分；
- 页面刷新或 Agora 重启后，Daemon 在线时可按需读取本地历史；Server 重启或 Daemon 离线期间，应明确显示历史暂不可用，而不是假设 Server 有完整副本；
- 共享观察流不会自动注入其他 Session。

### 消息发送

- 用户可以向当前选中的 Session 发送文本；
- 用户可以用唯一的 `@成员名称` 指定目标 Session；
- 无法解析的点名不会被静默发送；
- 发送状态至少区分 pending、sent 和 failed；
- 消息显示发送者、接收者、内容和时间；
- **Managed Session** 的输入经 Daemon provider-specific transport 发送（Claude PTY、Pi RPC 等）；输出经对应 live observer/history reader → Server relay → Web/SSE 读回。第一阶段 IM 出站通知不属于输入路径。

### IM 通知与入口

- 配置的 IM 能收到高价值 Session 状态/attention 变化的聚合通知；
- 同一状态不会因轮询或重复事件造成消息风暴；
- 通知包含 Coordination、Session、短摘要、时间和指定 Session 的 Web deep link；
- 认证模式下，已认证且拥有目标 Session 读取权限的用户签发的一条有效 notification capability link，可以让通知接收者无需再次登录打开指定 Session 的只读观察页；
- capability link 兑换后只能访问绑定 Session 的 metadata、history、SSE event stream 和只读 PTY snapshot，不得访问其他 Session；
- 过期、撤销、篡改、绑定其他 Session 或 scope 不为 `read_observation` 的 link 必须失败，不能回退为普通登录或其他 Session 的访问；
- 使用 capability link 调用消息输入、resume、stop、PTY attach 写入、审批、设备管理或通知配置接口必须被拒绝（403 或等价的受限错误），不能改变 Session 状态；
- capability link 兑换端点应清理地址栏中的 token，设置受限 HttpOnly、Secure、SameSite cookie 和 `Referrer-Policy: no-referrer`；实现不得依赖“首次 GET 立即永久消费”来抵抗 IM provider 或安全扫描器的预取；
- trust-local 模式直接使用 `local` principal，不需要 notification link 兑换；`127.0.0.1` deep link 只能提示在 Agora 所在机器打开，不能宣称可从远程 IM 客户端访问；
- IM 不承载完整 transcript、PTY raw bytes 或 ANSI/VT 画面；
- IM 不提供 approve/reject 按钮，普通 IM 文本不会被解释为审批按键；
- 通知投递失败不影响 Session 运行结果、Daemon 本地历史或 Web 观察；一期只保留内存中的投递结果，不形成持久化通知审计历史；
- 无可用远程 Web 地址时，通知明确要求用户在 Agora 所在机器打开，或省略不可用链接。

### 手动转发

- 用户可以从 Session A 的输出中选择内容；
- 用户可以在发送前编辑内容；
- 用户可以将编辑后的内容发送给 Session B；
- 转发生成新的 Message，不会修改原始事件；
- 没有用户明确操作时，Session A 的输出不会自动发送给 B。

### 角色

- 用户可以为成员设置显示名称和角色；
- 角色可以在成员面板和相关消息中显示；
- 角色不会自动触发广播、路由、响应或权限提升。

## 12. 架构决策记录

### ADR-001：Coordination 是产品层群组

Agora 的用户可见协作单元是 Coordination。一个 Coordination 可以包含多个真实 Agent Session。

### ADR-002：CoordinationContext 是运行时协作上下文

CoordinationContext 保存群组成员、角色、共享观察流和消息历史，不等同于任一 Agent 的上下文窗口。

### ADR-003：Session 优先，Mission 可选

外部 Agent Session 是 Agora 接入、观察和控制的基本事实单元。Mission/Task 不作为 Session 的强制父对象。

### ADR-004：角色属于 Membership，第一版只是标签

Role 描述 Session 在当前 Coordination 中的职责，但第一版不使用角色做自动路由、自动响应或权限提升。

### ADR-005：共享观察与上下文注入分离

共享事件默认只用于观察。只有用户明确发送或转发的内容，才进入目标 Session。

### ADR-006：真实 Provider 驱动 Adapter，规范化观察模型

第一阶段以真实 Claude Code 和 Pi 探测结果定义接入边界。Provider-specific 的启动、transport、history locator 和 parser 保留在 Daemon；Session、Capabilities 和 Event 作为跨 Agent 的规范化产品模型。不得用 Mock Agent 的假行为定义核心抽象。

### ADR-007：消息模型保持最小

第一版只支持一条消息发送给一个明确 Session，使用简单的 `@成员名称` 解析和可选 `ReplyTo`。不提前引入角色广播、群组广播、Thread、Topic 或复杂投递协议。

### ADR-008：Transport 是 Provider-specific，PTY 是可选 Surface

Claude Code 使用 Agora-owned PTY 保留原生 TUI；Pi 使用 stdio RPC 控制和 JSONL 事件；未来 OpenCode 可使用 HTTP/ACP。通用 runtime 不假设所有 Agent 有终端，attach、raw bytes 和 terminal snapshot 只在 provider 声明 `CanReadTerminal`/`CanAttach` 时提供。

### ADR-009：IM 是通知和 Web 入口，不是 transcript 镜像

第一阶段 IM 只接收聚合的高价值 Session 状态/attention 通知，并提供指向指定 Session Web 详情页的 deep link。完整 history、SSE 事件流和 PTY snapshot 留在 Agora Web；IM 不渲染 ANSI/VT、不保存 transcript 镜像，也不承载每条工具事件。

### ADR-010：通知与 Session input、审批控制分离

出站通知不是 `Message`，不会进入 Session 上下文。未来 IM 入站文本若要作为明确 Session input，必须单独完成身份、授权、审计和输入仲裁设计。任何普通 IM 文本都不能被解释为 Claude 审批按键；HIL 通知不改变 `CanApprove=false`。

### ADR-012：Server 是无业务持久化的 Relay/Gateway

产品化一期将 Server 定义为远程控制面和统一入口，而不是 Claude 业务数据库。Server 可以在转发过程中短暂处理规范化事件、消息、PTY snapshot 和通知摘要，但不持久化 transcript、Event、Message、PTY snapshot 或通知正文。Server 只保留设备、配对状态、在线连接、`session_id -> daemon_id` 路由和必要的配置元数据。Server 重启后由 Daemon 重新注册和 resync，不要求 Server 恢复业务历史。

### ADR-013：Daemon 只管理 Agora 显式纳管的 Session

Daemon 不扫描或同步用户全部 Agent history。只有通过 Agora Wrapper、Web 创建或显式注册的 Session 才进入管理范围。完整 history 继续留在 provider 原始事实源，Daemon 只保存纳管 Session 的本地映射、observer cursor、进程状态和有界 outbox。断线期间优先保留关键状态和错误，重连后通过稳定事件 ID 或游标补发。

### ADR-014：IM Provider 适配集中在 Server

Daemon 只上报规范化状态和短摘要，Server 负责根据用户配置的多个 webhook target 生成飞书、钉钉、企业微信和 generic 的简单文本请求。通知不进入 Session input，不提供审批按钮；provider 适配位于 Server 便于集中升级。通知正文和完整 webhook URL 不写入普通日志或 Server 业务数据库。

### ADR-015：Logto 证明身份，Agora 管理设备归属与 Session 授权

Agora 不自建密码、社交登录或多 provider 登录页面。显式认证模式使用 Logto/OIDC 证明 Web 用户身份，Server 将稳定的 `(provider, subject)` 映射为内部 `user_id`；不同登录方式由身份服务统一处理，不改变 Agora 的授权模型。已认证用户签发一次性、短期 pairing code，Daemon 兑换为绑定该用户且可撤销的 per-device credential。Server 根据 `principal.user_id → device.user_id → session.daemon_id` 执行 Session 授权。v1 不引入 admin 角色或复杂 RBAC；trust-local 模式是默认本地例外，固定使用 `local` 用户并强制 loopback 监听。

### ADR-016：通知链接是绑定 Session 的只读 capability，不是登录凭证

IM 通知使用 Agora 自己签发的短期 opaque notification capability link，而不是把 Logto access token、完整 Web session 或通用 API bearer token 放进消息。link 只绑定一个 Session 和 `scope=read_observation`，兑换后建立受限的 HttpOnly、Secure、SameSite cookie，并清理 URL 中的 token。它只允许读取目标 Session 的 metadata、history、SSE observation 和只读 PTY snapshot，禁止输入、resume、stop、attach 写入、审批、设备管理和通知配置。由于 provider 可能预取链接，短 TTL、scope 限制和撤销优先于“首次 GET 即永久消费”；token 泄露的后果明确限定为 TTL 内观察单个 Session，因此通知 target 必须视为可信接收边界。

## 13. 未决问题

这些问题暂不阻塞 MVP：
2. Agent 是否需要主动调用 Agora 发送消息；
3. 是否需要 Thread 或 Topic 来组织大量消息；
4. 是否需要用户级通知订阅、静默时段和未读状态；
5. 首选 generic webhook 或具体 IM provider（钉钉 / 企微 / 飞书）及其卡片格式；
6. 是否需要可靠重试、幂等和消息风暴防护；
7. 如何支持多个 Coordination 共享一个 Session；
8. Claude Code 的真实审批、暂停、恢复和附加能力；
9. Pi/OpenCode/Codex 等 provider 的版本兼容、history locator 和 capability discovery；
10. 是否需要自动摘要和上下文编译；
11. Managed Session 的输入仲裁：终端键盘与未来 IM 消息并发时如何串行化（回合制 / 队列）；
12. Managed Session 的安全模型：未来 IM 用户消息经何种权限进入 PTY；
13. 是否需要文件附件、provider 原生线程或多用户身份映射。

后续设计必须以真实使用和 Adapter 探测结果为依据，优先做减法，而不是预先实现完整的群聊或分布式消息系统。

## 14. 参考材料

- `Agora for multi agent chat tool - DeepSeek.pdf`：原始讨论记录；
- `Agora for multi agent chat tool - DeepSeek.txt`：PDF 文本提取结果。

这些材料用于记录背景和设计推演，不等同于本规范中的已确认需求或技术承诺。
