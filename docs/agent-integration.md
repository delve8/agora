# Agent 接入规范

> 状态：Design / 多 Agent 接入基线
>
> 本文定义 Agora 接入外部 Agent 的通用边界。它不是某一个 Agent 的 SDK 设计，也不要求所有 Agent 都具备相同的交互方式。Claude Code、Pi、OpenCode、Codex 可以使用不同的进程和协议，只要能映射到 Agora 的规范化 Session、Event 和 Capabilities。
>
> 当前状态：`session.Session`、`session.Capabilities`、`event.Event` 和 Server–Daemon 协议已经具备部分通用基础；runtime 中仍有 Claude-specific 耦合。本文件是后续渐进抽象的目标，不代表接口已经在代码中全部实现。

## 1. 设计原则

### 1.1 Session 是接入事实单元

Agora 管理的是外部 Agent 的 Session，而不是某一种终端、某一个模型或某一套原始 transcript 格式。每个 Session 至少有：

- `Agent`：provider/agent 标识，例如 `claude`、`pi`、`opencode`；
- `AgentSessionID`：外部 Agent 的原生 Session URI，例如 `pi://<pi-session-id>`；
- `Workspace`：Agent 运行或历史所属的工作目录；
- `Capabilities`：已探测并验证的可用操作；
- 本地运行态：PID、transport、history locator、cursor、连接状态。

canonical Agora ID 继续使用：

```text
daemon/<daemon-id>/<agent>://<agent-session-id>
```

Server、Web 和通知层只依赖 canonical ID、规范化事件和能力，不依赖 Agent 的原始 session ID 格式。

### 1.2 不把 PTY 作为统一前提

不同 Agent 的控制面不同：

| Transport | 典型 Agent | 适用能力 |
|---|---|---|
| PTY / Unix socket | Claude Code 原生 TUI | terminal attach、raw snapshot、键盘输入、信号控制 |
| stdio JSONL RPC | Pi | prompt、steer、follow-up、abort、状态查询、结构化事件 |
| Session Host control/attach | Pi、Claude Code 及其他 Agent | 进程生命周期、PTY/stdio/ACP 控制、实时观察 |
| HTTP / ACP | OpenCode | server session、结构化事件、远程/本地控制 |
| history-only | 外部已存在或无法实时控制的 Session | discover、read history、resume（如果 provider 支持） |

因此通用 Session driver 不应暴露一个“必然存在的 PTY”。PTY、terminal snapshot 和 attach 是可选的 `TerminalSurface`。

### 1.3 观察与控制分离

- `EventParser` 只负责把原始记录映射为 `event.Event`，不启动进程、不发送输入；
- `SessionDriver` 只负责生命周期和控制，不决定如何解析事件；
- `HistoryReader` 只负责读取 provider 的历史事实源，不推断控制权限；
- `ControlTransport` 负责协议 framing、请求关联和异步事件，不承担业务状态机；
- `runtime.Manager` 负责把上述组件组合成 Agora Session，而不是理解每个 Agent 的字段细节。

这能避免把 Claude 的 PTY、Pi 的 RPC、OpenCode 的 HTTP/ACP 塞进一个包含大量无意义 no-op 方法的“大而全 Adapter”。当 Agent 运行态需要跨 Daemon 重启保持时，以上 driver/transport 由独立的 per-session Session Host 持有；Session Host 的生命周期、接管和清理见 [session-host.md](session-host.md)。

## 2. 推荐的抽象边界

以下是目标形态的 Go-like 伪代码。它们用于说明职责，不是本次必须直接落地的 API。

### 2.1 ProviderDescriptor

```go
type ProviderDescriptor interface {
    Name() string
    Detect(ctx context.Context) (VersionInfo, error)
    Capabilities(ctx context.Context, mode RuntimeMode) session.Capabilities
    NewDriver(opts DriverOptions) (SessionDriver, error)
    NewHistory(opts HistoryOptions) (HistoryReader, error)
    NewLiveParser() LiveEventParser
    NewHistoryParser() HistoryEventParser
}
```

`ProviderDescriptor` 是注册表/factory 的入口，不拥有某个 Session 的状态。它只描述如何创建 provider-specific 组件。

### 2.2 SessionDriver

