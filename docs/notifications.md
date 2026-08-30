# IM 通知实现契约（产品化一期）

> 状态：Design / provider implementation contract
>
> 一期 IM 只负责出站通知和打开 Web deep link。普通 IM 文本不会进入 Claude，不会写入 PTY，也不会被解释为审批按键。

## 1. 通知路径

```text
Claude Session
      │
      ▼
Daemon：解析 JSONL，产生规范化 state/attention/summary
      │
      ▼
Server：内存 policy + dedupe + provider adapter
      │
      ├── Feishu webhook
      ├── DingTalk webhook
      ├── WeCom webhook
      └── Generic webhook
```

Provider 适配集中在 Server，便于统一升级；Daemon 不依赖各家 IM SDK，也不负责生成 provider 专属 envelope。

## 2. 通知模型

```go
type SessionNotification struct {
    ID             string
    CoordinationID string
    SessionID      string
    SessionName    string
    State          string
    Attention      string
    Title          string
    Summary        string
    OpenURL        string
    CreatedAt      time.Time
}

type Target struct {
    ID       string
    Provider string // generic | feishu | dingtalk | wecom
    Label    string
    URL      string
    Enabled  bool
}
```

通知必须包含：

- Coordination 或 Session 可识别信息；
- state 或 attention；
- 面向人的短标题和摘要；
- 时间；
- Server Session deep link（如果配置了可用的公开地址）。

通知不包含：

- 完整 transcript；
- 原始 JSONL 或 `RawJSON`；
- PTY raw bytes、ANSI/VT 画面；
- 完整 tool input/output；
- approval/reject 按钮；
- device credential、签名 token 或完整 workspace 私密路径。

## 3. Attention 与发送策略

Webhook 是任务通知，不是运维告警。Server 不因为 Session 启动、恢复、停止、进入普通 waiting、轮询或普通 error event 发送通知。只有 Agent 明确给出任务结果，或终端进入已识别的用户介入提示时才发送。

首期支持：

- `none`：不发送；
- `failed`：Agent 任务明确失败或异常退出；
- `completed`：Adapter 能可靠识别任务完成；
- `approval_required`：已识别的原生终端用户介入提示。

推荐默认行为：

| attention | 默认行为 |
|---|---|
| failed | 发送 |
| completed | 发送 |
| approval_required | 发送 |
| none | 不发送 |

普通 assistant 文本、单个 tool call/tool result、PTY snapshot、轮询和 token 不逐条发送。

## 4. 去重、debounce 与失败

- 任务结果使用 `session_id + attention + event_id` 去重；
- 用户介入提示按 Session 和短时间窗口去重；
- duplicate Event 不重新触发通知；
- provider 失败不影响 Agent、Daemon JSONL、Web SSE 或 Web input；
- 多个 target 独立投递，一个失败不能阻断其他 target；
- 一期不引入可靠分布式队列；
- 可做有限内存重试，不能无限重试造成消息风暴；
- 投递失败只保留内存中的低敏结果：provider、HTTP status、错误类别和时间。

## 5. Provider payload

### Feishu

简单文本消息：

```json
{
  "msg_type": "text",
  "content": {
    "text": "Agora · Agent 任务完成\n任务结果已产生\n打开 Agora：https://..."
  }
}
```

### DingTalk

简单文本消息：

```json
{
  "msgtype": "text",
  "text": {
    "content": "Agora · Agent 任务完成\n任务结果已产生\n打开 Agora：https://..."
  }
}
```

### WeCom

一期使用最简单文本消息：

```json
{
  "msgtype": "text",
  "text": {
    "content": "Agora · Agent 任务完成\n任务结果已产生\n打开 Agora：https://..."
  }
}
```

### Generic

```json
{
  "text": "Agora · Agent 任务完成\n任务结果已产生\n打开 Agora：https://...",
  "notification": {
    "session_id": "sess-1",
    "state": "failed",
    "attention": "completed"
  }
}
```

Generic payload 可以保留规范化通知元数据，但不应包含 `RawJSON`、完整工具参数或敏感凭据。

## 6. Target 配置

一期支持多个 target：

```text
Target 1: feishu / personal / URL / enabled
Target 2: dingtalk / team / URL / enabled
Target 3: wecom / ops / URL / enabled
```

配置接口后续可以提供 REST 或本地管理 CLI，至少支持：

