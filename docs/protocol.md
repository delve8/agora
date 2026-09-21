# Server–Daemon 协议（产品化一期）

> 状态：Design / 下一阶段实现契约
>
> 本协议定义 Server 与用户电脑上 Daemon 之间的出站 WebSocket 控制通道。协议传输规范化数据和控制请求，不传输持续 PTY raw bytes；Server 只在内存中转发，不持久化业务内容。

## 1. 通用 Envelope

每个 WebSocket 文本帧都是一个 JSON object：

```json
{
  "type": "event.batch",
  "request_id": "req-optional",
  "message_id": "msg-unique",
  "created_at": "2026-08-11T10:00:00Z",
  "payload": {}
}
```

字段：

- `type`：稳定的协议消息类型；
- `request_id`：请求/响应关联 ID；server-push 可为空；
- `message_id`：每帧唯一 ID，用于 ACK 和幂等；
- `created_at`：UTC RFC3339 时间；
- `payload`：类型对应内容。

未知字段必须忽略；未知 `type` 返回协议错误，不得让连接直接崩溃。

## 2. 连接与认证

### 2.1 配对

配对必须由一个已认证的 Agora 用户发起，pairing code 的作用是把陌生设备安全地绑定到该用户；Daemon 不能通过自报 `user_id` 或 `daemon_id` 自行声明归属。

1. 已认证用户在 Server Web UI 请求添加设备；
2. Server 生成短期 pairing code，并将 `code_hash -> user_id` 写入控制面；
3. Daemon 使用 pairing code 调用配对接口；
4. Server 验证 code 未过期、未消费且未撤销；
5. Server 返回随机、可撤销的 device credential，并记录 `device_id -> user_id`；
6. Daemon 将 credential 保存到本地安全存储，至少保证文件权限 `0600`；
7. 后续 WebSocket 连接使用 device credential；
8. Server 不返回完整 credential，日志不记录 credential。

配对 code：

- 一次性使用；
- 有过期时间；
- 服务端只保存哈希；
- 成功配对后立即失效；
- 绑定签发它的 `user_id`，不能跨用户使用；
- 支持撤销设备。

设备归属在配对时决定，WebSocket 连接时只验证 credential。Web 用户身份不通过 Daemon 协议传递。

### 2.2 连接认证

Daemon 建立 WebSocket 时使用 `Authorization: Bearer <device-credential>`。Server 依据 credential 哈希查找设备记录，并得到绑定的 `device_id` 与 `user_id`。

连接认证成功后，Daemon 首帧 `daemon.register` 的 `payload.daemon_id` 必须等于 credential 绑定的 `device_id`。不一致时 Server 拒绝注册并关闭连接，不能允许 credential 持有人冒充其他设备。

- trust-local 模式可省略 device credential，但 Server 仍只接受 loopback 连接；
- auth 模式必须使用 per-device credential，不得退化为所有设备共享的全局 token；
- credential 被撤销后，新的 WebSocket 连接立即失败，已有连接按实现策略断开；
- Web 用户的 Logto principal 只存在于 Server HTTP 请求上下文，不传给 Daemon。

### 2.3 注册

Daemon 连接后首先发送 `daemon.register`：

```json
{
  "type": "daemon.register",
  "message_id": "dmsg-1",
  "created_at": "2026-08-11T10:00:00Z",
  "payload": {
    "daemon_id": "daemon-abc",
    "version": "0.2.0",
    "hostname": "workstation",
    "capabilities": {
      "can_start": true,
      "can_attach": true,
      "can_observe": true,
      "can_send_input": true,
      "can_stream": true,
      "can_resume": true,
      "can_approve": false,
      "can_read_history": true
    }
  }
}
```

Server 回复 `daemon.registered`，包含协议版本和 resync 要求。注册成功后，Server 将该连接视为已认证的指定设备，并只允许它报告或访问属于该 `daemon_id` 的 Session。

## 3. 消息类型

### `daemon.heartbeat` / `daemon.heartbeat_ack`

保活消息。Server 依据最近心跳更新在线状态；超时后将设备标记为 offline，不删除路由历史。

### `daemon.resync` / `server.resync_request`

Daemon 重连后发送当前纳管 Session 摘要、当前路由和 outbox 范围。Server 可以要求某个 Session 重新发送状态或某段 event。

Resync 不要求上传用户全部 Claude 会话，只允许纳管 Session。

### `session.create` / `session.created`

Server 或本地 Agent Runtime 请求 Daemon 在指定 workspace 启动某个 Agent。Server 负责生成 Agora Session ID；Daemon 返回 provider 的 agent session ID、PID（如果有）、history locator 摘要和能力。

workspace 路径不得写入普通 Server 日志；跨用户/跨设备请求必须校验 device scope。

### `session.update`

Daemon 上报：

- Agora Session ID；
- state / connection；
- PID；
- Agent session ID；
- 能力；
- last observed 时间；
- 错误类别和脱敏错误摘要。

