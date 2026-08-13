import { Alert, Card, Empty, Space, Tag, Typography } from "antd";
import type { PTYSnapshot } from "../types";

const { Text } = Typography;

type TerminalSnapshotProps = { snapshot: PTYSnapshot | null; error: string; loading: boolean };

function looksLikeApprovalPrompt(snapshot: PTYSnapshot) {
  const text = snapshot.lines.join("\n").toLowerCase();
  return text.includes("do you want to allow claude") || (text.includes("claude wants to") && /(?:\n|^)\s*❯?\s*[123]\.\s/.test(snapshot.lines.join("\n")));
}

export function TerminalSnapshot({ snapshot, error, loading }: TerminalSnapshotProps) {
  const approvalVisible = snapshot ? looksLikeApprovalPrompt(snapshot) : false;
  return <Card className="terminal-panel" title={<Space direction="vertical" size={0}><Text type="secondary">LIVE TERMINAL OBSERVATION</Text><span>Native Claude TUI</span></Space>} extra={snapshot && <Text type="secondary">{snapshot.cols}×{snapshot.rows} · #{snapshot.sequence}</Text>}>
    {approvalVisible && <Alert type="warning" showIcon message="Claude is waiting for approval in the native terminal" description="Agora can observe this screen but cannot approve or reject it." />}
    {error && !snapshot ? <Empty className="terminal-empty" description={`PTY observation unavailable: ${error}`} /> : snapshot ? <pre className="terminal-screen" aria-label="Native Claude terminal screen">{snapshot.lines.join("\n")}</pre> : <Empty className="terminal-empty" description={loading ? "Waiting for terminal output…" : "No terminal snapshot available."} />}
    {snapshot && <div className="terminal-status"><Tag>{snapshot.alternate_screen ? "alternate screen" : "primary screen"}</Tag><Text type="secondary">parser {snapshot.healthy ? "healthy" : "reported an error"}{snapshot.cursor_visible ? " · cursor visible" : ""}</Text></div>}
  </Card>;
}