- create；
- list（URL 脱敏）；
- enable/disable；
- delete；
- test notification。

URL 的要求：

- 不写入普通日志；
- 不出现在 API list 响应；
- 错误信息不能回显完整 URL；
- 如果持久化配置，使用加密配置或系统安全存储；
- 一期不支持通过 IM 入站 webhook 反向控制 Agent。

## 7. Deep link 与 capability link

通知可以携带两种不同的 Web 链接，provider adapter 必须按 Server 当前认证模式选择：

### 7.1 普通登录 Session URL

```text
https://agora.example/sessions/<session-id>
```

该 URL 不包含秘密。用户打开后按正常 Web 认证流程（Logto 登录或已有 Web session）进入，Server 再检查当前 principal 是否拥有目标 Session。`focus=terminal` 等参数只改变展示位置，不改变权限。

### 7.2 只读 notification capability link

如果产品要求用户点开 IM 链接后无需再次输入登录信息，Server 应签发专用 capability link：

```text
https://agora.example/auth/notification-link?token=<opaque-token>
```

签发和兑换规则：

- 只能由已认证且拥有目标 Session 读取权限的用户触发签发；
- token 高熵不可预测，Server 只保存哈希或使用带密钥的签名结构；
- grant 绑定 `user_id`、单个 `session_id`、`scope=read_observation`、`issued_at`、`expires_at` 和撤销依据；
- 推荐 TTL 为 10 分钟；
- 专用 endpoint 验证成功后设置 `HttpOnly + Secure + SameSite` 的受限 cookie，再 302 到不带 token 的 Session URL；
- 响应设置 `Referrer-Policy: no-referrer`，页面和静态资源不得传播 token；
- 兑换后的 cookie 生命周期不超过 grant TTL，且不能作为通用 API Bearer credential。

`read_observation` 只允许：

- 指定 Session metadata；
- 指定 Session history；
- 指定 Session SSE event stream；
- 指定 Session 只读 PTY snapshot。

它不允许：

- `POST /api/sessions/{id}/messages`；
- resume、stop、PTY attach 写入；
- 设备配对、设备撤销、webhook 配置；
- 其他 Session 或未来审批接口。

这不是完整登录凭证。token 泄露的后果是持有者在 TTL 内可以观察绑定 Session，因此 IM 群、频道、邮箱等通知 target 是可信接收边界。通知正文仍不得包含完整 transcript、PTY raw bytes、credential、完整 workspace 或敏感工具参数。

IM provider、邮件客户端和安全扫描器可能预取链接；实现不得依赖“首次 GET 立即永久消费”作为唯一防重放机制。优先使用短 TTL、scope 限制、撤销/授权版本和兑换后清理 URL。若未来需要严格一次性消费，应另行设计预览/确认流程。

### 7.3 本地地址

- trust-local 模式自动使用 `local` 用户，不要求登录或 capability cookie；
- `127.0.0.1`/`localhost` 链接只适用于 Agora 所在机器；
- 没有公开 HTTPS 地址时，通知应提示“请在 Agora 所在机器打开”或省略链接；
- `local` 模式禁止把 Server 绑定到非 loopback；跨设备通知链接必须使用认证模式和 HTTPS。


## 8. 隐私与生命周期

Server 可以在发送 HTTP webhook 前短暂看到通知内容，但一期不持久化：

- 不保存通知正文；
- 不保存完整 webhook response；
- 不保存完整 webhook URL 到普通数据库；
- 不将通知写入 Event/Message；
- 不将通知作为 Agent input。

如果未来需要通知历史、未读状态、用户订阅或可靠重试，需要单独评审持久化数据范围和隐私策略。

## 9. 当前实现映射

当前仓库已经提供基础实现：

- `internal/notification/notification.go`：通知与 target 模型；
- `internal/notification/policy.go`：窗口去重；
- `internal/notification/dispatcher.go`：多 target 调度；
- `internal/notification/webhook.go`：四种 payload 和 HTTP 投递；
- `internal/notification/*_test.go`：payload、失败隔离和去重测试。

尚未接入：

- Daemon 上报到 Server 的真实协议；
- Server state transition 到 Dispatcher 的真实调用点；
- Target 配置 API；
- 加密配置存储；
- provider 特定签名/加签 webhook（如后续需要，应在 adapter 内扩展）。
