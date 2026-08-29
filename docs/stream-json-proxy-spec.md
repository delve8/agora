# Claude Code IDE Plugin Process Wrapper / stream-json Proxy Spec

> 本文定义 Claude Code 插件 stream-json 的透明代理，属于 Claude-specific provider 文档。通用 Agent 接入边界见 [agent-integration.md](agent-integration.md)。Pi 首选 `--mode rpc`/`--mode json`，不应套用本文的 Claude stream-json 字段、PTY stdin 语义或 `AGORA_CLAUDE_BINARY` 配置；其方案见 [pi-integration.md](pi-integration.md)。本文中可复用的部分仅包括 bounded stdout relay、observation queue、raw JSON preservation 和退出上报。
状态：Draft / implementation gate
最后更新：2026-08-09

## 1. 背景

Cursor/VS Code 中的 Claude Code 插件可以通过 `Claude Code: Claude Process Wrapper` 配置项指定启动 Claude 进程的可执行文件。插件启动的进程不是普通终端 TUI，而是通过 stdin/stdout 使用结构化的 stream-json 协议。

已观察到的启动形态类似：

```text
<configured-wrapper> \
  --output-format stream-json \
  --verbose \
  --input-format stream-json \
  --permission-prompt-tool stdio \
  --resume=<session-id> \
  --setting-sources=user,project,local \
  --permission-mode auto \
  --allow-dangerously-skip-permissions \
  --include-partial-messages \
  --debug --debug-to-stderr \
  --enable-auth-status --no-chrome --replay-user-messages
```

当前 Agora 的 `claude-wrapper` + managed PTY 模型不能直接接入这种已经由 IDE 插件创建的 stream-json 进程：

- PTY 路径面向终端字符、ANSI/VT 和原生 TUI；
- 插件路径面向 stdin/stdout JSON 协议；
- 修改 PATH 或扫描 PID 不能可靠接管已经存在的 stdin/stdout pipe；
- 插件提供 process wrapper 配置，因此可以在进程**启动前**让 Agora 进入启动链路。

本 spec 定义一个透明的 stream-json process wrapper。它不替代 Claude，不改写插件协议，只在插件和真实 Claude binary 之间转发字节，并可旁路观察事件。

## 2. 目标

### 2.1 必须实现

1. 插件把 Agora wrapper 当作 Claude executable 启动；
2. wrapper 接收插件传入的全部参数，并原样传给真实 Claude；
3. 真实 Claude 默认通过当前进程的 `PATH` 查找 `claude`；
4. 支持 `AGORA_CLAUDE_BINARY` 显式指定真实 binary；
5. stdin、stdout、stderr 保持正确的进程语义；
6. stdout 对插件保持协议透明，任何 Agora 日志不得写入 stdout；
7. wrapper 正确处理子进程退出、信号和退出码；
8. Agora 不可用时，透明代理仍可让插件和 Claude 继续工作；
9. 在不阻塞主协议通道的前提下，旁路解析 stream-json 事件；
10. 旁路事件可以进入 Agora 的 Session/Event/SSE 观察体系。

### 2.2 不属于本阶段

- 不接管已经启动的插件进程；
- 不通过 macOS 调试注入、PTY 抢占或修改进程 fd 实现运行后劫持；
- 不修改 Cursor/VS Code 扩展安装目录中的 binary；
- 不默认使用扩展目录内置的 Claude binary；
- 不把 PTY wrapper 改造成 stream-json wrapper；
- 不把 Web/IM 输入默认注入插件代理 session；
- 不自动回复 permission/approval 消息；
- 不修改、过滤、重排或补造插件与 Claude 之间的协议消息；
- 不实现多个插件进程共享一个 Claude 子进程；
- 不承诺不同版本 Claude CLI 的全部参数兼容性。

## 3. 术语

- **Plugin**：Cursor/VS Code Claude Code 插件。
- **Wrapper**：用户在 `Claude Code: Claude Process Wrapper` 中配置的 Agora 可执行文件。
- **Real binary**：wrapper 启动的真实 `claude` 可执行文件。
- **Main channel**：插件与 Claude 之间的 stdin/stdout/stderr 进程通道。
- **Observation channel**：wrapper 将 stdout 的副本解析后发送到 Agora 的旁路通道。
- **Proxy session**：由 wrapper 代表一个插件 Claude 进程创建的 Agora 观察 Session。
- **stream-json**：Claude CLI 使用的 stdin/stdout 结构化消息流。当前只对已观察到的参数和事件形态做兼容承诺，未知字段必须保留并透明转发。

