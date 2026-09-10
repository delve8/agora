# 待办事项

> 这里记录已经确认、但还没有修复的问题。每项包含现象与证据、影响、建议方案和验收标准。
> 修完一项后请连同证据一起更新或删除对应条目。

当前没有未处理条目。

已完成的条目（保留摘要，便于回溯）：

- **Daemon history discovery 全量扫描导致常态高 CPU**：`PiHistoryCatalog` 只按 `size + mtime`
  变化重新解析 transcript，Daemon 复用同一个 catalog 实例。实测 95MB / 57 个文件：
  冷启动 1.9s，缓存命中 ~1ms（此前每次 0.8–3.9s），发现单文件变化时 3ms。
- **Session Host 的进程组与信号语义**：`Spawn` 让 Host 进入独立 session，Host 入口忽略
  SIGINT/SIGHUP；只有显式 `stop`/`shutdown`、SIGTERM 或 Agent 自身退出才结束。
- **runtime registry 的 stale 清理**：Daemon 接管时清理无 metadata 的目录、终态目录，
  以及进程已消失的目录和对应 socket；活跃但无响应的 Host 一律保留。
- **managed 会话在 transcript 不存在时宣称可读历史**：`can_read_history` 跟随
  `history_path`，UI 对这类会话显示"该会话还没有历史记录"而不是空对话。
- **provider 结构化 session 事件**：Host 注入 `pi -e agora-session-reporter.ts`，由 Pi 的
  `session_before_switch` / `session_start` 做精确 rebind，证据式检测作为 fallback。
