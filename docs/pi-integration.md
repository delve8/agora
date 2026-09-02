# Pi Integration

> 状态：已实现 PTY/TUI 托管（Web snapshot + native attach）
>
> 本文记录本机 Pi coding agent 的接口探测结果和 Agora 对接方案。Pi 的 Web/RPC 设计已经替换为 Agora Daemon 真实 PTY 托管；JSONL history 仍是 Web 消息的权威来源。
>
> 实测版本：Pi `0.84.2`，npm 包 `@earendil-works/pi-coding-agent`。Pi 版本升级后必须重新检查 CLI help、RPC 文档和事件 fixture；不要把某一版本的事件字段当成永久稳定 API。

## 1. 结论

Pi 是 Agora 首个非 Claude Agent 的推荐接入对象。它不应被套进 Claude 的 PTY 模型，而应作为：

```text
Pi managed process
  ├── control/input: Agora-owned PTY（普通 Pi TUI）
  ├── live screen: VT emulator -> Web /pty/snapshot
  ├── native attach: Unix socket -> agora attach / pi-wrapper
  ├── history: 本地追加式 session JSONL -> Web/SSE
  └── lifecycle: Pi TUI 与 RPC 模式互斥
```

Pi 和 Agora 现有设计的匹配点：

- session history 是追加式 JSONL，适合 byte offset/line cursor；
- `--mode json` 是结构化事件流，适合旁路观察；
- `--mode rpc` 提供 prompt、steer、follow-up、abort 和状态查询，输入不需要伪装成键盘字符；
- 原生支持 Anthropic provider，也可使用其他 provider，因此 Pi adapter 不绑定单一模型厂商；
- `--resume`、`--continue`、`--session-id`、`--fork` 提供明确的会话恢复/分叉语义。

## 2. CLI 与认证

### 2.1 启动参数

`pi-wrapper`/`agora wrap pi` 会把所有 Pi 参数按原顺序、原字符串直接传给真实 Pi，不解析、不过滤、不重写：

```text
pi --session <id> --provider anthropic --model <provider-model> ...
```

因此 Pi 的版本新增参数也可以直接使用。Agora 只会将 binary 放在参数列表最前面，并通过 PTY 建立 attach；参数含义和运行模式完全由 Pi 决定。传入 `--print`、`--mode json` 或 `--mode rpc` 时，Pi 会按自身语义运行，可能不会显示 TUI，这是预期行为。

非交互观察或一次性运行可以使用：

```text
pi --provider anthropic --model <provider-model> --mode json ...
```

Pi 的默认 provider 可能是 Google，当前机器的默认 model 也可能是 DeepSeek。默认值属于 Pi 用户配置，不是 Agora 的稳定协议，因此必须由 Agora 配置或 SessionCreate 请求明确传入。

推荐 Daemon 配置字段：

```text
agent: pi
binary: pi
provider: anthropic
model: claude-opus-5 / claude-sonnet-5 / user-selected model
thinking: provider-supported level
session_dir: optional explicit Pi session directory
```

### 2.2 认证边界

实测 `pi auth check --provider anthropic` 可以报告 provider ready。Agora 不读取或打印 credential 内容，也不把 token 放入：

- Server–Daemon protocol payload；
- session metadata；
- 普通日志、事件 RawJSON 或 IM 通知；
- 命令参数摘要。

Pi 负责 provider credential 的解析和刷新；Agora 只负责选择 provider/model，并在启动失败时报告不含 secret 的错误摘要。若未来 Daemon 需要显式注入 credential，应使用本地 secrets/环境继承边界，不写入 Agora 的业务数据模型。

### 2.3 Project trust 与 approval 的区别

Pi 的 `--approve`/`--no-approve` 和 `defaultProjectTrust` 是启动时的项目资源信任设置。它们不构成逐次工具调用的审批协议，不能据此声明：

```text
CanApprove = true
```

除非后续实测 Pi RPC 或 extension 暴露了可关联、可等待、可 allow/deny、可审计的结构化审批事件。第一版保持 `CanApprove=false`。

## 3. Session identity 与生命周期

### 3.1 原生 identity

Pi session header 提供 UUID/session ID，并写入 session JSONL。Agora 使用：

```text
AgentSessionID = pi://<pi-session-id>
AgoraID        = daemon/<daemon-id>/pi://<pi-session-id>
```

Pi 的 session 文件路径是 provider-owned locator，不应从 UUID 直接拼出而不验证。实测路径形态为：

```text
~/.pi/agent/sessions/<project-key>/<timestamp>_<uuid>.jsonl
```

