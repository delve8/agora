import { Card, Descriptions, Empty, Tag, Typography } from "antd";
import type { Session } from "../types";

const { Text, Title } = Typography;

export function MemberPanel({ session, selected, onSelect }: { session?: Session; selected: boolean; onSelect: () => void }) {
  if (!session) return <Card className="member-card"><Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="No session yet" /></Card>;
  return <Card className={`member-card ${selected ? "member-selected" : ""}`} onClick={onSelect} hoverable>
    <Text type="secondary">MANAGED MEMBER</Text>
    <Title level={3}>{session.display_name}</Title>
    <Tag color={session.state === "running" ? "success" : session.state === "failed" ? "error" : "default"}>{session.connection || session.state}</Tag>
    <Descriptions className="member-details" column={1} size="small" colon={false}>
      <Descriptions.Item label="Agent">{session.agent}</Descriptions.Item>
      <Descriptions.Item label="Role">{session.role || "unassigned"}</Descriptions.Item>
      <Descriptions.Item label="Workspace"><Text ellipsis={{ tooltip: session.workspace }}>{session.workspace}</Text></Descriptions.Item>
      <Descriptions.Item label="Session ID"><Text copyable={{ text: session.claude_session_id || session.id }} ellipsis={{ tooltip: session.claude_session_id || session.id }}>{session.claude_session_id || session.id}</Text></Descriptions.Item>
    </Descriptions>
    {session.last_error && <Text type="danger">{session.last_error}</Text>}
  </Card>;
}
