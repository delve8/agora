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
- **`pi <args>` 创建会话后 attach 报 `session not found`**：wrapper 转发参数时
  `PiManager.Command` 不会附加 `--session-id`，Pi 于是自己生成 native id；注入的
  reporter 在 create 返回后立刻原子 rekey，Server 仍用 create 拿到的旧 canonical id
  发 attach，Daemon 的 `GetSession` 因此报 `session not found`（`pi update`、`pi version`
  均复现）。修复一：`Manager` 记录 rekey 重定向表（有界、单跳、防环），`GetSession`
  先解析重定向，attach 用解析后的 id 取 socket；SessionCreate 若发现会话已被
  provider 报告 rekey，则直接采用该身份。回归测试：
  `internal/runtime/rekey_alias_test.go`，去掉重定向解析后
  `TestAttachFollowsProviderRebindAfterCreation` 复现原文 `session not found`。
- **`pi update` 等 Pi CLI 子命令被当成 Agent 会话**：升级子命令是一次性、非交互的，
  包成托管会话既产生垃圾 session（日志里 history 计数随 `pi version`/`pi update` 递增），
  又会在命令提前退出时让 wrapper 报 socket 错误。修复二：`agora wrap pi` 在联系 Daemon
  前先用 `piRunsAnAgentSession` 判定（子命令集合、一次性开关、`--mode json|rpc`），不是
  会话就返回 exit code 76，PATH wrapper 据此直接 exec 真实 pi 并透传退出码；显式
  session-id attach 与 `pi <prompt>` 不受影响。回归测试：`cmd/agora/pi_wrapper_test.go`。
- **claude wrapper 对任何选项直接报错**：claude 的 argv 由 Daemon 拼装（`--session-id`/`--resume`），
  wrapper 只能贡献初始 prompt，所以 `claude --help`、`claude -p ...` 在 wrapper 下都是硬错误。
  修复：`claudeRunsAnAgentSession` 只承认 prompt 形状，选项与 Claude 自己的子命令一律 exit code 76
  透传真实 claude。子命令清单来源为 Claude Code CLI reference 的 CLI commands 表
  （docs.claude.com/en/docs/claude-code/cli-reference，对照 claude-code 2.1.250，含
  `plugins`/`kill` alias），手工维护；本机 native binary 被 macOS 签名拦截（`Killed: 9`），
  无法用 `claude --help` 校对，因此清单在代码注释里标注了来源与版本。回归测试：
  `cmd/agora/wrapper_test.go`。
- **托管会话里 Shift+Enter 与粘贴换行失效（终端能力协商丢失）**：Agent 在任何 client attach
  之前启动，它启动时写的 `\x1b[?2004h`（bracketed paste）和 `\x1b[>7u\x1b[?u\x1b[c`（kitty
  keyboard protocol 协商）既没有接收者也没有应答者。实测（pi 0.85.1，PTY 探针）：无应答时 pi
  不写 `\x1b[>4;2m`（modifyOtherKeys），Shift+Enter 与 Enter 无法区分；Daemon 无 TERM 时颜色从
  truecolor 降为 256 色（`38;2;102;102;102` → `38;5;241`）。iTerm2 3.6.9 本身完整实现了 kitty
  协议（`CSI > N u` / `CSI ? u` / `CSI = N ; m u`，见 `sources/VT100/VT100Terminal.m`），所以问题
  只在于查询根本没有到达终端。修复：Host 充当 Agent 的终端——无 client 时自己应答 `CSI ? u`
  （用 Agent 请求的 flags），跟踪持久模式并在 attach 时回放（bracketed paste、鼠标、focus、
  application cursor keys、kitty flags、modifyOtherKeys）；同时把发起方终端的
  `TERM`/`COLORTERM`/`TERM_PROGRAM` 从 wrapper 经 create 请求带成 Agent 的环境（缺省
  `TERM=xterm-256color`）。回归测试：`internal/sessionhost/terminal_test.go`、
  `internal/sessionhost/host_test.go`（attach 回放）、`internal/runtime/terminalenv_test.go`、
  `internal/runtime/terminalenv_host_test.go`（端到端 env）、`internal/server/wrap_payload_test.go`。
  客户端退出时由 `terminal.ModesReset` 复原这些模式，异常断开也不会把 bracketed
  paste 残留到用户的 shell 里。
