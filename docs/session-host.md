# Session Host 设计

> 状态：Design + Phase B prototype / Daemon 重启保持 Agent 运行态
>
> 本文定义 Agora 如何通过独立的 per-session `session-host` 进程托管 Pi、Claude 以及未来其他 Agent，使 Daemon 重启、升级或短暂崩溃不自动终止正在运行的 Agent。当前仓库已提供独立 `agora session-host --config <path>`、control/attach socket、metadata、token handshake、Agent exit cleanup，以及 Daemon 侧的创建/接管基础；完整生产级接管、事件流和所有 provider 路径仍按本文实施计划推进。

## 1. 背景与目标

当前原型由 Daemon 直接持有 Agent 子进程、PTY master、attach socket 和 observer：

```text
agora daemon
  ├── Pi/Claude child process
  ├── PTY master
  ├── attach socket
  └── observer / history cursor
```

这种结构实现简单，但 Daemon 的进程生命周期和 Agent Session 的生命周期耦合在一起。Daemon 重启时，如果直接关闭 manager，Agent、PTY 和 wrapper 都会受到影响；如果只删除 `SIGTERM`，PTY fd 和 attach socket 又无法由新 Daemon 安全接管。

Session Host 的目标是解除这两个生命周期：

```text
agora daemon
  └── Session Host A
        ├── Agent A
        ├── PTY / control transport
        └── attach surface

agora daemon
  └── Session Host B
        ├── Agent B
        └── control transport
```

目标行为：

- 一个 managed Agora Session 对应一个独立的 Session Host；
- Session Host 持有 Agent 进程及其 transport，Daemon 只持有控制面连接；
- Daemon 正常退出、升级、重启或短暂断线时，Session Host 和 Agent 继续运行；
- 新 Daemon 可以发现并重新连接仍存活的 Session Host；
- Agent 自己退出后，Session Host 发送退出通知、清理资源和 runtime metadata，然后自行退出；
- 用户明确执行 Stop 时，Session Host 才终止 Agent；
- Server、Web 和 IM 仍然只看到规范化的 Session、Event 和 Capabilities，不感知 Session Host 的 provider-specific 实现。

非目标：

- 不把 Session Host 变成 transcript 存储服务；
- 不把 Server 变成 Agent 进程 supervisor；
- 不要求所有 Agent 都有 PTY；
- 不在 Daemon 重启时恢复任意已经被操作系统杀死的进程；
- 不通过扫描任意 PID 自动接管无法验证归属的外部进程。

## 2. 核心模型

### 2.1 进程粒度

推荐的第一版模型是：

```text
一个 Agora managed Session
  = 一个 Session Host
  = 一个 Agent runtime
  = 一个 Agent process（如果 provider 使用进程）
  = 一个独立的 control/attach surface
```

同一个 Agent Session 的多条消息、工具调用和 `/resume` context switch 都在同一个 Session Host 内完成，不为每条消息创建新的 Host。

一个 Session Host 只托管一个 Agent context。多个 Session Host 之间相互隔离：一个 Host 崩溃、清理或 Agent 退出不应影响其他 Session。

### 2.2 per-session 与 shared supervisor

第一版采用 per-session Host：

| 方案 | 优点 | 代价 | 决策 |
|---|---|---|---|
| per-session Host | 故障隔离清晰、权限边界清晰、rebind 和清理简单 | 进程数量更多 | 采用 |
| shared supervisor | 进程数量少、可统一调度 | 一个 supervisor 故障影响全部 Session，fd/状态隔离复杂 | 暂不采用 |

后续如果单机 Session 数量非常大，可以在不改变 Server 协议的前提下，将多个 Host 的实现合并到一个 supervisor；那是运行时优化，不改变 Session 的逻辑边界。

### 2.3 生命周期边界

必须区分：

```text
Daemon disconnect / restart
  ≠ Agent stop
  ≠ Session Host shutdown
```

| 事件 | Session Host | Agent | Session 状态 |
|---|---|---|---|
| Daemon 短暂断线 | 继续运行，等待重连 | 继续运行 | running / detached |
| Daemon 正常重启 | 继续运行 | 继续运行 | running / detached |
| 用户 Stop | 收到 shutdown/stop | 终止 | stopping → stopped |
| Agent 自己退出 | 执行清理后退出 | 已退出 | exited / stopped |
| Host 异常退出 | 操作系统回收资源 | 通常一起退出 | stale，需要重建或 resume |
| 机器关机 | 由系统终止 | 由系统终止 | 依赖 provider history 恢复 |