其中 project key、时间戳和 UUID 都是 Pi 的本地存储约定；适配器应通过 session catalog/文件扫描或 Pi 的 session 选项定位，而不是把路径格式散落到 runtime Manager。

### 3.2 Start

Start 的目标是启动一个长驻 Pi RPC 子进程，并在建立 RPC 通道后获取 state：

```text
Agora Daemon
    │ spawn pi --mode rpc --provider ... --model ... --session-dir ...
    │
    ├── stdin  ← RPC commands
    └── stdout → RPC responses + async events
```

创建新 session 后，driver 从 `get_state` 或 session-start event 获取 native session ID，再写入 `AgentSessionID` 和 `HistoryLocator`。如果 Pi 在启动时尚未创建 session 文件，不能把临时 PID 当作永久 AgentSessionID。

### 3.3 Resume / continue / fork

支持路径：

| 用户意图 | Pi 入口 | Agora 语义 |
|---|---|---|
| 恢复指定会话 | `--resume` 或 `--session` | `CanResume`，保持同一个 `pi://id` |
| 继续最近会话 | `--continue` | 只适合显式“最近会话”语义，不应替代 canonical ID |
| 指定精确 ID | `--session-id` | 适合从 Agora Session identity 启动/恢复 |
| 从会话分叉 | `--fork` | 创建新的 AgentSessionID，并保留 parent metadata |
| RPC 内新会话 | `new_session` | 当前 driver session 内切换/创建新上下文，必须重新读取 state |

Agora 的 resume API 以 canonical Session ID 为准；Daemon 解析 `pi://`，再将 native ID/path 交给 Pi driver。不能把 `--continue` 的“最近”行为作为远程恢复的唯一依据。

### 3.4 TUI 内部 `/resume` 与运行时 rebind

`--session-id <id>` 能准确建立 Pi 进程**启动时**的初始绑定，但不能单独保证 TUI 运行期间不会通过 `/resume` 或未来的 session picker 切换上下文。Agora 必须把这类切换视为 provider context rebind，而不是普通 user message。

Pi adapter 的目标流程是：

```text
pi://A + A.jsonl
      │
      │ 检测到已提交的 /resume
      ▼
resume_pending
      │
      │ 记录时间和各 session history 的 cursor/size 基线
      │ 观察后续用户输入
      ▼
扫描 Pi history 增量
      │
      │ 在时间窗口内找到另一个 workspace 相同的 pi://B，
      │ 且 B 的新增 user message 与后续输入匹配
      ▼
原子 rebind 到 pi://B + B.jsonl
```

具体要求：

- PTY 输入方向优先识别用户**提交**的 `/resume`，不能仅搜索 TUI 输出中的字符串；原始字节仍必须原样发送给 Pi；
- 识别 `/resume` 后不立即更新 `AgentSessionID` 或 `HistoryPath`，先保存旧绑定和所有候选文件的 byte offset/record ID/size，进入 `resume_pending`；
- 后续消息优先从 PTY 输入捕获，并与 `/resume` 之后各 JSONL 的新增 user record 匹配。匹配必须使用增量 cursor，不能因为旧 transcript 中存在同样文字就切换；
- 使用 workspace、native session ID、输入提交时间、history record timestamp 和本地文件写入时间作为联合约束。重复内容只有在时间窗口和新增记录条件同时满足时才算候选；
- 若只有一个明确候选，停止旧 observer，以目标文件末尾初始化新 cursor，更新 `AgentSessionID=pi://B`、`HistoryPath` 和运行 metadata，再启动新 observer；目标文件已有历史不能作为新 live event 重放；
- canonical Agora ID 包含 native ID 时必须原子 rekey/更新 route，不能只修改 history path；
- 没有后续输入、候选不唯一、history 延迟写入或无法确认时保持 pending/需人工确认，不猜测。Pi 如果能从 RPC `get_state` 或 context event 得到当前 native ID，应优先使用结构化状态确认，并用 history 匹配做校验；
- rebind 后的扫描必须幂等，依靠 cursor 和稳定 external ID 去重。

Pi adapter 的测试至少覆盖：`--session-id` 初始绑定、`/resume` 触发、不同 session 在时间窗口外的相同消息、history 延迟 flush、目标文件旧记录不重放、无后续消息、多个候选、rebind 后输入和 observer 重启恢复。

该流程不是 Pi 独有的产品语义。Claude、OpenCode、Codex 或其他支持交互式 context switch 的 provider 也必须提供等价的 trigger、候选确认和原子 rebind；没有可靠状态或 history 证据的 provider 不得伪造 native identity。

## 4. History 接入

### 4.1 文件事实源

Pi session history 是追加式 JSONL：