- **进程内（legacy）Agent 路径清理**：`m.hosts == nil` 时由 Daemon/Server 进程自己
  fork Agent 的那套路径（`CreateManagedSession`、`createPiSession`、`resumePiSession`、
  Claude 的旧 `resume` 分支、`PTYManager.Launch*/Input/Stop/Snapshot/Rekey`、
  `PiManager.Start*/Send/Stop/Snapshot/Rekey/Events`、`runtime/input.go`、
  attention 检测）已经删除：Agent 进程一律由 `agora session-host` 持有。`agora serve`
  也改为 `manager.EnableSessionHosts(os.Args[0])` 并在启动时 `AdoptSessionHosts`；
  `daemon.Config.SessionHostEnabled` 随之删除（不再是可选项）。两个 provider 退化为
  argv/env 构造器并改名为 `ClaudeProvider` / `PiProvider`。附带修复：`serve` 的
  resume 走 hosted 路径时，`UpdateSessionObservation` 不写 `capabilities_json` 会让
  恢复出来的会话一直显示为"只能读历史"，已补上（`internal/store/sqlite_test.go`）。
  清理后留下的已知缺口（需要时再补）：
  - Claude 的**审批提示 attention** 检测原本只在老路径的 PTY observation 里，现在没有
    producer；Host 侧要重建（`looksLikeApprovalPrompt` 已随老路径删除）；
  - `session.Session.SessionMetaPath` 与 sqlite 的 `session_meta_path` 列是老路径遗留
    字段，不再有人写入，保留是为了不动 schema；
  - `e2e`/`server` 包新增 `TestMain` 以扮演 `session-host`，测试里创建的 Host 需要
    显式停止（Host 按设计比 Daemon 活得久）。
- **`agora update`(工作站自更新）+ 构建注入版本**：以前升级工作站只能重跑配对面板的
  `curl | sh` 或本地 `make install-wrapper` + 手动重启服务。新增 `agora update`
  (`--check` / `--no-restart` / `--force` / `--server` / `--install-dir`)：从已配对的 Server
  取 `/download/install.sh` 并以 `sh -s -- --server … --install-dir … --no-restart` 执行，
  由**同一份安装脚本**完成下载、sha256 校验、原子替换（`install_file`：先写 `dest.tmp.$$`
  再 `mv -f`）、刷新 wrapper 与 `pi`/`claude` 软链、刷新服务定义；重启由 `agora update`
  自己做（`launchctl kickstart -k gui/<uid>/com.delve8.agora.daemon` /
  `systemctl --user restart agora-daemon`），没有服务时只提示。并发用
  `~/.agora/update.lock`（记录 pid，死进程的锁会被接管）；`~/.local/bin/agora` 是指向
  源码 checkout 的软链时拒绝更新（除非 `--force`），避免污染 `make install-wrapper` 的仓库。
  同时加了构建期版本注入：`main.version/commit/date`（Makefile 的 `VERSION/COMMIT/BUILD_DATE`
  与 `-ldflags`，Containerfile 的同名 build-arg、Server 与 daemon 产物共用），
  `agora version [--porcelain]`、`GET /api/version`（Server 自身 + `artifact_version`）、
  `/healthz` 带上版本、`/download/version.txt` 进 allowlist；`agora update --check` 因此能报
  `0.9.0 -> 1.2.3`，没有 `version.txt` 的旧 Server 退化为比 checksum。回归测试：
  `cmd/agora/update_test.go`（check 判定、安装器参数、`--no-restart`、软链拒绝、锁、重启命令、
  version 输出）、`internal/server/version_test.go`、`install.sh` 内容断言。端到端演练：
  用镜像 `--target daemons` 产出的真产物 + 本地 Server，在临时 HOME 下跑
  `agora update --check` 与 `agora update --no-restart`，确认版本比对、下载校验、原子替换、
  wrapper/软链刷新、`9.9.9-check` 版本生效。