### 2.4 进程组与信号

仅仅“让 Host 成为中间进程”不足以实现上面这张表，还需要明确信号归属：

- Daemon 发起的 `Spawn` 让 Host 进入**独立 session / 进程组**（`Setsid`）。否则 Ctrl-C 会发给 Daemon 所在终端的前台进程组，Host 会一起收到 SIGINT 而立刻退出，Agent 则因为 PTY master 被关闭而跟着退出。
- Host 入口调用 `sessionhost.IgnoreTerminalSignals()`，忽略 SIGINT 与 SIGHUP。这两个信号属于“驱动 Daemon 的终端”，不代表用户要停止 Agent。
- SIGTERM 仍然生效：它表示明确要求该 Host 进程退出。用户显式 Stop 走 control socket 的 `stop`/`shutdown`，不依赖信号。
- `Daemon.Close()` 只丢弃 client 连接，绝不向 Host 发信号。

对应约束：

```text
SIGINT / SIGHUP（终端）        → Host 忽略，Agent 继续运行
SIGTERM（直接发给 Host）        → Host 退出
control socket stop/shutdown   → Host 终止 Agent 并清理后退出
Agent 自己退出                 → Host 清理后退出
```

因此判定“Daemon 重启不影响 Agent”的验收必须在两种停止方式下都成立：

1. 在 Daemon 所在终端按 Ctrl-C；
2. 只向 Daemon 进程发送 SIGTERM（或 `kill <daemon-pid>`）。

需要注意的是，Agent 自己由 `pty.Start` 启动，已经拥有独立 session，因此它不会被 Daemon 终端的信号直接命中；在缺少上述措施时，它是因 Host 退出、PTY master 被关闭而退出。

## 3. 目标进程拓扑

```text
用户终端 wrapper
        │
        │ Unix attach socket（raw terminal，可选）
        ▼
Session Host
        │
        ├── control socket（JSON framed control plane）
        ├── PTY master / stdio / HTTP-ACP client
        ├── provider observer
        └── Agent process

Agora Daemon
        │
        ├── Server WebSocket
        ├── Session Host registry/client
        ├── history catalog / reader
        └── normalized event relay
```

Daemon 与 Session Host 的职责：

### Session Host 负责

- 启动 Agent 并设置 provider 所需的 workspace、环境和参数；
- 持有 Agent 的 process、PTY master、stdio pipe 或 provider transport；
- 提供可选的 attach socket 和 terminal snapshot；
- 提供 control socket，处理 input、interrupt、stop、snapshot、state、rebind；
- 读取或监听 Agent 的 live output，并把原始数据提供给 Daemon；
- 监控 Agent child exit；
- 保持 Daemon 断线期间的最小运行态；
- 写入、更新和清理本地 runtime metadata；
- 在 Agent 退出后发送最终退出结果并退出。

### Daemon 负责

- 与 Server 建立设备级 WebSocket 和重连；
- 创建、连接、认证和管理 Session Host；
- 把 Web/API 的 input、stop、resume、attach、snapshot 请求路由到目标 Host；
- 使用 provider adapter 读取 history 和建立 observer；
- 将 Host 的状态和规范化 Event 上报 Server；
- 在重启后扫描 runtime registry 并恢复 Host 连接；
- 维护 Server 可见的 canonical Session ID、route 和能力摘要。

### Server 负责

- 用户、设备和 Session ownership 授权；
- 保存必要的控制面 Session metadata；
- 将 Web 请求 relay 给目标 Daemon；
- 接收 Daemon 的 Session update、rebind、exit 和规范化事件；
- 不连接 Session Host，不持有工作站上的 PID、PTY fd 或 provider history。

## 4. Runtime 目录和元数据

### 4.1 目录布局

建议使用用户私有目录：

```text
~/.agora/runtime/
├── registry/
│   └── <host-key>.json
└── sessions/
    └── <host-key>/
        ├── metadata.json
        ├── control.sock
        ├── attach.sock       # 只有有 terminal surface 时存在
        ├── state.json        # 可选，短期运行态/退出结果
        └── lock
```

`<host-key>` 必须是安全的本地标识，不直接使用未经编码的 workspace 或 provider session ID。可以使用随机 UUID；canonical Agora Session ID 和 native Session URI 作为 metadata 字段保存。