```text
session header
session/model/thinking metadata entries
user/assistant/tool messages
compaction/session tree/runtime entries
```

首行包含 session 版本、ID、时间戳和 cwd 等元数据；后续记录按行追加。适配器应保留未知 entry，以便 Pi 新版本扩展时不阻塞读取。

### 4.2 HistoryLocator

Pi history reader 的 locator 建议使用：

```go
type PiHistoryLocator struct {
    Path        string
    SessionID   string
    ProjectKey  string
    Workspace   string
}
```

通用 runtime 只保存序列化 locator 和 cursor：

```text
path + byte_offset + line + last_external_id
```

读取规则：

1. 打开 locator 指向文件；
2. 如果文件大小小于 cursor offset，视为 truncate/rotate，cursor 回到 0；
3. 从 byte offset 读取完整 LF 记录；
4. 最后一个记录不完整时保留 offset，不把半行解析成事件；
5. 每条记录解析出稳定 external ID 后推进 cursor；
6. 只保存纳管 Session 的 cursor，不把 transcript 扫描并同步到 Server；Daemon 可以持续扫描本地 catalog，并仅上报新增/变化会话的 summary 和 locator。

Pi 的 session catalog 用于发现本机 `~/.pi/agent/sessions` 下的有效会话；新建或外部启动的 Pi 会话在下一次 catalog 刷新后通过 Daemon resync 出现在 Server 的会话列表中。Server 不接收或持久化 Pi transcript，完整历史仍由 Daemon 从本地 JSONL 按需读取。

### 4.3 History parser 映射

Pi history parser 以 entry/message 类型为 provider-specific 输入，输出统一 `event.Event`：

| Pi 内容 | Event |
|---|---|
| user message | `KindUser`, `Type=text`, `Role=user` |
| assistant text | `KindAssistant`, `Type=text`, `Role=assistant` |
| assistant thinking/reasoning summary | `KindAssistant`, `Type=thinking`, `Role=assistant`, `Thinking` |
| tool call | `KindTool`, `Type=tool_call`, `Role=tool`, `ToolName/ToolInput` |
| tool result | `KindTool`, `Type=tool_result`, `Role=tool`, `ToolOutput` |
| session/compaction metadata | `KindSystem` 或降级 metadata event |
| provider/error record | `KindError`, `IsError=true` |

具体 JSON 字段必须以真实 fixture 和当前 Pi 类型定义为准；没有确认的字段不能在通用协议中硬编码。

## 5. Live observation

Pi 提供两种结构化观察路径。

### 5.1 `--mode json`

```text
pi --provider anthropic --model <model> --mode json "<prompt>"
```

stdout 是 JSON lines。实测/文档确认的事件类别包括：

```text
session
agent_start
agent_end
turn_start
turn_end
message_start
message_update
message_end
tool_execution_start
tool_execution_update
tool_execution_end
queue_update
compaction_start
compaction_end
```

`message_update` 是 delta-only 事件，包含当前增量和 usage；它不带可直接替代最终消息的完整 cumulative snapshot。`message_end` 是最终 authoritative message。Agora 实现必须选择一种发布策略：

- 默认只把 delta 作为 live preview，把 `message_end` 作为最终事件并按关联 ID reconcile；或
- 只发布 `message_end`，牺牲逐 token 展示换取简单去重。

不能把每个 delta 和最终 message 都当成独立完整 assistant event，否则 Web/IM 会重复显示文本。

### 5.2 RPC 异步 events

`--mode rpc` 的 stdout 同时包含：

- command response：`type=response`，带 `command`、`success` 和可选 `id`；
- agent async events：事件类型由 Pi RPC 协议定义，不一定带 command ID。

Agora 必须先按顶层 `type` 分流：

```text
response → 请求关联器 / command result
其他     → live event parser / state machine
```

不能把 response 当作 Agent transcript，也不能把异步 event 等同于某个请求的成功响应。

RPC 事件可直接映射为 live observation；同一进程不得同时启用多个 observer 重复消费同一条事件。Pi 的控制和观察应由同一个 managed driver 或 Session Host 管理。

### 5.3 framing 与缓冲

Pi RPC 要求严格 LF-delimited JSONL：

- 只把 `\n` 当作 record delimiter；
- 可接受输入末尾的 `\r`；
- 不要使用会把 Unicode line separator 当作分隔符的通用 reader；
- 单行缓冲设置上限；
- 超长或非法记录只记录有限诊断，不阻塞 stdout/stderr relay；
- stdout 的 response/event 读取与 stdin command 写入必须有独立并发控制。

## 6. RPC 控制接口映射