## 4. 总体架构

```text
Cursor / VS Code Claude Code Plugin
        │
        │ configured executable: /path/to/agora claude-proxy
        │ stdin/stdout: stream-json
        ▼
Agora Claude Process Proxy
        │
        ├── resolve real claude binary
        ├── exec real claude with original args
        ├── stdin  ───────────────→ real claude stdin
        ├── real claude stdout ───→ plugin stdout
        │             └────────────→ observation parser
        ├── real claude stderr ───→ plugin stderr
        └── child lifecycle/signals

Observation channel (best effort):

proxy ── localhost HTTP/Unix channel ──→ agora serve
                                      ├── Session
                                      ├── SQLite events
                                      └── SSE/Web observation
```

### 4.1 两种接入模式并存

| 维度 | Managed PTY | IDE stream-json proxy |
|---|---|---|
| 入口 | `agora wrapper` / Web | IDE 的 Process Wrapper 配置 |
| Claude owner | Agora | IDE wrapper 链路中的代理 |
| 输入 | PTY 字节 | stdin JSON |
| 输出 | PTY/ANSI/VT | stdout stream-json |
| TUI | 保留 | 不处理，由 IDE 负责 |
| 观察 | JSONL + PTY snapshot | stdout 旁路解析 |
| 既有会话接管 | 不支持外部导入 | 不支持运行后接管 |
| 真实 binary | Agora 配置或 PATH | `AGORA_CLAUDE_BINARY` 或 PATH |

两种模式共享 Event、Session、Store 和 SSE 抽象，但不得共享 PTY 输入逻辑。

## 5. Wrapper 启动契约

### 5.1 配置方式

插件配置项填写 Agora wrapper 的**绝对路径**，例如：

```text
/Users/<user>/bin/agora claude-proxy
```

如果插件配置只接受单个 executable path，则应编译独立入口：

```text
/Users/<user>/bin/agora-claude-proxy
```

第一版实现必须明确支持独立 executable；是否支持把 `agora` 与子命令拆成两个配置字段取决于插件行为，不作为前置假设。

### 5.2 参数透传

wrapper 从 `os.Args[1:]` 获取插件传入的参数。除 wrapper 自己明确规定的参数外，所有参数必须按原顺序、原字符串值传给真实 Claude：

```text
plugin args ──exactly unchanged──→ exec.Command(realBinary, args...)
```

包括但不限于：

- `--output-format stream-json`；
- `--input-format stream-json`；
- `--verbose`；
- `--resume=<id>`；
- `--permission-prompt-tool stdio`；
- `--permission-mode auto`；
- `--allow-dangerously-skip-permissions`；
- `--include-partial-messages`；
- `--debug-to-stderr`；
- 插件未来增加的未知参数。

wrapper 不得：

- 通过 shell 拼接命令；
- 删除未知参数；
- 将 `--resume=<id>` 改写为其他 session；
- 把 prompt 或 JSON 输入改成命令行参数；
- 依赖参数中出现真实 binary 路径。

### 5.3 Real binary 解析

解析优先级固定为：

1. `AGORA_CLAUDE_BINARY` 非空时使用其值；
2. 否则使用当前 wrapper 进程环境中的 `PATH` 执行 `exec.LookPath("claude")`；
3. 两者均失败时，向 stderr 输出可诊断错误并以非零状态退出。

默认不搜索或回退到：

- Cursor 扩展目录；
- VS Code 扩展目录；
- 任意固定版本目录；
- 当前工作目录中的同名文件（除非它已通过 PATH 解析）。

如果 `AGORA_CLAUDE_BINARY` 包含路径分隔符，应按显式路径处理，并在启动前验证文件存在且可执行。

wrapper 启动时可以在 stderr 输出实际解析路径和 CLI 版本摘要，但不得输出到 stdout，也不得默认记录完整 prompt、token 或协议正文。

### 5.4 环境变量

stream-json proxy 默认继承插件的环境变量，不复用 managed PTY 的 `cleanClaudeEnv()` 清理策略。

原因：插件注入的 Claude/Cursor session 标识可能是其自身会话管理的一部分；proxy 的职责是代理插件启动的进程，而不是伪装成一个由 Agora 独立创建的 managed session。

因此：