目录和 socket 权限至少为当前用户可读写：

```text
runtime directory: 0700
metadata/state:    0600
control.sock:      0600
attach.sock:       0600
```

### 4.2 metadata schema

目标 schema 示例：

```json
{
  "schema_version": 1,
  "host_id": "host-uuid",
  "session_id": "daemon/device-1/pi://native-id",
  "daemon_id": "device-1",
  "agent": "pi",
  "agent_session_id": "pi://native-id",
  "workspace": "/Users/me/project",
  "pid": 12345,
  "process_start_time": "2026-09-02T12:30:00.123Z",
  "host_pid": 12344,
  "host_start_time": "2026-09-02T12:30:00.100Z",
  "control_socket": "/Users/me/.agora/runtime/sessions/host-uuid/control.sock",
  "attach_socket": "/Users/me/.agora/runtime/sessions/host-uuid/attach.sock",
  "history_path": "/Users/me/.pi/agent/sessions/project/session.jsonl",
  "display_name": "Refactor Agora",
  "display_name_source": "custom",
  "state": "running",
  "created_at": "2026-09-02T12:30:00Z",
  "updated_at": "2026-09-02T12:31:00Z",
  "token_hash": "..."
}
```

约束：

- metadata 是恢复和发现索引，不是 transcript；
- token 只保存 hash 或由系统安全存储保护的引用，不把明文 token 写入普通日志；
- PID 必须和 process start time 一起校验，不能只依赖 PID；
- `session_id`、`agent_session_id`、workspace 和 history locator 必须彼此一致；
- metadata 更新采用临时文件 + `fsync`（在目标平台可行时）+ rename，避免留下半写文件；
- Host 创建时先持有独占 lock，避免 Daemon 重启扫描时重复启动同一 Host。

### 4.3 registry 与 metadata 的区别

- `registry/` 是 Daemon 启动时快速发现 Host 的索引；
- `sessions/<host-key>/metadata.json` 是 Host 的权威本地描述；
- registry 可以被 Daemon 重建，不应被视为 Agent history；
- Host 正常退出时删除自己的 registry/metadata；异常退出时由新 Daemon 清理 stale entry。

## 5. Session Host 控制协议

Session Host control socket 使用本机 Unix domain socket。协议必须有明确 framing、request ID、版本和认证信息；不能把任意 JSON 拼接后依赖 EOF 判断一条消息结束。

示例 envelope：

```json
{
  "version": 1,
  "request_id": "req-123",
  "token": "one-time-or-session-token",
  "type": "state",
  "payload": {}
}
```

响应：

```json
{
  "version": 1,
  "request_id": "req-123",
  "type": "state.result",
  "ok": true,
  "payload": {
    "host_id": "host-uuid",
    "session_id": "daemon/device-1/pi://native-id",
    "agent_session_id": "pi://native-id",
    "state": "running",
    "pid": 12345,
    "capabilities": {
      "can_send_input": true,
      "can_read_terminal": true
    }
  }
}
```

### 5.1 最小命令集合

| 命令 | 作用 | 备注 |
|---|---|---|
| `hello` | 握手和 Host 身份确认 | 返回 schema、host ID、session identity |
| `state` | 查询运行态 | 返回 PID、transport、native ID、能力和状态 |
| `input` | 发送用户输入 | 仅由已授权 Daemon 调用 |
| `interrupt` | 中断当前 Agent 操作 | provider-specific |
| `stop` | 显式终止 Agent 并清理 | 终止条件之一 |
| `attach` | 获取 attach surface 信息 | PTY/terminal Agent 可用 |
| `snapshot` | 获取只读终端 snapshot | 可选能力 |
| `rebind` | 更新 native Session identity | 进程和 transport 不重启 |
| `ping` | 保活 | 不改变 Agent 状态 |
| `shutdown` | Host 自身退出 | 只在 Agent 已退出或显式清理时使用 |

事件方向也使用同一 control connection 或单独的 authenticated event connection：

- `host.ready`
- `agent.state`
- `agent.output`（仅内部 raw/structured stream，不直接上送 Server）
- `agent.exit`
- `session.rebind`
- `host.error`
- `host.exited`

### 5.2 连接断开语义

Control client（Daemon）断开不等于 stop。Host 应：

