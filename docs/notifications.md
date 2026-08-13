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

首期支持：

- `none`：不发送；
- `failed`：Session 失败；
- `stopped`：进程停止或退出；
- `completed`：Adapter 能可靠识别完成；
- `resumed`：Session 从断线/等待恢复。

推荐默认行为：

| attention | 默认行为 |
|---|---|
| failed | 发送 |
| stopped | 发送 |
| resumed | 发送 |
| completed | 可配置，默认发送 |
| none | 不发送 |

普通 assistant 文本、单个 tool call/tool result、PTY snapshot、轮询和 token 不逐条发送。

## 4. 去重、debounce 与失败

- 去重 key：`session_id + attention + state`；
- 短窗口内相同 key 只发送一次；
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
    "text": "Agora · Session failed\nClaude stopped\n打开 Agora：https://..."
  }
}
```

### DingTalk

简单文本消息：

```json
{
  "msgtype": "text",
  "text": {
    "content": "Agora · Session failed\nClaude stopped\n打开 Agora：https://..."
  }
}
```

### WeCom

一期使用最简单文本消息：

```json
{
  "msgtype": "text",
  "text": {
    "content": "Agora · Session failed\nClaude stopped\n打开 Agora：https://..."
  }
}
```

### Generic

```json
{
  "text": "Agora · Session failed\nClaude stopped\n打开 Agora：https://...",
  "notification": {
    "session_id": "sess-1",
    "state": "failed",
    "attention": "failed"
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

## 7. Deep link

通知链接形态：

```text
https://agora.example/sessions/<session-id>
```

链接只定位指定 Session 的 Web 观察页：

- 不授予 PTY attach 写入；
- 不授予审批能力；
- 不允许访问其他 Session；
- 不替代 Web 认证。

没有可用的公开 Server Web 地址时：

- 通知明确提示“请在 Agora 所在机器打开”；或
- 省略链接。

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