### 6.1 第一版最小命令

| Agora 操作 | Pi RPC command | 结果/事件 |
|---|---|---|
| send input | `prompt` | response 表示接受/排队；后续 agent events 表示实际执行 |
| steer running turn | `steer` | 当前 turn 工具执行完成后注入 |
| queue follow-up | `follow_up` | 当前 Agent 完成后处理 |
| interrupt | `abort` | response + 后续 settled/end 状态 |
| fresh context | `new_session` | response data + 新 state/session identity |
| query state | `get_state` | model、session ID、streaming、queue 等 |
| read transcript | `get_messages` | 当前进程内消息视图；完整历史仍以 JSONL 为事实源 |
| switch model | `set_model` | 返回 model；是否允许远程切换需单独授权 |
| set reasoning | `set_thinking_level` | 返回 accepted；按 provider/model 能力校验 |
| compact | `compact` | response data + compaction events |

`get_state` 的 `isStreaming`、`isCompacting`、`pendingMessageCount` 只用于辅助状态，不替代 `agent_settled` 或最终 message/turn event。

### 6.2 请求关联与回合完成

每个可以关联的 command 都带随机 `id`：

```json
{"id":"req-1","type":"prompt","message":"Review this file"}
```

对应 response：

```json
{"id":"req-1","type":"response","command":"prompt","success":true}
```

`success=true` 只说明 prompt 被接受、排队或立即处理，不说明 Agent 已完成。真正的完成判断依赖：

- `agent_settled`；
- `agent_end`/`turn_end`；
- session state 中 `isStreaming=false`；
- driver 进程退出或显式 error。

Agora 的 `Session.State` 应由 runtime 状态机综合这些信号，不能在收到 prompt response 后立即设置为 waiting。

### 6.3 并发与输入仲裁

- Agent streaming 时，普通 `prompt` 必须显式选择 `steer` 或 `followUp` 语义；
- Agora 一次只允许一个未决的 direct control command，除非 driver 明确支持排队；
- `steer`/`follow_up` 是不同的输入策略，不能都映射成无序的 `prompt`；
- `abort` 是控制信号，不应进入 user message history；
- Web、终端（如果未来添加）和 IM 输入必须在 Daemon 侧统一串行化；
- RPC 进程退出时，所有 pending command 标记 failed，Session 进入 stopped/failed，不能把 pipe EOF 当作正常回合结束。

## 7. Pi 能力矩阵

第一版 Pi RPC managed session 建议：

| 能力 | 值 | 说明 |
|---|---:|---|
| `CanStart` | true | Agora 能 spawn Pi |
| `CanDiscover` | true | 能扫描/定位 Pi session JSONL，但只纳管显式工作区 |
| `CanAttach` | false | 默认无 PTY/TUI attach；RPC attach 不等同 terminal attach |
| `CanObserve` | true | RPC events 或 `--mode json` |
| `CanSendInput` | true | RPC `prompt`/`steer`/`follow_up` |
| `CanStream` | true | message/tool/agent events |
| `CanInterrupt` | true | RPC `abort` |
| `CanResume` | true | `--resume`/`--session-id`/session locator |
| `CanApprove` | false | `--approve` 是启动级信任，不是结构化审批 |
| `CanReadHistory` | true | 追加式 JSONL |
| `CanReadTerminal` | true（运行中） | Pi PTY 的 VT emulator 提供只读屏幕 snapshot；停止会话不可读取 |

能力必须随着 Pi 版本、启动模式和配置重新计算。例如 `--no-session` 会关闭 history/resume；若只连接 `--mode json` 而不保留进程控制，则 `CanSendInput`/`CanInterrupt` 不应宣称为 true。

## 8. 第一阶段实现方案

### Phase P0：只读 parser 与 fixture

不启动真实模型调用，先把已采集的 Pi JSONL 作为 fixture：

- `ParsePiHistoryEvent`；
- `ParsePiJSONEvent`；
- delta/final reconcile 测试；
- unknown event、非法 JSON、半行和超长行测试；
- stable event ID 和 cursor 测试。

### Phase P1：RPC transport smoke test

使用本地 `pi --mode rpc --no-session` 做无模型状态测试：

- `get_state`；
- `get_messages`；
- `get_available_models`；
- response ID 关联；
- stdout response/event 分流；
- EOF、stderr、进程退出处理。

### Phase P2：单 Session managed loop

- Daemon 显式以 `--provider`、`--model` 启动 Pi RPC；
- 创建 Agora Session 并获取 `pi://id`；
- `prompt` → events → `agent_settled`；
- Web `session.input` 映射到 RPC `prompt`；
- Web `session.stop`/interrupt 映射到 `abort`；
- server/daemon/UI 继续使用现有规范化协议。

