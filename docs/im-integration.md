# Agora IM 集成规范

> 状态：Draft / notification-entry design
>
> 本文定义 IM 在 Agora 中的第一阶段定位和边界。它描述产品行为与安全约束，不代表已经实现了任何具体 IM SDK、Webhook、认证服务或远程访问网关。

## 1. 目标与核心决策

Agora 的完整观察界面是 Web UI。IM 不复制 Agora 的 transcript、PTY 画面或每一条事件，而是承担两种职责：

1. **通知**：在 Session 需要用户关注时发送低噪音、可聚合的状态摘要；
2. **入口**：提供一个链接，让用户打开 Agora Web 查看指定 Session 的完整上下文。

核心分工如下：

```text
IM                 = 通知和打开入口
Agora Web          = 完整历史、事件流、PTY screen snapshot 和普通 Session 输入
原生 Claude 终端   = Claude Code 原生 TUI，以及当前仍需 terminal-only 的审批操作
```

这不是把 Agora 变成 IM transcript mirror，也不是把 IM 变成远程终端控制器。

## 2. 适用范围

### 2.1 第一阶段目标

第一阶段的 IM 集成只要求：

- 为一个 Coordination 或其中的 Session 配置出站通知目标；
- 在高价值状态变化时发送一条简短通知；
- 通知能标识 Coordination、Session、状态和最近动作；
- 通知包含打开 Agora Web Session 详情页的链接；
- 用户打开链接后，可以使用现有 Web UI 查看 JSONL/history 和只读 PTY snapshot；
- 通知失败、重复和暂时不可达时，不破坏 Session、历史事件或 Web UI。

第一阶段可以先实现 generic webhook，再接入一个真实 IM provider。provider-specific 的卡片、按钮和富文本格式属于 adapter 层，不应进入核心 Session 模型。

### 2.2 非目标

第一阶段不做：

- 在 IM 中同步每条 assistant token、user message、tool_use 或 tool_result；
- 在 IM 中渲染 ANSI/VT 控制序列、完整 terminal snapshot 或 PTY raw bytes；
- 在 IM 中保存 Agora transcript 的镜像副本；
- 把每次轮询、每个 JSONL event 或每次屏幕刷新都推送为消息；
- 在 IM 中提供 approve、reject、always allow 等审批按钮；
- 将 `yes`、`no`、`y`、`n`、Enter、Esc 或任意普通 IM 文本解释为 Claude permission prompt 的按键；
- 通过通知链接绕过 Web 认证或获得完整控制权限；
- 在本阶段设计可靠队列、分布式消息总线、复杂重试或多租户权限系统；
- 把出站通知与发往 Session 的 `Message` 记录混为同一种数据。

未来如果需要 IM 双向输入，必须作为独立的输入权限和投递设计评审，不能从本通知规范中推导出审批控制能力。

## 3. 用户流程

### 3.1 普通关注

```text
Session 状态/attention 发生值得关注的变化
        │
        ▼
通知策略聚合并去重
        │
        ▼
IM 发送简短摘要 + Session deep link
        │
        ▼
用户点击链接
        │
        ▼
Agora Web 加载该 Session 的 history、SSE 和 PTY snapshot
```

IM 消息只表达“有一个 Session 需要查看”，不试图在消息内重建页面。

### 3.2 HIL/permission prompt

当 Agora 能从 PTY snapshot 稳定识别一个 presentation-level 的 Claude permission prompt 时，通知可以写成：

```text
Agora · 需要人工关注
Session: weather-fetcher
动作: Claude 正在等待原生终端 permission approval
工具: WebFetch
目标: wttr.in

审批仍需在原生 Claude 终端完成。Agora Web 当前只提供只读观察。
打开 Agora 查看：<session deep link>
```

该通知不能包含“允许/拒绝”操作，也不能因为用户点击链接而改变 `can_approve`。如果 prompt 识别不可靠，系统应退化为普通 Session 状态或不发送 HIL 通知，而不是猜测审批状态。

### 3.3 失败、退出和恢复

建议首期关注以下状态转换：

```text
running → attention_required
running → failed
running → stopped/exited
attention_required → running/resumed
running → completed（如果 Adapter 能可靠识别）
```

只在状态转换或聚合窗口结束时通知。不要因为同一个状态被轮询多次而重复发送。

## 4. 通知模型

通知是对 Session 事件或 attention 状态的外部呈现，不是新的 Agent 输入。核心层可以使用类似如下的内部模型：