1. 关闭该 Daemon 的控制连接；
2. 保留 Agent、PTY 和本地 attach surface；
3. 进入 `detached` 或继续保持 `running`；
4. 在有限时间内等待新 Daemon 连接；
5. 可选地在 metadata 中更新 `last_daemon_seen_at`；
6. 不因一次网络断开杀死 Agent。

是否设置 orphan TTL 需要按产品策略决定。第一版建议：

- 正常 Daemon 重启期间不设短 TTL；
- 只有用户明确 Stop、Agent 退出，或 Host 自身无法继续提供 transport 时才清理；
- 长期没有任何 Daemon 接管的 Host 可以由后续 housekeeping 策略按明确的用户可见规则处理，不能静默杀掉正在运行的 Agent。

## 6. 启动、接管和恢复

### 6.1 创建新 Session

```text
Web / wrapper 请求创建
  → Server 授权并 relay 给 Daemon
  → Daemon 创建 host-key 和 runtime directory
  → Daemon 启动 session-host
  → Session Host 校验 workspace/token
  → Session Host 启动 Agent
  → Host 写 metadata
  → Daemon 完成 hello/state 握手
  → Daemon 上报 canonical Session 和 capabilities
  → wrapper 获取 attach socket
```

创建必须避免以下窗口：

- Agent 已启动但 metadata 尚未落盘；
- metadata 已落盘但 Host 没有监听 control socket；
- Server 已看到 Session 但 Host 创建失败；
- Daemon 重启扫描时重复启动同一个 Agent。

推荐顺序是先创建目录和 lock，启动 Host，完成 authenticated `hello`，收到 `host.ready` 后再向 Server 发布可控的 running Session。失败时发送明确的 failed 状态并清理 Host。

### 6.2 Daemon 重启

新 Daemon 启动时：

```text
扫描 registry 和 sessions/*
  → 校验 metadata schema 和权限
  → 校验 host_pid / start time
  → 连接 control.sock
  → 发送 hello + token proof
  → 查询 state
  → 校验 native ID、workspace、history locator
  → 恢复本地 Session mapping/observer
  → 向 Server 发送 resync
```

接管成功后：

- Host 的 PID 不变；
- Agent 的 PID 不变；
- PTY master 不变；
- native Session ID 不变，除非随后发生 `/resume` rebind；
- Daemon 可以重新创建自己的 observer、history cursor 和 Server WebSocket route；
- wrapper 如果原 attach connection 已断，可以使用新的 attach socket 重新连接。

接管失败时不能直接按 metadata 启动第二个 Agent。必须先判断：

1. Host 是否还活着；
2. control socket 是否可连接；
3. token proof 是否通过；
4. Host 返回的 identity 是否与 metadata 一致；
5. Agent child 是否仍属于该 Host。

只有确认 Host 不存在或不可恢复，且用户允许恢复时，才把 Session 标记为 stopped/stale，并通过 provider history 提供显式 Resume。

### 6.3 Server 重启

Server 重启与 Daemon 重启不同：

- Session Host 和 Agent 不受 Server 影响；
- Daemon 保持或重建对 Host 的连接；
- Daemon 重新连接 Server 后发送完整 resync；
- Server 依据 Daemon 的 live summaries、history summaries 和 routes 重建在线控制面；
- Server 不要求 Host 连接 Server，也不保存 Host 的 raw transcript。

## 7. Agent 退出与清理

### 7.1 正常退出流程

Session Host 必须把 child exit 作为明确的状态转换，而不是依赖 Daemon 是否在线：

```text
Agent child exits
  → Wait 收集 exit code/signal/error
  → 停止新 input 和新 attach
  → flush/close provider transport
  → 更新最终 metadata/state
  → 通知 Daemon agent.exit（若在线）
  → 等待有限时间发送 host.exited（可选）
  → 关闭 attach/control socket
  → 删除 socket、lock、metadata 和临时 runtime 文件
  → 退出 Session Host
```

如果 Daemon 不在线：

- Host 可以把有限的 exit summary 写入短期 `state.json`，供新 Daemon 诊断；
- 不写入完整 transcript、PTY 内容或敏感输入；
- 完成清理后退出；
- 新 Daemon 通过 provider history 看到 stopped/resumable history，而不是假设旧 Host 仍然存在。

### 7.2 显式 Stop

用户点击 Stop 的路径是：