- 不得无条件删除 `CLAUDE_CODE_CHILD_SESSION`、`CLAUDE_CODE_SESSION_ID`、`CLAUDE_PID`、`CURSOR_*` 等变量；
- 不得擅自注入新的 Claude session id；
- 不得把 proxy 的 Agora session id 当作 Claude 的 `--resume` id；
- `AGORA_CLAUDE_BINARY`、`AGORA_ADDR` 等 Agora 配置变量可以被 wrapper 使用；
- 环境中的 API key 和认证信息只按插件原有行为传给真实 Claude，不写入日志。

这与 `internal/runtime/ptymanager.go` 中“Agora 自己启动 managed Claude 时清理父会话环境”的策略是有意不同的，必须在实现和文档中保持区分。

## 6. Main channel 透明性

### 6.1 stdout 零污染是最高优先级

插件的 stdout 只能收到真实 Claude stdout 的原始字节。以下内容禁止进入 stdout：

- Agora 日志；
- binary 解析日志；
- HTTP 上报状态；
- JSONL/SQLite 调试信息；
- panic stack；
- 代理启动提示；
- 任何自行生成的 JSON 行。

所有 wrapper 日志必须使用 stderr 或独立日志文件。默认使用 stderr，推荐统一前缀：

```text
[agora-proxy] ...
```

### 6.2 stdout 转发

代理读取真实 Claude stdout 后：

1. 将原始字节写入 wrapper stdout；
2. 将同一份字节复制给旁路 parser；
3. 不等待 Agora HTTP 请求完成后才写 stdout；
4. 不因为 parser 解析失败而丢弃或修改主通道字节；
5. 不对 JSON 重新序列化。

必须验证以下属性：

```text
wrapper stdout == direct real-claude stdout
```

在相同环境和参数下，允许进程时序差异，但代理不得改变协议字节内容。

### 6.3 stdin 转发

wrapper stdin 原样转发到真实 Claude stdin。第一版不解析、不改写、不注入 stdin 内容。

如果未来需要从 Agora Web/IM 输入，必须新增显式授权和输入仲裁协议，不得在透明 proxy 中偷偷复用现有 `/api/sessions/{id}/messages` 直接写入。

### 6.4 stderr 转发

真实 Claude stderr 原样转发到 wrapper stderr。Agora 自己的诊断日志也写 wrapper stderr，但应使用明确前缀以便区分。

## 7. Observation channel

### 7.1 旁路原则

观察是 best effort，主协议是 authoritative：

- parser 失败不影响插件和 Claude；
- Agora server 不可用不影响插件和 Claude；
- HTTP 超时不阻塞 stdout；
- 有界队列满时可以丢弃观察事件，但必须在 stderr 记录计数；
- 不把 parser 的结构化结果重新写回 stdout。

### 7.2 解析边界

复用 `internal/adapter/stream_json.go` 的 `ParseStreamEvent`，并逐步补充真实 fixture 测试。

parser 应：

- 按换行处理 NDJSON/stream-json 输出；
- 保留完整原始行作为 `event.Event.RawJSON`；
- 识别 `system`、`assistant`、`user`、`tool_use`、`tool_result`、`result` 等已知事件；
- 对 `result.is_error` 归一化为 error；
- 对 `system/thinking_tokens` 遵循现有过滤规则；
- 对未知 `type` 保留原始 JSON 并采用可观察的降级类型；
- 对不合法 JSON 仅记录诊断，不阻断转发。

parser 的单行缓冲必须有上限。超过上限时：

- 主通道继续转发；
- 该行旁路解析失败；
- stderr 记录原因和字节数；
- 不把完整内容写入日志。

### 7.3 Proxy Session

每个 wrapper 进程最多对应一个 proxy session；一个 proxy session 只对应一个真实 Claude 子进程。

proxy session 至少记录：

- Agora session id；
- Agent = `claude-code`；
- Source = `proxy`；
- Workspace；
- wrapper PID 和 real Claude PID（如果可得）；
- `--resume` 值（只作为关联信息，不改写）；
- running/stopped/failed 状态；
- 事件和退出信息（事件默认仅实时转发；有可定位的 Claude JSONL 时才支持历史回读）；
- 创建与结束时间。

proxy session 的输入能力默认是：

```text
CanObserve = true
CanStream = true
CanReadHistory = false or best-effort
CanSendInput = false
CanApprove = false
```

插件 stdin 是插件到 Claude 的主通道，不等同于 Agora 授予 Web 的输入权限。

### 7.4 与现有事件模型集成

