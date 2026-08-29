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

  return <Card size="small" className="terminal-panel" title={`Native ${agentName} TUI`} extra={snapshot && <Text type="secondary">screen {snapshot.rows} rows</Text>}>
    {approvalVisible && <Alert type="warning" showIcon message="Claude is waiting for approval in the native terminal" description="Agora can observe this screen but cannot approve or reject it." />}
    {error && !snapshot ? <Empty className="terminal-empty" description={`PTY observation unavailable: ${error}`} /> : snapshot ? <pre ref={screenRef} className="terminal-screen" onScroll={handleScreenScroll} aria-label={`Native ${agentName} terminal screen`}>{snapshot.lines.join("\n")}</pre> : <Empty className="terminal-empty" description={loading ? "Waiting for terminal output…" : "No terminal snapshot available."} />}
    {snapshot && <div className="terminal-status"><Tag>{snapshot.alternate_screen ? "alternate screen" : "primary screen"}</Tag><Text type="secondary">parser {snapshot.healthy ? "healthy" : "reported an error"}{snapshot.cursor_visible ? " · cursor visible" : ""} · scroll to inspect the current screen</Text></div>}
  </Card>;
}
