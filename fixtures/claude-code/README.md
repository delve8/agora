# fixtures/claude-code

该目录存放经脱敏的真实 Claude Code 运行样本，供解析器、状态机和 UI 回放测试使用。

## 采集方式

使用 `scripts/claude-code-probe/run.sh` 生成原始样本，筛选后放入本目录。原始样本在 `/tmp/agora-probe-<timestamp>/`，不得提交。

## 提交要求

- 每个样本文件必须包含来源注释：CLI 版本、执行命令、采集日期；
- 禁止包含 API key、认证 token、个人绝对路径、真实用户名、真实代码正文或敏感对话；
- 文件名建议：`<cli-version>-<scenario>.stdout.jsonl`；
- 场景如 `01-empty`、`02-minimal-user`、`04-real-turn`、`05-session-resume`。

## 当前状态

- 尚无已提交样本（Phase 0 探测中）。

## 版本

样本格式与 CLI 版本强相关。`claude` 在 2026-08 周期内版本更新较快，样本必须在更新 Adapter 解析器时重新采集或显式标注兼容版本。
