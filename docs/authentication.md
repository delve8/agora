# Agora 认证与授权规范

> 状态：Design / 产品化一期实现契约
>
> 本文定义 Agora Server 的认证与授权模型：身份来源、运行模式、用户与设备归属、Session 授权，以及 IM 通知链接的只读能力语义。它同时是 `docs/architecture.md`、`docs/protocol.md`、`docs/im-integration.md`、`docs/notifications.md` 与 `docs/spec.md` 中认证相关内容的统一出处。

## 1. 目标与核心原则

Agora 区分三个概念，并坚持把它们分开：

- **认证（authentication）**：证明"当前操作者是谁"。由 Logto 等外部身份服务或 trust-local 本地模式完成。
- **身份（identity）**：归一化后的稳定用户 ID。Agora 只消费并持久化这个 ID，不直接验证口令。
- **授权（authorization）**：该用户能对哪些资源做什么。Agora 依据设备归属和 Session 归属在 Server 侧执行。

核心原则：

1. **身份是声明，不是凭证。** 登录方式（微信扫码、Google、GitHub、企业 SSO 等）全部由身份服务承载；Agora 只把已验证的 `subject` 映射到内部用户，不自己实现密码或社交登录。
2. **Agora 不信任前端或 Daemon 自报的身份字段。** `user_id`、`email`、`display_name`、`daemon_id` 以及 Session 的所有权都只能来自已验证的凭证和 Server 自己维护的归属关系。
3. **通知链接是受限的只读能力，不是登录凭证。** 它允许"无需再次登录打开指定 Session 的观察页"，但绝不授予完整用户权限或任何控制能力。
4. **v1 不提供 admin 角色与复杂 RBAC。** 权限模型只有一条规则：用户只能访问自己设备归属下的 Session。未来出现跨用户管理需求时再引入角色。

## 2. 运行模式

Server 有两种运行模式，通过启动配置切换：

| 模式 | `AGORA_AUTH_MODE` | 监听 | Web 登录 | Daemon 认证 | 说明 |
|---|---|---|---|---|---|
| trust-local | `local`（默认） | **强制 `127.0.0.1`** | 不需要，全部请求视为 `local` 用户 | 不需要 device credential | 本地单机开箱即用 |
| auth | `logto` | 可任意，但必须 TLS | 通过 Logto 授权码 + PKCE | pairing code → device credential | 远程 / 跨设备 / 多人共享 |

### 2.1 trust-local 模式

- 默认值，适合 `agora serve` 或单机 `agora server + agora daemon`。
- 所有 HTTP/SSE 请求的 principal 固定为 `local` 用户；`local` 用户首次请求时惰性创建。
- Daemon 不带 credential 即可连接，所有权不区分用户。
- 仍然保留非 GET `/api/*` 的 **Origin/Referer 同源校验**。trust-local 不等于裸奔：它只是不做用户认证，CSRF 防护是白赚的防御层。
- **安全强制规则**：`local` 模式下如果配置的监听地址不是 loopback，Server 拒绝启动。防止"关认证 + 暴露局域网"。

### 2.2 auth 模式

- 必须配置 Logto tenant：`AGORA_LOGTO_ISSUER`、`AGORA_LOGTO_AUDIENCE`；首次登录自动创建用户可显式开启 `AGORA_LOGTO_PROVISIONING=enabled`。
- 所有 Web/API 请求必须携带经过验证的 Logto access token（或由后续 BFF/session-cookie 演进承载的会话）。
- Daemon 必须完成配对并携带 per-device credential。
- 非 loopback 部署必须使用 HTTPS；Daemon 与 Server 之间必须 `wss://`。

### 2.3 未配置时的行为

`AGORA_AUTH_MODE` 为空等同于 `local`。**不要**把"未配置"当作 auth 模式：本地优先是产品默认，认证是部署远程时才需要显式选择的能力。

## 3. 身份模型

### 3.1 Principal

每个已认证请求解析为一个 `Principal`：

```go
type Principal struct {
    UserID      string // Agora 内部用户 ID
    AuthSubject string // 身份服务提供的稳定 subject（Logto sub）
    Provider    string // "logto" 或 "local"
    DisplayName string
}
```

trust-local 模式下 `Provider="local"`、`UserID="local"`（或首个子项）。

### 3.2 Logto 作为推荐身份服务