```go
type SessionDriver interface {
    Start(ctx context.Context, req StartRequest) (StartedSession, error)
    Resume(ctx context.Context, req ResumeRequest) (StartedSession, error)
    Send(ctx context.Context, sessionID, content string) error
    Interrupt(ctx context.Context, sessionID string) error
    Stop(ctx context.Context, sessionID string) error
    State(ctx context.Context, sessionID string) (DriverState, error)
    Wait(ctx context.Context, sessionID string) (ExitInfo, error)
    Close() error
}
```

约束：

- 不要求 `Send` 一定写 PTY；Pi 通过 RPC `prompt`，OpenCode 通过 HTTP/ACP，Claude managed session 才写 PTY；
- 如果 provider 不支持某操作，driver 返回显式错误，Manager 依据能力拒绝，不伪造成功；
- `StartedSession` 返回原生 session URI、PID（如果有）、workspace、transport metadata 和可选 history locator；
- `Wait` 是进程/driver 生命周期事实，不等同于 Agent 的“回合结束”；回合结束由 live event 或 driver state 解释。

### 2.3 Session Host 与 Daemon 生命周期

目标架构中，一个 managed Session 对应一个独立的 Session Host：

```text
Daemon → Session Host control socket → Agent process / PTY / RPC / ACP
```

Daemon 只负责 Host client、Server relay、history/observer 和规范化 metadata；Session Host 持有 Agent process 与 transport。Daemon 重启或短暂断线不能终止 Host/Agent，Agent 退出后 Host 负责通知、清理 runtime metadata、socket 和 transport 并自行退出。Session Host 不是 transcript 存储，也不改变 Session、Event、Capabilities 或 canonical ID 模型。

详细设计见 [session-host.md](session-host.md)。

### 2.4 ControlTransport

```go
type ControlTransport interface {
    Send(ctx context.Context, command Command) error
    Events() <-chan RawMessage
    Close() error
}
```

实现可以是：

- `PTYTransport`：输入输出是 raw bytes，适合 Claude TUI；
- `JSONRPCTransport`：stdin/stdout 都是 LF-delimited JSON，响应和异步事件共用 stdout，按 request ID 关联；
- `HTTPTransport`/`ACPTransport`：由 provider adapter 管理 HTTP 或 ACP 的请求和订阅。

Transport 不把 provider 的业务事件直接写入 Server；它只输出原始消息，由 parser 负责归一化。

### 2.5 Observation 与 parser

```go
type HistoryReader interface {
    Catalog(ctx context.Context) ([]HistorySummary, error)
    Read(ctx context.Context, cursor Cursor, sessionID string) ([]HistoryRecord, Cursor, error)
}

type LiveEventParser interface {
    ParseLive(sessionID string, raw []byte) ([]event.Event, error)
}

type HistoryEventParser interface {
    ParseHistory(sessionID string, raw []byte) (event.Event, error)
}
```

parser 必须：

- 保留原始内容到 `event.Event.RawJSON`（涉及隐私的日志另行脱敏）；
- 生成稳定的 `ID`/`ExternalID`；没有外部 ID 时使用原始记录 hash + source + session 生成；
- 区分最终消息和增量消息，避免把 delta 与 authoritative final message 重复发布；
- 对未知类型保留 raw JSON 并降级为可观察事件，不因新版本事件阻断主进程；
- 对非法 JSON、不完整尾行和超长记录采用有界处理；旁路解析失败不能改变 Agent 主通道结果。

`event.Event` 继续作为跨 Agent 的唯一观察模型：`Kind`、`Type`、`Role`、`Content`、`ToolName`、`ToolInput`、`ToolOutput`、`Thinking`、`IsError` 和 `RawJSON`。

### 2.6 History locator 与 cursor

History 不是固定路径。每个 provider 负责定义：

```go
type HistoryLocator struct {
    Provider string
    Path     string
    NativeID string
    Metadata map[string]string
}

type SessionCatalog interface {
    Provider() string
    // 返回会话 metadata/locator，不返回 transcript；Daemon 会持续调用。
    List(ctx context.Context, coordinationID, daemonID string) ([]Session, error)
}

type Cursor struct {
    Locator    HistoryLocator
    ByteOffset int64
    Line       int
    LastID     string
}
```

Claude 使用项目目录下的 `<session-id>.jsonl`；Pi 使用项目 key 目录下的 session JSONL；OpenCode 使用 SQLite database/session/message/part 查询或 provider export。通用 runtime 只保存 locator 和 cursor，不假定 `.claude`、文件名或 JSON 字段。