```go
type SessionNotification struct {
    ID           string
    CoordinationID string
    SessionID    string
    SessionName  string
    State        string
    Attention    string
    Title        string
    Summary      string
    Tool         string
    Target       string
    OpenURL      string
    CreatedAt    time.Time
}

type Notifier interface {
    NotifySessionEvent(ctx context.Context, notification SessionNotification) error
}
```

字段可以在实现时调整，但必须保留：

- 明确的 Coordination 和 Session 标识；
- 规范化状态或 attention 类型；
- 面向人的短标题和摘要；
- 可选的工具/目标摘要，而不是完整参数和原始 JSON；
- 指向指定 Session 的 Web 链接；
- 生成时间和可用于去重的稳定通知 ID。

`Notifier` 只负责向外部目标投递；它不读取 PTY、不判断审批、不写 PTY、不创建 Message，也不决定 Session 是否成功。

### 4.1 Attention 类型

可先使用有限且可扩展的枚举：

```text
none
approval_observed
failed
stopped
completed
resumed
```

`approval_observed` 仅表示 PTY 画面符合已验证的展示模式，不等同于拥有可执行的 approval API。它不改变 Session 的 `Capabilities.CanApprove`，该字段仍必须为 `false`，直到另行完成审批识别、授权和控制设计。

### 4.2 通知策略

默认策略按价值分层：

| 类别 | 示例 | 首期行为 |
|---|---|---|
| 高优先级 | permission/HIL 观察、失败、进程退出 | 发送一条状态变化通知 |
| 中优先级 | 恢复、完成、Session 启动 | 可配置发送 |
| 低优先级 | 普通 assistant 文本、单个工具调用/结果 | 默认不发送 |
| 高频数据 | PTY screen、raw bytes、token、轮询结果 | 永不逐条发送 |

同一个 Session 在短窗口内发生多条低层事件时，应优先合并成一条摘要。通知策略应至少具备：

- 状态转换去重；
- 稳定事件 ID 或 `(session, attention, transition)` 去重键；
- HIL 识别的连续快照确认或 debounce；
- provider 暂时失败时的有限重试或明确记录；
- 不因通知失败而重试 Session 输入或重复执行 Agent 操作。

## 5. Deep link 与访问模型

### 5.1 链接语义

通知链接定位动态 Session，而不是静态复制通知内容：

```text
https://agora.example/sessions/<session-id>
```

可以使用非安全语义的展示参数，例如：

```text
https://agora.example/sessions/<session-id>?focus=terminal
```

`focus=terminal` 只影响 Web UI 初始展示位置，不能授予额外权限，也不能触发键盘输入或审批。

### 5.2 本地运行

当前 Agora 默认监听 `127.0.0.1`。因此：

- `localhost` 链接只适用于 IM 客户端与 Agora 位于同一台机器的场景；
- 手机、另一台电脑或远程 IM 客户端不能依赖远端设备上的 `localhost`；
- 不应仅为了让链接可点击就把本地 API 直接暴露到公网。

跨设备访问应通过 VPN、Tailscale、受控 HTTPS reverse proxy 或后续的 Web gateway，并保持认证和 Session 授权检查。

### 5.3 深链接安全

如果部署需要免登录打开链接，可以使用短期 signed link，但必须满足：

- 绑定指定 Session，不能只绑定 Coordination 或全局 Agora；
- 具有明确的 `read_observation`/只读 scope；
- 有过期时间和不可预测的签名 token；
- 服务端仍检查 Session 是否存在、用户是否有权访问；
- token 不能调用 `POST /messages`、PTY attach 写入、审批或其他控制接口；
- 日志和通知模板避免泄露完整 workspace、秘密参数或 token；
- token 泄露后的撤销/失效策略在真正实现前明确记录。

更推荐普通 Web 登录后使用无秘密的 Session URL。深链接不是身份认证的替代品，也不是控制凭证。

## 6. 与现有信息流的关系

通知路径应与现有观察和输入路径分开：

```text
Claude Code
   ├── JSONL → history observer → Event store/SSE → Web
   ├── PTY → VT emulator → read-only snapshot → Web
   └── normalized state/attention transition → notification policy → IM

Human/Web/未来授权的 IM input → Message → 明确 Session input
```

- JSONL 仍是工具、结果和历史事件的主要来源；
- PTY snapshot 仍是 native TUI 当前画面的观察来源；
- IM notification 只消费归一化的状态/attention 结果，不消费原始 PTY 字节；
- `Message` 仍然表示一个明确发往单个 Session 的人类输入；
- 通知不是 `Message`，不进入 Session 上下文，也不自动广播给其他 Session；
- Web 页面打开后，可以继续使用现有普通消息输入，但这与 HIL 审批严格分离。