Agora 采用 [Logto](https://logto.io) 作为统一身份平台（与 `../hong` 相同的模式）。个人微信扫码是首个目标社交连接器；Google、GitHub 等后续连接器在 Logto 管理台启用，不需要修改 Agora 认证代码。

```text
Browser
  │  Authorization Code + PKCE
  ▼
Logto
  │  access token
  ▼
Agora Web/API
  │  校验 JWT：签名 / iss / aud / exp（JWKS）
  ▼
Agora user_id（由 subject 映射）
```

- 稳定身份是 `(provider="logto", subject=claims.sub)`，**不是 email 或昵称**。
- 首次登录可自动创建 Agora 用户（`AGORA_LOGTO_PROVISIONING=enabled`），但首次登录的 claims **绝不授予 admin 或任何特权**。
- `TokenValidator` 与 `UserLookup` 保持接口抽象；未来可替换为 Auth0、Keycloak、企业 SSO 或其他 OIDC provider，不改变 handler 代码。

### 3.3 前端配置

Web UI 使用 Logto SDK（如 `@logto/react`）：

```text
VITE_LOGTO_ENDPOINT=https://<tenant>.logto.app
VITE_LOGTO_APP_ID=<SPA client id>
VITE_LOGTO_AUDIENCE=<与后端 AGORA_LOGTO_AUDIENCE 一致的 API resource>
```

第一版允许 SPA 持有 access token 并以 `Authorization: Bearer` 调用 API；正式部署方向是收窄为 HttpOnly 会话（见 §5）。

## 4. 控制面数据模型

Server 只持久化控制面数据，业务内容（transcript、Event、Message、PTY snapshot、通知正文）不落库。

```text
users
  id, display_name, email, status, created_at, last_seen_at

user_identities
  user_id, provider, subject, unique(provider, subject)

devices
  device_id, user_id, credential_hash, name, revoked_at, created_at, last_seen_at

pair_codes
  code_hash, user_id, expires_at, consumed_at

link_grants                       # IM 通知链接
  token_hash, user_id, session_id, scope, issued_at, expires_at, revoked_at

web_auth_sessions                 # 后续 BFF/session-cookie 演进
  session_id, user_id, expires_at
```

- 所有凭证与链接 token **只保存哈希**（或带服务端密钥的签名结构），明文只出现在签发瞬间。
- `pair_codes` 一次性使用、短时效、只存哈希。
- `devices` 是归属关系本体：`device_id -> user_id`，可撤销。

## 5. Web 登录与会话

- 推荐做法：Logto OIDC 授权码 + PKCE。
- v1 可直接接受 Logto access token 作为 Bearer；同时把 **HttpOnly + Secure + SameSite=Strict 的会话 cookie** 作为正式方向，由 Server 侧 OIDC/BFF 演进，避免 access token 常驻前端存储。
- 无论哪种 Web 会话形态，**IM 通知链接都必须兑换为独立的受限 HttpOnly cookie**（见 §8），不得复用完整登录态。

## 6. Daemon 设备归属与配对

### 6.1 配对流程

配对 code 必须由**已认证且已登录**的用户签发，并绑定该用户：

1. 用户在 Web UI（已登录）点击"添加设备"；
2. Server 生成一次性 pairing code，`pair_codes` 记录 `code_hash -> user_id`，短时效（如 10 分钟）；
3. 用户在目标机器运行 `agora daemon --pair <code>`（或首次运行向导输入）；
4. Daemon 调用配对接口；
5. Server 验证 code 未过期、未消费，创建 `device_id -> user_id` 并发放随机高熵 device credential；
6. Daemon 将 credential 保存到 `~/.agora/device.credential`，文件权限 `0600`；
7. 之后所有 WebSocket 连接使用该 credential。

**归属在配对时决定，连接时只验证。** 陌生 Daemon 无法自证归属；归属只能由已认证用户在配对时声明。

### 6.2 连接与注册

- Server 用 Bearer credential 查找 `devices` 得到 `device_id -> user_id`；
- `daemon.register` 中声明的 `daemon_id` **必须等于** credential 绑定的 `device_id`，防止一个设备的凭据冒充另一个；
- Web 用户身份**不通过 Daemon 协议传递**；Daemon 连接只承载设备身份，Web 授权由 Server 独立完成。

### 6.3 撤销与轮换

- 删除/撤销设备即删除或失效其 credential，重连被拒；
- 支持 credential 轮换（重新配对或主动换发）；
- 日志和错误响应不得记录 credential 明文。

## 7. Session 授权

授权关系是链式的：

```text
current principal.user_id
  └── 拥有 devices.user_id == principal.user_id
        └── 该 device 上的 sessions（session.daemon_id == device_id）
```

- Session ID 内嵌 `daemon/<daemon_id>/...`，`daemon_id` 是现成的授权 key；
- 每次读取或控制 Session 前，Server 校验 `session.daemon_id -> device.user_id == principal.user_id`；
- trust-local 模式下该检查恒成立（所有请求都是 `local`）；
- 跨用户/跨设备的 `session_id -> daemon_id` 路由请求在 Server 拒绝（protocol.md §7 既有约束）。

## 8. IM 通知链接（read-only capability link）

通知链接是"点开无需再次登录"的实现载体，但它是一个**绑定单个 Session、仅具只读观察能力、短期有效的 bearer capability**。

### 8.1 链接语义

```text
https://agora.example.com/auth/notification-link?token=<opaque-token>
```

- `opaque-token` 高熵不可预测，服务端只存哈希；
- grant 绑定：`session_id`、`user_id`（签发者）、`scope=read_observation`、`issued_at`、`expires_at`、撤销依据（如授权版本号）；
- 签发者必须是已认证且有权读取目标 Session 的用户；
- 推荐 TTL：**10 分钟**。

### 8.2 兑换流程

```text
IM 通知链接
  │
  ▼
/auth/notification-link?token=...
  │  验证 token：未过期、未撤销、session_id 合法且仍归属签发用户
  ▼
设置受限 HttpOnly + Secure + SameSite cookie（scope=read_observation, session_id）
  │
  ▼
302 重定向到不带 token 的 /sessions/<session-id>
```

- 响应设置 `Referrer-Policy: no-referrer`；
- 页面与静态资源不得继续传播 token；浏览器地址栏重定向后不含 token；
- 兑换后的 cookie 生命周期不超过原 grant TTL，且只对该 Session 的只读接口有效。

### 8.3 能力边界

只读 grant 允许：

- 读取指定 Session 的 metadata、history；
- 订阅指定 Session 的 SSE observation；
- 读取只读 PTY snapshot。

禁止：

- `POST /messages`（Session 输入）；
- `resume`、`stop`；
- PTY attach 写入；
- 设备配对 / 撤销；
- webhook 与通知配置；
- 读取其他 Session；
- 任何未来的审批接口。

### 8.4 预取与重放

IM provider、邮件客户端、安全扫描器可能预取链接。因此：

- **不依赖"首次 GET 必须成功并立即永久消费"作为唯一防重放机制**；
- 优先组合：短 TTL + scope 限制 + 服务端撤销/授权版本 + 兑换后清理 URL；
- 若未来需要严格一次性，应提供预览/确认流程，而不是直接消费 GET。

### 8.5 泄露边界

- token 泄露的后果被明确接受为"在 TTL 内获得该 Session 的只读观察能力"；
- 因此通知 target（IM 群、频道、邮箱）必须视为可信接收边界；完整 transcript、workspace 私密路径、凭据和高敏感摘要**不能**放入通知正文；
- token 不写入日志、不回显、不进入通知正文以外的持久化。

### 8.6 本地模式

- trust-local 模式下所有请求已是 `local`，不需要 Web 登录也不需要链接兑换；
- `127.0.0.1` 链接只在本机可达；通知应提示"请在 Agora 所在机器打开"或省略不可用链接，不得宣称本地地址可远程打开。

## 9. 安全不变量

- 认证与授权都必须在 Server 完成，Daemon 协议不携带用户身份；
- 凭证、pairing code、链接 token 只存哈希；明文只出现在签发瞬间；
- 非 GET `/api/*` 恒做同源校验（两个模式都做）；
- `local` 模式禁止非 loopback 监听；`logto` 模式非 loopback 必须 TLS；
- 日志不记录 credential、token、完整 workspace、完整消息正文。

## 10. 与现有文档的关系

- `docs/architecture.md`：本规范的架构落点（职责、数据边界、请求路径）。
- `docs/protocol.md`：Daemon 配对与设备认证的协议细节。
- `docs/im-integration.md` / `docs/notifications.md`：通知链接形态与安全边界。
- `docs/spec.md`：产品化一期范围与验收标准。

## 11. 当前实现映射与落地顺序

当前代码状态：

- Server HTTP API 无认证；Daemon WebSocket 仅支持可选的共享 `AGORA_DAEMON_TOKEN`；
- `docs/protocol.md` 的 pairing 设计未实现；`device.credential` 仅有路径占位；
- Web 登录、`users/devices` 表、通知链接均未实现。

建议落地顺序：

1. trust-local 默认行为与启动强制规则（`local` 模式 + loopback 校验 + Origin 中间件 + `local` principal 注入）；
2. `users` / `user_identities` / `devices` / `pair_codes` 表与配对接口；
3. Logto 接入（`AGORA_AUTH_MODE=logto`、JWKS 校验、provisioning）；
4. Session 归属授权检查；
5. 通知链接签发与兑换端点；
6. BFF/session-cookie 演进与设备撤销/轮换管理界面。