```text
Web → Server → Daemon → Session Host stop
  → Host 向 Agent 发送 provider-specific stop/terminate
  → 等待 child exit
  → 执行统一清理
  → Daemon/Server 收到 stopped
```

`stop` 必须幂等：Agent 已退出、Host 已进入 cleaning 或重复 Stop 不应重新启动 Agent，也不应产生错误的 running 状态。

### 7.3 异常退出与 stale 清理

Host 可能因 kill -9、机器断电或运行时崩溃来不及删除 metadata。新 Daemon 的 housekeeping 需要：

- 检查 metadata JSON 是否完整；
- 检查目录和 socket 是否属于当前用户；
- 检查 Host PID 和 start time；
- 尝试 authenticated hello；
- 删除确认 stale 的 socket、lock 和 metadata；
- 保留有限的诊断信息，避免覆盖 provider history；
- 将对应 Session 标记为 stale 或 stopped，并允许用户显式 Resume。

不能仅凭“PID 当前存在”就判断 Host 正常，因为 PID 可能已被操作系统复用。

## 8. `/resume` 与运行时 rebind

`/resume` 是 Agent context switch，不是新建 Agent process。因此它发生在现有 Session Host 内：

```text
同一个 Session Host
  ├── Agent process 不变
  ├── PTY/stdio transport 不变
  ├── Agora Session logical binding：old → new
  └── provider history locator：old → new
```

推荐流程（按优先级）：

```text
1. provider 原生报告（首选，精确）
   Agent 进程由 Host 以 `pi -e <agora-session-reporter.ts>` 启动并带上 AGORA_HOST_ID；
   扩展在 session_before_switch / session_start 时通过本地 socket 上报
   {reason, target_session_file, session_file, session_id, previous_session_file}。
   Daemon 按 Host ID 解析出 managed Session，直接用上报的 file/id 做 rebind。
2. 证据式推断（fallback：用户自装 pi、扩展不可用、provider 无结构化事件）
   见下方 2–8 步。
```

证据式流程：

1. Daemon 记录 Host terminal 上提交的每一行输入（原始 bytes 仍原样转发给 Agent）；
2. 定期检查 provider history：只读取各候选 transcript **新增的字节**（用 `stat` 做廉价门槛），不重新解析历史内容；
3. 当某个候选 transcript 的增量里出现“与已提交输入行对应”的用户消息，并且 workspace 相同、时间接近、候选唯一时，确认发生了 context switch；
4. 在 Host 内原子更新 `agent_session_id`、history locator 和 logical binding；
5. Daemon 更新本地 observer/cursor 和 process key；
6. Daemon 向 Server 发送 `session.rebind` 和新的 Session metadata；
7. Server rekey route/session/cursor；
8. Web 跟随新的 canonical Session ID，重新订阅 history/SSE。

### 8.0 provider 原生报告（已实现，推荐）

`pi --extension, -e <path>` 支持按会话注入扩展，扩展 API 暴露了切换事件：

```typescript
pi.on("session_before_switch", (event) => { /* event.reason, event.targetSessionFile */ });
pi.on("session_start", (event, ctx) => {
  // event.reason: "startup" | "reload" | "new" | "resume" | "fork"
  // event.previousSessionFile
  // ctx.sessionManager.getSessionFile() / getSessionId() / getSessionName()
});
```

Agora 的实现：

- `internal/runtime/piextension/agora-session-reporter.ts` 嵌入到二进制，落盘到
  `~/.agora/extensions/pi/agora-session-reporter.ts`，由 Host 在 spawn 时以 `-e <path>` 注入；
- Host 给 Agent 注入 `AGORA_HOST_ID`（跨 rebind 稳定，canonical ID 会变化）；
- 扩展向 `$AGORA_DAEMON_SOCKET`（默认 `~/.agora/daemon.sock`）发送 `session.report`；
- Daemon 在同一个本地 socket 上分流：`type == "session.report"` 交给
  `Manager.ReportAgentSession`，其余仍是 wrapper 请求；
- `ReportAgentSession` 用 Host ID 反查当前 canonical Session，再复用 `rebindSession` 完成
  rekey / Host metadata / observer / Server route；
- 上报 best-effort：socket 不可用或扩展抛错时静默降级，绝不阻断 Agent。

实测（真实 pi，非 mock）：