旁路事件归一化为现有 `event.Event`：

- `Source = event.SourceStream`；
- `SessionID = Agora proxy session id`；
- `ExternalID = stream-json uuid`（如果存在）；
- `RawJSON = 原始事件行`；
- `Kind/Content/Subtype` 复用现有 parser 结果。

事件通过有界内存去重后直接发布到现有 SSE，不写入 SQLite。proxy 不需要 JSONL observer 才能实时观察；如果 `--resume` 对应的 Claude JSONL 可定位，历史请求可以按需解析该原始文件。无法定位 JSONL 的 proxy session 是 live-only，进程或 Agora 重启后不提供历史回放。

## 8. Agora server side-channel

### 8.1 约束

现有 HTTP server 默认监听 `127.0.0.1:8080`。proxy side-channel 只能面向本机 Agora server，不能为了代理而新增公网监听。

具体 API 路径在实现前必须固定，并与现有 session/coordination 模型对齐。推荐最小接口：

```text
POST /api/proxy/sessions
POST /api/proxy/sessions/{id}/events
POST /api/proxy/sessions/{id}/exit
```

### 8.2 注册

注册请求包含：

- workspace；
- display name；
- wrapper PID；
- real Claude PID（可选）；
- 参数摘要（不得包含 prompt、token 或完整敏感内容）；
- `--resume` 关联信息（可选）；
- 协议模式标识。

服务端返回 Agora proxy session id。注册失败时 proxy 仍继续主通道，只在 stderr 报告观察不可用。

### 8.3 事件上报

事件上报可以批量发送。批量大小和 flush 时间必须有界。服务端：

- 校验 session 存在；
- 使用有界内存中的 `ExternalID + Source` 做进程生命周期内幂等；
- 直接发布现有 SSE；
- 不把事件写入 SQLite；
- 不把事件转发给其他 Claude session；
- 不把旁路事件解释成用户授权。

### 8.4 退出上报

子进程退出后 proxy 尽力上报：

- exit code；
- signal（如果有）；
- failed/stopped 状态；
- 最后一个可观测时间；
- 旁路丢弃计数。

上报失败不改变已经确定的子进程退出码。

## 9. 进程生命周期与信号

### 9.1 启动顺序

1. 读取 wrapper 自身配置；
2. 解析 real binary；
3. 准备可选 observation sink；
4. 启动 real Claude；
5. 建立 stdin/stdout/stderr 转发；
6. 开始旁路解析；
7. 运行直到 real Claude 退出。

注册是否必须发生在子进程启动前，由实现根据 server API 决定；无论如何，Agora 注册失败都不得阻塞主进程超过有限超时。

### 9.2 信号

至少处理：

- SIGINT；
- SIGTERM；
- SIGHUP（平台支持时）。

代理收到信号后应将对应信号转发给 real Claude，并等待其退出。不得先让 wrapper 自己退出而遗留 Claude 子进程。

### 9.3 退出码

- real Claude 正常退出：wrapper 返回相同退出码；
- real Claude 被信号终止：wrapper 采用平台约定的 signal exit status；
- real binary 找不到、无法启动或 wrapper 参数错误：wrapper 返回非零错误；
- Agora HTTP/观察失败：不改变 real Claude 的退出结果；
- 代理清理超时：先记录 stderr，再按明确的强制终止策略结束子进程。

## 10. 安全边界

插件命令可能包含：

```text
--permission-mode auto
--allow-dangerously-skip-permissions
```

因此 wrapper 位于一个可能执行高权限工具调用的进程前面。实现必须遵守：

1. 不把 stream-json 原文默认写入日志；
2. 不把 API key、认证 token、文件正文写入日志；
3. 不把 Agora HTTP API 暴露到非 loopback 地址；
4. side-channel 至少验证来源，推荐使用一次性本机 token 或 Unix socket；
5. proxy session 不允许客户端通过 session id 横向访问其他 session；
6. Web/IM 默认不能向 proxy session 注入输入；
7. 不自动回复 permission prompt；
8. 不把普通文本、事件或 IM 通知解释成审批结果；
9. 记录可审计的 session 注册、退出和观察失败信息；
10. 任何未来的控制 API 必须有单独的授权设计，不能因为已有 localhost 就视为安全。

## 11. 兼容性与降级

### 11.1 CLI 版本

插件和 PATH 中的 Claude CLI 可能不是同一版本。wrapper 不负责把新参数降级为旧参数：