### 2.7 运行时 Session rebind：处理 TUI/上下文切换

一个 managed Agent 进程可能允许用户在原生交互界面中切换上下文，例如在 TUI 中执行 `/resume`，或使用 provider 自己的 session picker、`continue`、`switch`、`new context` 命令。启动时通过 `--session-id`、`--resume` 或等价参数建立的绑定，只能证明**启动时**的 Session identity；不能自动证明整个进程生命周期内 TUI 始终使用同一个 native Session。

因此每个支持交互式上下文切换的 provider 都必须实现类似的运行时 rebind 机制。目标约束是：

```text
一个 Agora managed Session
  ↔ 当前 Agent context 的一个 native Session
  ↔ 当前 context 对应的 history locator
```

这不是 Server 的 provider-specific 协议，而是 Daemon/Agent Runtime 内部的状态机。Server 只接收 rebind 后的规范化 Session metadata 和事件。

#### 触发、确认与提交

rebind 分为三个阶段，不能在发现切换命令时立即改写绑定：

```text
bound(A)
  └─ context-switch trigger（例如 /resume）
       ↓
resume_pending(A)
       └─ 捕获后续用户输入 + 扫描 history 增量
            ↓
rebind_candidate(B)
       └─ 证据满足 provider 策略后原子提交
            ↓
bound(B)
```

1. **触发**：由 provider-specific 的输入拦截器、结构化 live event、RPC/API state change 或原生 metadata 变化发现疑似 context switch。对 PTY 只能把用户提交的 `/resume`（而不是正在编辑的半行）作为触发信号；对 RPC/HTTP/ACP 优先使用结构化命令或 state event。触发信号不得改变发往 Agent 的原始输入。
2. **pending**：保存旧 native ID、旧 locator、触发时间，以及所有可搜索 history locator 的基线 cursor（至少包括 byte offset/record ID/size）。此时继续使用旧绑定，但不把任何候选 Session 当成已确认事实。
3. **候选匹配**：捕获切换完成后的第一条或后续用户消息，并读取各候选 history 在基线之后的新增记录。候选应同时满足：
   - native Session ID 与旧 ID 不同；
   - workspace/project 与当前运行上下文一致；
   - history 在触发后出现新增记录，而不是只在旧 transcript 中找到相同文本；
   - 新增 user message 与捕获输入在 provider parser 定义的规范化规则下精确匹配；
   - record timestamp、写入观察时间和输入提交时间处于 provider 配置的有限窗口内；
   - 候选唯一，或最高候选相对第二候选有明确安全间隔。
4. **提交**：确认后停止旧 observer，以目标 history 的当前末尾建立新 cursor，更新 `AgentSessionID`、provider-native ID、history locator、workspace/name 等运行 metadata，再启动新 observer。目标 history 的旧内容由 history API 正常读取，但不得在 rebind 时作为新的 live event 重放。canonical ID 包含 native ID 时，必须通过 runtime 的原子 rekey/route update 保持 Agora ID 与 native ID 一致；不能只更新 `HistoryPath`。

默认不依赖单一证据。history 内容是主要确认依据，文件 mtime/size 只是辅助排序，TUI 屏幕文本只作为辅助信号。PTY 输入方向优先于从 ANSI/VT 输出中反向提取消息；输入、prompt 和 tool 内容只在内存中短暂保留用于匹配，不进入普通日志或 Server 持久化。

#### 不确定与失败处理

- 只发现 `/resume` 而没有后续可匹配的消息时，保持 `resume_pending`，不自动切换；可设置有界超时，超时后回到 `bound(A)` 并记录 observation warning。
- 候选为零、候选不唯一、workspace 不一致或时间窗口不满足时，不 rebind；应上报可解释的 `session_rebind_pending`/`session_rebind_ambiguous` 状态，而不是猜测。
- 候选确认后如果目标 history 无法读取，保留旧绑定并报告错误；不得先删除旧 locator。
- rebind 必须幂等。重复扫描同一追加记录不能重复发布事件，也不能重复 rekey；cursor 和 stable external ID 仍是去重事实。
- provider 如果能直接提供“当前 context native ID”的结构化状态，应优先使用该状态确认，消息/history 推断只作为 fallback 或交叉校验。

#### Provider 接入要求