```text
启动新会话 → reason=startup, session_file=<将来的文件路径>, session_id=<id>
/resume    → reason=resume, target_session_file=<目标文件>   (切换前)
             reason=resume, session_file=<目标文件>, session_id=<id>,
                            previous_session_file=<原文件>   (切换后)
```

因此 `/resume`、`/new`、`/fork`、`/clone` 以及 `--resume` 启动都能在**用户发消息之前**完成精确 rebind，
且完全不受输入法/自动补全影响。

### 8.1 触发信号不能依赖按键

早期方案把“观察到 `/resume` 这一行”当作触发条件，这是不可行的：TUI 提交命令时用的是它自己的输入缓冲区，用户用补全菜单时终端只发出了 `/res` 之类的前缀，用其它方式打开 picker 时甚至没有任何对应按键。因此触发条件改为**证据**而不是按键：

```text
已提交的终端输入行
  + 同一 workspace 下另一个 transcript 新增了对应的用户消息
  + 该记录写入时间与提交时间接近（±1 分钟）
  + 候选唯一
  ⇒ 确认 context switch
```

### 8.1.1 context switch 本身没有任何持久化痕迹

对 Pi 0.84 的实测结论：**切换动作不会写进任何 transcript**。

全部 session 文件的 record type 只有：

```text
message / text / thinking             会话内容
session                               每个文件恰好一条 header（resume 不会重写）
model_change / thinking_level_change  状态类命令（/model、thinking level）
compaction                            /compact
session_info                          /name
image
```

没有 `resume` / `switch` / `fork` 之类的记录类型，`session` header 每个文件只有一条，`~/.pi/agent` 下也没有 current-session 指针文件（`pi -c` 是按目录内 mtime 推断的）。切换只是把后续写入重定向到另一个文件，两个文件都不会得到标记。

因此：

- 切换发生后、用户在目标会话发出第一条消息之前，history 侧**不存在任何证据**；
- rebind 必然由“下一条消息”触发，UI 在此期间仍显示旧 canonical ID（history 为空）属于已知行为，不是可以靠监听按键修复的缺陷；
- 若需要“切换瞬间即可感知”，只能由 provider 结构化暴露（例如 Pi 的 `--mode rpc` 或等价 session-state event），这也符合 `docs/agent-integration.md` 中“有结构化 context/session 变更事件时优先消费”的约定。屏幕指纹等启发式手段过于脆弱，不作为方向。

### 8.2 文本比较必须容忍输入法噪声

终端字节流 ≠ provider 存储的消息文本：

- 输入法会流式发送 preedit 内容以及无法表示时的替换字符（U+FFFD）；
- 用户编辑会发送退格/删除，宽字符还可能对应多次退格；
- provider 只保存 composer 最终文本。

因此比较不能要求字节相等，而是比较“短语重合度”：把观察到的行与候选消息都归一化（折叠空白、去掉替换字符），按 4 个 rune 的窗口统计重合短语，要求覆盖观察文本的一半以上（并有下限）。同时收紧时间窗口到 ±1 分钟，用时间+workspace+唯一性来保证证据强度。回退场景（没有“最近记录”可重读时）不会有误判，只是不切换。

### 8.3 未确认的记录不能被跳过

provider 是分片写入 JSONL 的，一次扫描可能正好看到“半条记录”。因此：

- 游标只能按 provider parser 给出的“完整记录结束位置”推进，绝不能直接跳到文件大小；
- 处于证据窗口内的未确认记录要保留可重读性（否则“先看到记录、后收到输入”的竞态会永久丢失证据）。

### 8.4 约束

- 只有 `/resume`（或任何按键）不能直接证明目标 Session；
- 没有后续输入、history 延迟、多个候选或证据不足时不得自动切换；
- rebind 不能杀掉或重启 Agent；
- rebind 失败时保留旧 binding，不产生半更新状态；
- Host metadata、Daemon state、Server route 必须最终收敛到同一个 native Session URI。

## 9. attach 与终端断线

对于 Claude、Pi 等原生 TUI：

```text
wrapper → attach.sock → Session Host → PTY master → Agent
```

Daemon 重启不应关闭 `attach.sock`。如果 wrapper 连接依赖 Daemon 返回的地址，则新 Daemon 可以从 metadata/state 重新返回同一个 attach socket。

需要明确：