- 参数原样传递；
- 由真实 Claude 报告 unknown option；
- wrapper 将错误保留在 stderr；
- spec/fixture 记录插件版本与 CLI 版本。

### 11.2 插件重启

插件重启通常会重新启动 wrapper。每个 wrapper 进程独立处理自己的 stdin/stdout 和子进程；不得依赖全局单例或复用旧 pipe。

### 11.3 Agora 不运行

如果 `agora serve` 没有启动：

- wrapper 仍尝试运行真实 Claude；
- 观察功能降级为不可用；
- stdout/stderr 主通道不受影响；
- stderr 给出一次性可诊断信息；
- 不在后台无限重试或阻塞插件启动。

## 12. 测试与验收

### 12.1 Unit tests

必须覆盖：

- `AGORA_CLAUDE_BINARY` 优先于 PATH；
- PATH 找不到 binary 的错误；
- 参数顺序和值原样透传；
- stdin 字节透传；
- stdout 字节透传；
- stderr 字节透传；
- Agora 日志不会进入 stdout；
- 退出码透传；
- SIGINT/SIGTERM 转发；
- parser 失败不影响主通道；
- 超大 JSON 行有界处理；
- observation sink 失败不影响 Claude；
- 事件批量与退出 flush。

应为 `internal/adapter/stream_json.go` 补充测试，覆盖：

- assistant text；
- thinking；
- user；
- tool_use/tool_result；
- result；
- result error；
- system/thinking_tokens；
- 空 content 的 result；
- 未知事件；
- malformed JSON。

### 12.2 Integration tests

使用假的 `claude` executable：

1. 读取 stdin；
2. 输出固定的 stream-json fixture；
3. 输出 stderr 标记；
4. 以指定状态退出。

验证：

- wrapper 输出和 fake Claude 输出字节一致；
- plugin 参数全部到达 fake Claude；
- Agora sink 收到规范化事件；
- sink 不可用时 wrapper 仍返回 fake Claude 的退出码；
- 信号能结束 fake Claude；
- 多个 wrapper 进程之间没有状态串扰。

### 12.3 Real CLI validation

使用 PATH 中的真实 `claude`，不要默认使用插件扩展目录 binary：

```bash
CLAUDE_BIN="$(command -v claude)"
AGORA_CLAUDE_BINARY="$CLAUDE_BIN" \
  agora claude-proxy \
  --output-format stream-json \
  --input-format stream-json \
  --verbose
```

验证：

- 最小 user stream-json 输入可被真实 CLI 接收；
- `--resume` 参数不被改写；
- `--permission-prompt-tool stdio` 不被改写；
- 插件连接时协议正常；
- Agora Web 能观察旁路事件；
- Agora 关闭时插件仍可正常与 Claude 通信。

测试必须避免把真实 session id、prompt、API key 或工作区敏感内容提交到仓库。真实样本放在 `/tmp/agora-probe-*`，fixture 脱敏后才可提交。

## 13. 实现顺序

本 spec 是实现门槛。未完成评审前不得写代理代码。

评审通过后按以下顺序实现：

1. 固化命令名和 side-channel API；
2. 新增 `agora claude-proxy` 入口；
3. 实现 PATH / `AGORA_CLAUDE_BINARY` 解析；
4. 实现透明 stdin/stdout/stderr 转发；
5. 实现信号和退出码处理；
6. 实现旁路 parser 与 bounded sink；
7. 增加 proxy session 与 server endpoint；
8. 增加单元和集成测试；
9. 用真实 PATH CLI 验证；
10. 最后在 Cursor/VS Code 插件配置中验证完整链路。

## 14. 决策记录

- **D1：新增 stream-json proxy，不修改 PTY wrapper。** 两者协议和生命周期不同。
- **D2：默认使用 PATH 中的 `claude`。** 不绑定 Cursor/VS Code 扩展目录，也不隐式使用插件内置 binary。
- **D3：参数原样透传。** 兼容性由真实 CLI 决定，wrapper 不猜测或重写未知参数。
- **D4：stdout 只做字节级中继。** 旁路解析不能污染、延迟或改变主协议。
- **D5：观察失败与主通道隔离。** Agora 是观察和协作层，不应成为插件运行的单点故障。
- **D6：不接管已运行进程。** 只有重新启动并经过 Process Wrapper 配置的插件 session 才进入 Agora 代理链路。
- **D7：spec 先行。** 先评审协议、权限和生命周期，再实现代码。