| Provider 能力 | 实现要求 |
|---|---|
| 启动时可指定 native ID | 使用明确的 `--session-id`、`--resume` 或等价参数建立初始绑定，并保存 locator/cursor 基线 |
| 支持 TUI 内部切换 | 提供输入/屏幕触发检测，并用后续输入与 history 增量确认；不能只依赖 `/resume` 字符串 |
| 支持结构化 RPC/HTTP/ACP | 优先消费 context/session state change event；保留同样的 pending、确认和原子提交语义 |
| 可读取追加式 history | 按 locator/cursor 扫描切换后的新增记录，使用 provider parser 生成统一 `event.Event` |
| 不能可靠识别当前 context | 不声明自动 rebind；进入 pending/需人工确认或限制该 TUI 命令，不能伪造 identity |
| 无可切换 context | 明确声明 provider contract 不支持 rebind，并保证单个 process 的 identity 不会悄悄变化 |

新增 provider 的验收必须至少覆盖：检测 context-switch trigger、相同内容的时间窗口排除、无后续输入、多个候选、history 延迟写入、目标 history 初始旧记录不重放、rebind 后继续发送输入和 observer 重启去重。

## 3. 接入模式

### 3.1 Managed process

Agora Daemon 启动并持有 Agent 子进程，负责生命周期、输入和观察。Claude PTY 与 Pi RPC 都属于这一类，但 transport 不同。

### 3.2 External/history-only

Session 由用户或其他工具启动，Agora 只通过 history catalog 或已有 provider server 发现和读取。不能控制的能力必须为 false；`CanReadHistory` 不推导出 `CanSendInput` 或 `CanResume`。

## 3.3 Session Host

Managed Agent 由独立的 per-session `session-host` 持有。Daemon 通过认证的 control/attach socket 连接 Host；Daemon 重启、升级或短暂断线不等于 Agent 停止。详见 [`docs/session-host.md`](session-host.md)。

## 4. 能力与降级

`session.Capabilities` 是跨 Agent 的 UI/协议能力矩阵：

| 能力 | 含义 |
|---|---|
| `CanStart` | Agora 能通过 provider driver 创建新 Session |
| `CanDiscover` | 能从本地/远端事实源发现历史 Session |
| `CanAttach` | 能接入原生交互表面，例如 PTY/ACP session |
| `CanObserve` | 能读取至少一种规范化事件源 |
| `CanSendInput` | 能把明确的用户消息送入当前 Session |
| `CanStream` | 能接收实时增量或实时事件 |
| `CanInterrupt` | 能中断正在运行的操作 |
| `CanResume` | 能用原生 Session identity 恢复上下文 |
| `CanApprove` | 有经过验证的结构化审批协议；启动时 `--approve` 不等于此能力 |
| `CanReadHistory` | 能按需读取 provider 的本地/远端历史 |
| `CanReadTerminal` | 能提供只读终端/屏幕快照 |

能力声明必须按“provider + transport + version + 当前运行模式”计算。UI 只展示真实能力；不支持的操作返回可解释错误。

### 会话命名的唯一写入方

会话名称只由 Server 侧决定并持久化（`display_name` + `display_name_source`），优先级固定为：

```text
custom（/name 或显式重命名） > ai_title > first_user > initial（"New session" 等占位名）
```

补齐（enrichment）只能填充**还没有信息量**的名字：空值、占位名、不透明的旧格式名，以及旧版本
用 workspace 目录名当子项名的行。已经携带 `custom`/`ai_title`/`first_user` 的名字不会被覆盖。

客户端**不得**自己推导会话名。Web UI 只加载会话的一个事件窗口（默认最近 60 条），窗口里的
"第一条用户消息"通常是**最近**一条消息，用它命名会与服务端从完整 transcript 得到的名字冲突；
两边各按自己的定时器重新应用（history 每 2s、state 每 5s），用户就会看到名称来回跳。

能力还必须反映**当前运行态**，而不是 provider 的理论能力。典型例子：`CanReadHistory` 需要
provider 的 transcript 已经存在——Agora 新建的会话在 provider 写入第一条消息前没有 JSONL，
context switch 也可能指向尚未落盘的目标会话。这种情况下必须声明 `CanReadHistory: false`，
UI 显示"该会话还没有历史记录"，而不是一个看起来像历史丢失的空对话。

## 5. 现有实现评估

### 5.1 已经可以复用