## 7. Provider Adapter 约束

核心不应直接依赖钉钉、企业微信、飞书、Slack、Telegram 或其他 provider 的 SDK。建议分层：

```text
Session/Event/attention
        │
        ▼
Notification policy + dedupe
        │
        ▼
Notifier interface
        ├── generic webhook
        ├── provider adapter A
        └── provider adapter B
```

Provider adapter 负责：

- 认证凭据和 endpoint；
- 文本、Markdown 或卡片格式转换；
- provider 的长度限制和速率限制；
- provider message ID、错误和投递结果记录。

一期核心路径是：Daemon 上报规范化状态/attention 和短摘要；Server 负责 provider adapter，并发送用户配置的 webhook 请求。这样飞书、钉钉、企业微信和 generic webhook 都能集中升级，不需要把 provider SDK 放进 Daemon 或核心 Session 模型。

Provider adapter 不负责：

- 解析 Claude TUI；
- 识别审批按键；
- 决定 Session 状态；
- 接收普通文本并写入 PTY；
- 绕过 Web 授权。

## 8. 审计与隐私

在产品化一期，Server 采用“可转发、不可持久化业务内容”的隐私边界：

- Server 可以在 HTTP/WebSocket 转发、SSE fan-out 和 IM payload 构造时短暂看到规范化状态、短摘要和必要的消息内容；
- Server 不持久化完整 transcript、原始 JSONL、Event、Message、PTY snapshot、通知正文或完整工具参数；
- Daemon 只管理 Agora 显式纳管的 Session，完整 Claude 历史继续留在用户电脑的原始 JSONL 文件中；
- Server 只保存设备、配对状态、在线路由和必要的 webhook 配置元数据；
- 普通日志和错误响应不得记录 webhook URL、device credential、RawJSON、PTY 内容或完整消息正文；
- Server 重启后不保证恢复业务历史，由 Daemon 重新连接、注册和按需提供历史。

一期通知运行时可以在内存中关联：

- 触发它的 Session 和 Coordination；
- 规范化状态/attention；
- 通知生成时间和去重 key；
- 目标 provider 和 HTTP 结果；
- 打开的 deep link 所绑定的 Session。

这些信息只用于短期去重、debounce、投递和运维诊断，不形成通知审计历史。若未来需要用户查看完整投递记录，需要重新评估持久化和隐私边界。

通知默认只带最小必要信息。工具参数可能包含 URL、文件路径、用户输入或敏感数据，应由摘要策略截断、脱敏或省略。完整内容留在受保护的 Agora Web 和本地历史中。

## 9. 验收标准

### 通知与入口

- Session 发生配置的高价值状态转换时，IM 收到一条简短通知；
- 同一个状态不会因轮询或重复 JSONL event 产生消息风暴；
- 通知包含可识别的 Session 和可用的 Web deep link；
- 点击链接后能打开指定 Session 的现有 Web 观察页面；
- Web 页面仍显示 history/event stream 和当前 PTY snapshot；
- 通知投递失败不会改变 Session 的运行结果或重复发送 Agent input。

### 安全边界

- IM 消息不包含完整 transcript、PTY raw bytes 或 ANSI/VT 控制序列；
- IM 不提供审批按钮；
- 任意 IM 文本都不会自动解释为 approve/reject 或 PTY 按键；
- HIL 通知不会把 `can_approve` 改为 `true`；
- deep link 不绕过 Web 认证，也不授予写入、attach 或审批权限；
- 通知摘要不会默认泄露完整工具参数、workspace 私密信息或签名 token。

### 降级行为

- 没有可用 Web 地址时，通知应明确标记“需要本机打开 Agora”或不发送不可用链接；
- PTY 无法观察时，不得伪造 `approval_observed`；
- provider 不可用时，Session 和 Web 观察仍然可用；一期只保留内存中的投递失败结果，不形成持久化通知审计历史；
- Session 删除、停止或过期后，旧链接不能访问其他 Session。

## 10. 后续演进

只有在出站通知的真实使用证明有需求后，才考虑：

- IM 入站消息到明确 Session 的授权输入；
- provider 原生线程或回复映射；
- 文件附件和图片引用；
- 用户级订阅、静默时段和通知偏好；
- 多用户身份映射和细粒度 RBAC；
- 可靠投递队列、幂等键和 dead-letter 处理。

审批控制是独立的高风险能力，不属于上述普通 IM 输入演进。任何审批 API 都必须重新定义身份、授权、审计、并发、失败恢复和防误操作模型。
