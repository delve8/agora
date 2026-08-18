import { Alert, Card, Empty, Tag, Typography } from "antd";
import type { PTYSnapshot } from "../types";

const { Text } = Typography;
const TERMINAL_VISIBLE_ROWS = 9;

type TerminalSnapshotProps = { snapshot: PTYSnapshot | null; error: string; loading: boolean };

function visibleTerminalLines(snapshot: PTYSnapshot) {
  if (snapshot.lines.length <= TERMINAL_VISIBLE_ROWS) return snapshot.lines;
  let lastContentRow = -1;
  for (let index = snapshot.lines.length - 1; index >= 0; index--) {
    if (snapshot.lines[index].trim()) {
      lastContentRow = index;
      break;
    }
  }
  const anchorRow = Math.max(snapshot.cursor_row, lastContentRow, TERMINAL_VISIBLE_ROWS - 1);
  const end = Math.min(snapshot.lines.length, anchorRow + 1);
  return snapshot.lines.slice(Math.max(0, end - TERMINAL_VISIBLE_ROWS), end);
}

function looksLikeApprovalPrompt(snapshot: PTYSnapshot) {
  const text = snapshot.lines.join("\n").toLowerCase();
  return text.includes("do you want to allow claude") || (text.includes("claude wants to") && /(?:\n|^)\s*❯?\s*[123]\.\s/.test(snapshot.lines.join("\n")));
}

export function TerminalSnapshot({ snapshot, error, loading }: TerminalSnapshotProps) {
  const approvalVisible = snapshot ? looksLikeApprovalPrompt(snapshot) : false;
  const lines = snapshot ? visibleTerminalLines(snapshot) : [];
  return <Card size="small" className="terminal-panel" title="Native Claude TUI" extra={snapshot && <Text type="secondary">latest {lines.length} rows</Text>}>
    {approvalVisible && <Alert type="warning" showIcon message="Claude is waiting for approval in the native terminal" description="Agora can observe this screen but cannot approve or reject it." />}
    {error && !snapshot ? <Empty className="terminal-empty" description={`PTY observation unavailable: ${error}`} /> : snapshot ? <pre className="terminal-screen" aria-label="Native Claude terminal screen">{lines.join("\n")}</pre> : <Empty className="terminal-empty" description={loading ? "Waiting for terminal output…" : "No terminal snapshot available."} />}
    {snapshot && <div className="terminal-status"><Tag>{snapshot.alternate_screen ? "alternate screen" : "primary screen"}</Tag><Text type="secondary">parser {snapshot.healthy ? "healthy" : "reported an error"}{snapshot.cursor_visible ? " · cursor visible" : ""}</Text></div>}
  </Card>;
}