- `internal/session/session.go`：`Agent`、`AgentSessionID`、canonical session URI 已支持多 Agent；
- `internal/session/capabilities.go`：能力矩阵已是 provider-neutral；
- `internal/event/event.go`：规范化事件模型已能承载 text、thinking、tool、result、error；
- `internal/protocol/protocol.go`：Session payload 已带 `agent`、`agent_session_id` 和通用 Event batch/history 载荷；
- Server relay、SSE、notification policy、Web 类型只应消费规范化模型，无需理解 Pi/Claude 原始协议。

### 5.2 当前 Claude-specific 耦合

以下耦合是真实存在的迁移点，不应在文档中假装已经抽象：

- `runtime.Manager` 持有 `*adapter.ClaudeCodeAdapter` 和 provider 的 argv/env 构造器
  （`ClaudeProvider` / `PiProvider`）；Agent 进程一律由 Session Host 持有，
  Manager 只管理 Session 记录、observer 和 host 注册表；
- `adapter/discovery.go` 的 catalog、`FindHistoryBySessionID` 和 `ParseHistoryEvent` 写死 `~/.claude/projects`、Claude 文件名和字段；
- `adapter/stream_json.go` 解析 Claude stream-json envelope；
- `daemon.Config` 只有 `ClaudeBinary`；
- daemon session.create/resume 和 `runtime.Manager.ResumeSession` 的默认启动/恢复路径仍按 Claude 处理；
- attach、snapshot、raw terminal frame 是 PTY 独有能力。

### 5.3 推荐渐进迁移

1. 先定义并测试 `ProviderDescriptor`、history reader/catalog、history/live parser、`SessionDriver` 的最小边界；
2. 把现有 Claude 实现包进这些边界，保持 Claude 行为和现有 fixture 不变；
3. 增加 Pi RPC driver、Pi history reader 和两个 Pi parser；
4. 将 Manager 从具体 Claude 类型改为 provider registry/factory；
5. 最后把 PTY attach/snapshot 从通用 Session runtime 中隔离为可选 `TerminalSurface`。

不建议先把所有代码重写为接口。应先用 Claude adapter 完成接口的兼容包装，再用 Pi 验证抽象是否真实覆盖不同 transport。

## 6. Provider 对比与复用边界

| Provider | Live | History | Control | Terminal | 首要 provider-specific 工作 |
|---|---|---|---|---|---|
| Claude Code | PTY/stream-json | JSONL | PTY + `--resume` | 有 | Claude parser、PTY、环境隔离 |
| Pi | `--mode json` / RPC events | 追加 JSONL | stdio RPC | 无（默认） | Pi event parser、session locator、RPC driver |
| OpenCode | `run --format json` / HTTP/ACP | SQLite/export | HTTP/ACP | 可选 | SQLite history reader、HTTP/ACP driver |
| Codex | 待实测 | 待实测 | 待实测 | 待实测 | 以真实探测结果为准 |

通用层只抽象能力和规范化数据；provider-specific 的命令行参数、目录布局、事件字段、认证和审批语义保留在 provider 包内。

## 7. 兼容性与安全

- provider 版本必须在 Daemon 启动或创建 Session 时探测并记录；未知版本进入降级或拒绝策略；
- 原始命令参数、token、prompt、完整 tool input/output 不进入普通日志或 Server 控制面持久化；
- Agent 退出码、RPC 错误和 parser 错误分开记录；parser 错误不能伪装成 Agent 失败；
- `RawJSON` 只在受控观察路径使用，通知层只发送短摘要；
- Agent 原生审批能力必须有结构化、可关联、可审计的协议，不能把 `--approve`、终端画面或普通文本当成 `CanApprove`；
- Server–Daemon wire schema 保持 provider-neutral，不新增 `pi.*` 或 `claude.*` 消息类型；provider 原始消息留在 Daemon 内部。

## 8. 验收标准

- 同一个 `event.Event` consumer 可以消费 Claude 和 Pi 的 user/assistant/tool/result/error 事件；
- Pi 的 RPC 输入不依赖 PTY，且 `prompt`、`abort` 的响应与异步事件不会混淆；
- Pi history observer 重启后可以从 cursor 继续读取，不重复发布 final message；
- provider 不支持的能力在 Session 中为 false，并得到明确错误；
- OpenCode 后续可以复用 Session/Event/Capabilities/Server relay，而不需要修改 Web 或通知协议；
- 真实 CLI fixture、parser 单测、driver transport 测试和最小端到端测试分层存在。