### `event.batch`

Daemon 将规范化 Event 批量上报：

```json
{
  "type": "event.batch",
  "message_id": "dmsg-2",
  "payload": {
    "session_id": "sess-1",
    "cursor": {"line": 42, "external_id": "claude-event-42"},
    "events": [
      {
        "id": "evt-42",
        "external_id": "claude-event-42",
        "session_id": "sess-1",
        "type": "text",
        "role": "assistant",
        "content": "Done",
        "summary": "Claude completed the requested change",
        "created_at": "2026-08-11T10:00:00Z"
      }
    ]
  }
}
```

Server 只做：

- 内存去重；
- SSE fan-out；
- notification policy；
- 必要的 ACK。

Server 不将 events 写入业务数据库，也不要求 Daemon 上传 `raw_json`。

### `session.history.request` / `session.history.response`

Web 请求历史时，Server 向目标 Daemon 发起按需请求。Daemon 从对应 Session 的原始 JSONL 读取并返回规范化分页结果。Server 只转发 response，不缓存完整历史。

请求必须包含：

- `session_id`；
- `request_id`；
- `cursor` 或 `since`；
- `limit`，必须有上限。

### `snapshot.request` / `snapshot.response`

PTY snapshot 只按需请求，不持续上传。响应包含屏幕文本、尺寸、cursor 状态和 sequence，不包含 PTY raw bytes。

### `session.input` / `session.input_result`

Web 的明确输入经 Server 路由给目标 Daemon。Daemon 串行写入 PTY，并返回 accepted/failed。普通 IM 文本不进入该消息类型。

### `session.exit`

Daemon 报告 Claude 进程退出，包含状态、退出码、signal 和脱敏错误摘要。

### `ack`

Server 对已接收的 `event.batch` 或其他需要可靠确认的帧回复 ACK：

```json
{
  "type": "ack",
  "message_id": "smsg-3",
  "payload": {"ack_message_id": "dmsg-2"}
}
```

Daemon 收到 ACK 后删除对应 outbox 项。

## 4. Request/response 规则

- `request_id` 在一次连接内唯一；
- response 必须携带原 request 的 `request_id`；
- 超时返回明确的 timeout error，不重试具有副作用的 input，除非带同一个幂等 ID；
- Server 断开后未完成的请求由调用方重新发起；
- Web 的 SSE 不直接暴露 Daemon WebSocket 错误细节。

## 5. 幂等与顺序

- `message_id` 是帧级幂等键；
- Event 使用 `external_id` 或 `id` 去重；
- 同一 Session 的 event 按 `created_at + sequence/cursor` 排序；
- duplicate event 不重复触发 IM notification；
- `session.input` 使用 message ID 防止重试造成重复 PTY 输入；
- Server 不承诺跨 Session 的全局顺序。

## 6. 断线 outbox

Daemon 只缓存有限内容：

- session state transitions；
- error / exit；
- 必要的 event batch 元数据；
- 未完成的协议请求。

普通高频 assistant 文本可以在 outbox 满时丢弃。Daemon 必须上报一个 gap 标记，让 Web 显示离线期间可能存在未完整同步内容。原始 JSONL 不因 outbox 丢弃而删除。

## 7. Web 用户授权与设备 scope

Web 请求由 Server 先解析当前 principal，再执行 Session ownership 检查：

```text
principal.user_id
  └── devices.user_id == principal.user_id
        └── session.daemon_id == devices.device_id
```

因此：

- Server 不因客户端提交 `daemon_id`、`user_id` 或 `session_id` 就视为有权；
- `session_id -> daemon_id` 路由必须与设备归属一致；
- 跨用户或跨设备的 history、SSE、snapshot、input、resume、stop 请求统一拒绝；
- IM 通知链接如被兑换，也只能携带 `scope=read_observation`，并绑定单个 `session_id`；
- 该 scope 不能调用 `session.input`、PTY attach、resume、stop、审批或设备管理。

## 8. 安全约束

- Server 不接受 Daemon 反向开放的公网端口；Daemon 只建立出站连接；
- device credential 不放 URL query、普通日志或 Web 页面；
- Server 校验 `session_id -> daemon_id` 路由，禁止跨设备访问；
- Web input 必须经过 Server 的单用户控制面；
- IM webhook 仅出站通知，不能反向调用 `session.input`；
- 不把 `raw_json`、完整 workspace、工具参数和 PTY 内容写入协议日志。

## 9. 当前实现映射

现有代码仍是本地单进程原型：

- `internal/runtime.Manager` 持有 PTY 与 observer；
- `internal/server.Server` 直接持有 Store 和 Manager；
- SSE 直接消费进程内 pub/sub；
- `internal/store` 仍包含 Event/Message 表。

PTY 已迁移到 Session Host（`agora serve` 也通过 `session-host` 启动 Agent），
该阶段完成。