- attach socket 的存在不代表 Agent 一定仍在运行，连接时必须查询 Host state；
- 多个 attach client 的输入广播和互斥策略由 Host 负责；
- terminal raw bytes 不进入 Server 持久化；
- Web 的只读 snapshot 是可选能力，不应依赖 wrapper 在线；
- wrapper 的本地 attach client 可能在 Daemon 重启期间遇到短暂断线，产品上可以通过 reconnect 或重新执行 attach 恢复；
- 若要求现有 wrapper 连接完全无感知地跨 Daemon 重启，则 wrapper 必须直接连接稳定的 Host attach socket，而不是连接 Daemon-owned socket。

对于 stdio RPC、HTTP/ACP 等没有 PTY 的 Agent，Host 只暴露 control/event surface，不创建 attach socket。

## 10. 安全设计

Session Host 位于用户工作站本地，但不能因为是 Unix socket 就省略认证和校验。

### 10.1 本地边界

- runtime 目录使用 `0700`；
- socket 使用 `0600`；
- Host 拒绝不属于当前用户的 runtime metadata；
- control socket 每次连接都要求 session token proof；
- token 不进入命令行、普通日志、Server payload 或错误响应；
- workspace 必须由 Daemon 规范化并由 Host 在本地校验；
- 不允许通过 metadata 中的任意 path 访问其他用户目录或任意 Unix socket。

### 10.2 identity 校验

Host hello 至少返回：

- host ID；
- Session canonical ID；
- provider 和 native Session URI；
- workspace；
- Host PID/start time；
- Agent PID/start time（如果有）；
- protocol/schema version。

Daemon 必须把返回值与 metadata 和 Server 当前 Session 比较。任何 identity 不一致都进入 quarantine/stale 流程，而不是自动接管。

### 10.3 输入授权

只有经过 Server 用户授权、Daemon device ownership 校验和 Host 本地 token 校验的请求才能执行：

- input；
- stop；
- interrupt；
- attach 写入；
- rebind。

只读状态、history 和 snapshot 也必须限制在目标 Session，不允许用一个 Host token 访问其他 Host。

## 11. 状态机

Session Host 状态：

```text
creating
   ↓
starting
   ↓
ready ────────┐
   ↓          │
running      │ Daemon disconnect/reconnect
   ↓          │
 detached ────┘
   ↓
 stopping
   ↓
 cleaning
   ↓
 exited
```

异常分支：

```text
creating/starting → failed → cleanup → exited
running           → agent_exited → cleanup → exited
running           → host_crashed → stale（由新 Daemon 处理）
```

状态事实来源：

- Agent process 是否存在：Host；
- native Session identity：Host + provider metadata/history；
- history cursor：Daemon observer；
- Server 在线 route：Server；
- 用户可见的规范化 Session state：Daemon 上报，Server 汇总。

任何组件都不能用自己的暂时缓存覆盖更权威的事实来源。

## 12. 失败处理与幂等性

### 12.1 创建失败

如果 Agent 启动失败：

1. Host 返回明确错误；
2. 关闭 transport 和 sockets；
3. 删除 metadata 或标记 failed；
4. Daemon 向 Server 上报 failed；
5. 不留下一个看似 running 但无法 attach 的 Session。

### 12.2 Daemon 在 Host 启动中崩溃

Host 必须能够独立完成启动。新 Daemon 通过 metadata 和 hello 查询最终状态：

- Host ready：接管；
- Agent failed：读取 exit/error summary；
- Host 没有 metadata 且进程不可验证：清理并标记失败；
- Host identity 不一致：隔离，不自动接管。

### 12.3 重复命令

以下操作必须幂等或返回可判断的当前状态：

- `ping`；
- `state`；
- `stop`；
- `shutdown`；
- 重复 `hello`；
- 同一个 rebind request ID 的重试。

`rebind` 需要带旧 native ID 或 generation/version，避免旧 Daemon 的延迟请求覆盖新的 binding。

## 13. 与现有 Agora 模型的关系

Session Host 不改变现有 canonical ID：

```text
daemon/<daemon-id>/<agent>://<native-session-id>
```

也不改变以下边界：

- Server 不持有 transcript；
- provider history 仍由工作站本地事实源提供；
- Session、Event、Capabilities 继续是跨 Agent 的规范化模型；
- PTY、terminal snapshot 和 attach 仍是可选 `TerminalSurface`；
- Pi、Claude、OpenCode、Codex 使用各自的 driver/transport/history adapter；
- Server–Daemon 协议继续传递规范化 metadata 和控制结果，不暴露 Session Host 的原始 provider 协议。