### Phase P3：history/resume

- session locator/cursor 持久化；
- Agora restart 后按 canonical ID 重新 spawn `--resume`/`--session-id`；
- history 和 live stream 去重；
- Pi process exit 后保留本地 history，Session 进入 stopped；
- `--fork` 作为明确的新 Session 能力，不能覆盖父 Session ID。

### Phase P4：多 provider registry

- 将 runtime Manager 从 `*adapter.ClaudeCodeAdapter` 改为 provider registry；
- Claude 通过兼容 wrapper 接入；
- Pi 使用 RPC driver；
- OpenCode 复用 normalized model，但实现自己的 HTTP/ACP driver 和 SQLite history reader；
- 不修改 Server–Daemon wire schema。

## 9. 错误与降级

| 情况 | 行为 |
|---|---|
| Pi binary 不存在 | 创建失败，返回 provider/binary 错误 |
| provider credential 不可用 | Session failed，错误摘要不得包含 token |
| RPC response `success=false` | 当前 command failed，Session 是否继续由命令类型决定 |
| prompt 已接受但后续 Agent error | 通过 async event/settled error 上报，不伪造 sent completion |
| JSON parser 失败 | 记录有限诊断；主进程继续；必要时标记 observation gap |
| history 文件暂不存在 | Session 仍可 running；等待 Pi 创建文件并重新定位 |
| history 文件 rotate/truncate | cursor reset，并上报 gap/重新发现 |
| RPC stdout EOF | driver 结束；pending commands failed；记录 process exit |
| Agora/Server 断线 | Pi 本地继续运行；有限 outbox/重连后按 cursor 或 stable ID 补发 |

## 10. 安全边界

- 不默认启用 `--approve`；如果产品提供该选项，必须在 Session metadata 和 UI 中明确显示其风险；
- 不把普通 Pi 文本、`steer` 或 `follow_up` 当审批命令；
- RPC command 是可执行控制面，必须沿用 Agora 现有 `principal → device → session` 授权链；notification capability link 只能观察，不能发送 prompt/abort；
- 原始 RPC payload、tool args、provider credential 和 workspace 私密路径不写入 Server 持久化或普通日志；
- 事件 `RawJSON` 只在本地观察/调试路径保留，IM 只使用聚合摘要；
- Pi extension、skills、项目上下文文件属于 Agent 的执行输入，纳管前应按 Pi 的 project trust 和 Agora workspace 边界处理；
- 不因为 Pi 能运行 bash/edit/write 就自动给用户或 IM 增加审批/控制权限。

## 11. 与其他 Agent 的复用

| Pi 经验 | 后续复用 |
|---|---|
| RPC response 与 async event 分流 | OpenCode HTTP/ACP response 与 event stream 也应分离 |
| delta-only + final authoritative | Claude stream-json 或其他增量协议的统一 reconcile 规则 |
| provider-owned history locator | OpenCode SQLite locator、Codex history locator |
| `prompt`/`steer`/`follow_up` 语义分离 | 通用 input queue/interrupt 模型 |
| 不把 terminal 当默认 surface | OpenCode server、Codex exec 等非 TUI Agent |
| provider/model 显式配置 | 所有 Agent 的版本和模型可复现启动 |

Pi 的成功标准不是让所有 Agent 都实现相同命令，而是验证 Agora 的通用层能否只依赖：

```text
Session identity
Capabilities
Normalized events
Input/interrupt result
History cursor
Process/connection state
```

## 12. 参考实测记录

2026-08-18，本机验证：

- `pi --version` → `0.84.2`；
- `pi --help` 暴露 `--mode text|json|rpc`、`--resume`、`--continue`、`--session-id`、`--fork`、`--session-dir`、`--provider`、`--model`；
- `pi auth check --provider anthropic --json --no-refresh` → provider ready；
- `pi --mode rpc --no-session --no-extensions --no-skills --provider anthropic` 可处理 `get_state`、`get_messages`、`get_available_models`，返回带 request ID 的 JSON response；
- `get_available_models` 返回 Anthropic、DeepSeek 等 provider/model 元数据；
- Pi 包内 `docs/json.md`、`docs/rpc.md` 明确 JSONL 事件和严格 LF framing；
- session 文件位于 `~/.pi/agent/sessions` 下的项目目录，首行是 versioned session header，后续记录追加写入。

这些是 0.84.2 的探测记录，不是对未来 Pi 版本的永久承诺。实现时应把 CLI version、help snapshot、event fixture 和 parser contract 纳入测试。
