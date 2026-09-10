# 待办事项

> 这里记录已经确认、但还没有修复的问题。每项包含现象与证据、影响、建议方案和验收标准。
> 修完一项后请连同证据一起更新或删除对应条目。

## 1. Daemon history discovery 全量扫描导致常态高 CPU

**现象与证据**

- `internal/daemon/daemon.go` 的 `historyDiscoveryInterval = 2 * time.Second`，`discoverHistory` 每 2s 调用一次 `refreshHistory`。
- `refreshHistory` → `manager.DiscoverHistorySessions` → `listCatalogSessions` → `adapter.PiHistoryCatalog.List`。
- `List` 会遍历整个 `~/.pi/agent/sessions`，并对每个 `*.jsonl` 调用 `scanPiHistorySummary`，逐个文件逐行 JSON 解析全部内容（`internal/adapter/pi.go`），即使文件完全没有变化。
- 本机实测：57 个 transcript、94MB，单次 `List` ≈ 0.82s；每 2s 一次 → 稳态约 40% CPU（`ps -o %cpu` 实测 30–60%，`ps -o time` 在 10s 内增长约 5.6s）。
- 同样的全量 `List` 还被 `switchCatalog`（context switch watcher）复用，watcher 侧已加 TTL 共享，但 discovery 侧仍是每次全量解析。

**影响**

- Daemon 常态占用接近半个核，造成耗电、发热，并与 Server resync、observer、watcher 抢 CPU。
- 成本随 transcript 数量与体积线性增长，会话越多越严重。
- 目前只是性能问题，不影响功能正确性。

**建议方案**

- 引入「stat 门槛 + 增量解析」：只为 `mtime`/`size` 发生变化的文件重新解析，并为每个文件保存解析游标（byte offset / line / last id），避免重复解析历史内容。
- catalog 快照加 TTL（目录布局已确认可复用），并在 daemon discovery、`switchCatalog`、observer 之间共享同一份快照。
- 新增文件可以通过目录列表差集发现，不需要解析任何旧文件内容。
- 最低成本的过渡方案：把 discovery 间隔从 2s 调整到 15–30s，并且在文件集合与 `mtime`/`size` 未变化时直接跳过解析。

**验收标准**

- 在 90MB 级别的 Pi catalog 上，Daemon 空闲稳态 CPU < 5%。
- 新增或修改 transcript 后，Server resync 仍能在 5s 内反映变化（保持现有可观测行为）。
- 增加回归测试或 benchmark：catalog 未变化时不得重复解析文件内容。

## 2. runtime registry 的 stale 清理

**现象与证据**

- `~/.agora/runtime/sessions` 曾累积 1065 个只有 `launch.json` 的中断残留目录（已手动清理）。
- 另有 Host 已经退出、但 `metadata.json` 仍记录 `state: running` 的目录（例如 `host-1789029852404460000`），需要人工判断才能安全删除。
- `$TMPDIR/agora-host` 下的 socket 也会随中断残留。

**影响**

- registry 目录与 socket 无界增长；`Adopt` 需要扫描无效条目，stale 条目容易被误认为仍在运行。

**建议方案**

- Daemon 启动扫描 registry：删除缺少 `metadata.json` 的目录、Host PID 已退出且状态非 running 的目录，并清理对应 socket。
- `metadata.json` 使用原子写（临时文件 + rename），避免把写入中断的目录误判成有效 Host。
- 清理逻辑必须做 PID/启动时间/ownership 校验，避免误删仍存活的 Host。

**验收标准**

- 反复启动/中断 Daemon 后，registry 中不再残留无效目录或孤儿 socket。
- 仍存活的 Host 在任何情况下都不会被清理。

## 3. managed 会话不应在 transcript 尚不存在时宣称可读历史

**现象与证据**

- hosted managed 会话（`internal/runtime` 的 `managedHostCapabilities` / `piCapabilities`）无条件返回 `CanReadHistory: true`。
- 但 Agora 创建的 managed 会话在 provider 真正落盘前没有对应 transcript：`pi --session-id <new-uuid>` 只在第一条消息持久化时才创建 JSONL。
- 结合 §8.1.1 的结论（context switch 本身不落任何记录），`/resume` 之后、下一条消息之前，Agora 仍持有旧的 native ID，history 请求必然返回空。
- 结果：UI 显示一个空的会话，看起来像历史丢失，而不是"该会话还没有历史"。

**影响**

- 用户误判为 rebind 失败或历史损坏；每次 `/resume` 后都会出现一次。
- 属于展示层的诚实性问题，不影响数据正确性。

**建议方案**

- 在 capabilities 计算中区分两种情况：
  - `AgentSessionID` 已有对应 transcript（`HistoryPath` 非空）→ `CanReadHistory: true`；
  - managed 会话但 transcript 尚不存在 → `CanReadHistory: false`，并让 UI 显示"等待该会话的第一条消息"之类的提示。
- rebind 成功后 `HistoryPath` 被填上，capabilities 自然变为 true，UI 自动恢复。
- 注意保留历史会话（`SourceHistory`）与已 resumable 会话的现有行为，不要把它们降级。

**验收标准**

- 新建 managed 会话在第一条消息落盘前，UI 不显示空历史，而是明确的"暂无历史"状态。
- 第一条消息落盘（或完成 rebind）后，history 正常可读。
- 历史会话与 resume 后的会话能力不变。

## 4.（长期/可选）为 provider 引入结构化的 context switch 信号

**背景**

`/resume` 不写 transcript、也不写任何状态文件（见 `docs/session-host.md` §8.1.1），所以当前只能靠"下一条消息 + transcript 增量"这种证据式推断。它刻意 fail-closed：证据不足就不切换。

**残留脆弱点**

- 切换后用户没有发消息 → 无法切换（这是固有限制，不是 bug）；
- 极短消息（runes < 4）在输入法噪声下只能精确相等才匹配，被破坏时不会切换；
- 证据比较依赖短语重合度（4-rune 窗口，覆盖率 ≥ 50%），是统计性的。

**建议方向**

- 评估 Pi `--mode rpc`（或 SDK/等价接口）是否暴露 session/context 变更事件；若有，则把它作为**权威触发**，TUI 证据式检测降级为 fallback。
- 同类 provider（Claude Code、OpenCode 等）按 `docs/agent-integration.md` 的约定：有结构化 context/session 变更事件时优先消费。
- 明确非目标：不用屏幕指纹/OCR 之类的启发式来推断当前 session。

**验收标准**

- 结构化信号可用时，切换在用户发消息前即可被识别（或至少与消息触发结果一致且无重复 rebind）。
- 结构化信号不可用时，行为与当前证据式实现一致。
- 文档明确每个 provider 走哪条路径。
