import { useLayoutEffect, useRef } from "react";
import { Alert, Card, Empty, Tag, Typography } from "antd";
import type { PTYSnapshot } from "../types";

const { Text } = Typography;

type TerminalSnapshotProps = { agent?: string; snapshot: PTYSnapshot | null; error: string; loading: boolean };

function looksLikeApprovalPrompt(snapshot: PTYSnapshot) {
  const text = snapshot.lines.join("\n").toLowerCase();
  return text.includes("do you want to allow claude") || (text.includes("claude wants to") && /(?:\n|^)\s*❯?\s*[123]\.\s/.test(snapshot.lines.join("\n")));
}

export function TerminalSnapshot({ agent, snapshot, error, loading }: TerminalSnapshotProps) {
  const approvalVisible = snapshot ? looksLikeApprovalPrompt(snapshot) : false;
  const screenRef = useRef<HTMLPreElement>(null);
  const stickToBottom = useRef(true);
  const agentName = agent === "pi" ? "Pi" : "Claude";

  useLayoutEffect(() => {
    const screen = screenRef.current;
    if (screen && stickToBottom.current) screen.scrollTop = screen.scrollHeight;
  }, [snapshot?.sequence]);

  const handleScreenScroll = () => {
    const screen = screenRef.current;
    if (!screen) return;
    stickToBottom.current = screen.scrollHeight - screen.scrollTop - screen.clientHeight < 32;
  };

  return <Card size="small" className="terminal-panel" title={`原生 ${agentName} TUI`} extra={snapshot && <Text type="secondary">屏幕 {snapshot.rows} 行</Text>}>
    {approvalVisible && <Alert type="warning" showIcon message="Claude 正在原生终端等待审批" description="Agora 只能观察这个画面，无法代为同意或拒绝。" />}
    {error && !snapshot ? <Empty className="terminal-empty" description={`PTY 观察不可用：${error}`} /> : snapshot ? <pre ref={screenRef} className="terminal-screen" onScroll={handleScreenScroll} aria-label={`原生 ${agentName} 终端画面`}>{snapshot.lines.join("\n")}</pre> : <Empty className="terminal-empty" description={loading ? "等待终端输出…" : "暂无终端画面。"} />}
    {snapshot && <div className="terminal-status"><Tag>{snapshot.alternate_screen ? "备用屏幕" : "主屏幕"}</Tag><Text type="secondary">解析器{snapshot.healthy ? "正常" : "上报了错误"}{snapshot.cursor_visible ? " · 光标可见" : ""} · 可滚动查看当前画面</Text></div>}
  </Card>;
}