建议新增或扩展的 Daemon 内部接口：

```go
type SessionHost interface {
    ID() string
    Start(ctx context.Context, req StartRequest) (HostState, error)
    Connect(ctx context.Context) (HostState, error)
    State(ctx context.Context) (HostState, error)
    Send(ctx context.Context, content string) error
    Interrupt(ctx context.Context) error
    Stop(ctx context.Context) error
    Rebind(ctx context.Context, req RebindRequest) (HostState, error)
    Attach(ctx context.Context) (AttachInfo, error)
    Snapshot(ctx context.Context) (terminal.Snapshot, error)
    Events() <-chan HostEvent
    Close() error
}
```

`SessionHost` 不应把 PTY 设为必选方法。无终端 Agent 可以返回不支持，或者使用不同的 attach/control surface。

## 14. 分阶段实施计划

### Phase A：抽象当前 Daemon 直接管理的 runtime

- 提取 Host-like interface；
- 把 PiManager/PTYManager 的 process key、transport、observer 边界整理清楚；
- 为每个 managed Session 定义 host ID 和 metadata schema；
- 不改变现有 Server API。

### Phase B：实现 per-session Host

- 新增 `agora session-host` 内部可执行入口；
- Host 独立启动 Pi/Claude；
- 实现 control socket、attach socket 和 token handshake；
- Agent child exit 触发 Host cleanup；
- Daemon 创建 Host 并通过 control client 管理它。

### Phase C：接管和 Daemon 重启

- Daemon 启动扫描 runtime registry；
- 校验 PID/start time/token/identity；
- 重新连接 Host；
- 恢复 observer、history locator、Server resync；
- wrapper 重新获取或自动重连 attach socket。

### Phase D：rebind 和升级体验

- 将 `/resume` rebind 放到 Host + Daemon 的双层状态机；
- 支持 Daemon graceful handoff；
- 增加 Host protocol version negotiation；
- 增加 stale cleanup、quarantine 和诊断命令。

### Phase E：服务托管

- Linux 使用 `systemd --user` 托管 Daemon；
- macOS 使用 `launchd` 托管 Daemon；
- Session Host 不需要成为用户可见的独立服务，但必须具备独立进程生命周期；
- 升级流程先启动新 Daemon，再接管旧 Host，最后退出旧 Daemon。

## 15. 验收标准

至少覆盖以下场景：

1. 创建一个 Pi Session，确认一个 Host 和一个 Agent child；
2. 创建多个 Session，确认 Host 之间互不影响；
3. Daemon 正常重启，Agent PID、native ID、PTY 和 wrapper 会话保持；
4. Daemon 被 kill 后重新启动，能够接管仍存活的 Host；
5. Server 重启，Daemon resync 后 Session route 恢复；
6. Agent 自己退出，Host 发送 exit、清理 socket/metadata 并自行退出；
7. 用户 Stop，Host 正确终止 Agent 且重复 Stop 幂等；
8. Host 异常退出，新 Daemon 能识别 stale metadata，不重复启动未知 Agent；
9. `/resume` context switch 不重启 Agent，完成 native ID、history locator、observer、route 和 UI selection rebind；
10. 旧 Daemon 的延迟 rebind 不覆盖新 binding；
11. 未授权用户无法通过本机 socket 发送 input/stop/attach；
12. 无 PTY 的 provider 可以使用同一 Host lifecycle，但不需要伪造 terminal surface；
13. runtime metadata 不包含 transcript、完整 PTY 内容、普通输入正文或明文长期 credential；
14. 并发创建、重连、Stop、Agent exit 和 cleanup 不留下重复 Host、孤儿 socket 或错误 running Session。

## 16. 结论

Session Host 的核心原则是：

> **Daemon 是控制面连接器，Session Host 是 Agent 运行态所有者。**

每个 managed Session 使用一个独立 Host。Daemon 重启只重建控制面连接，不重启 Agent；Agent 退出则由 Host 完成退出通知和资源清理后自行退出。这样可以同时满足：

- Daemon 可重启、可升级、可重连；
- Agent 和 wrapper 的生命周期稳定；
- 单 Session 故障隔离；
- `/resume` 在同一运行态内完成 rebind；
- Pi、Claude 和未来其他 Agent 共享 provider-neutral 的 Session 生命周期模型。
