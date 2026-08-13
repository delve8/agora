import { useEffect, useState } from "react";
import { Alert, Button, Card, Drawer, Layout, List, Space, Tag, Typography } from "antd";
import { MenuOutlined, PlusOutlined, WifiOutlined } from "@ant-design/icons";
import { EventStream } from "./components/EventStream";
import { MemberPanel } from "./components/MemberPanel";
import { MessageComposer } from "./components/MessageComposer";
import { SessionCreate } from "./components/SessionCreate";
import { TerminalSnapshot } from "./components/TerminalSnapshot";
import { useCoordination } from "./hooks/useCoordination";
import { usePTYSnapshot } from "./hooks/usePTYSnapshot";

const { Header, Content, Sider } = Layout;
const { Text, Title } = Typography;

function selectedFromLocation(sessions: { id: string }[]) {
  const match = window.location.pathname.match(/^\/sessions\/([^/]+)/);
  return match && sessions.some((session) => session.id === decodeURIComponent(match[1])) ? decodeURIComponent(match[1]) : "";
}

export default function App() {
  const { coordination, sessions, currentSession, selectedSessionId, setSelectedSessionId, events, loading, error, setError, addSession, send, refresh } = useCoordination();
  const terminal = usePTYSnapshot(currentSession?.id ?? "");
  const [createOpen, setCreateOpen] = useState(false);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);

  const linkedSession = selectedFromLocation(sessions);
  useEffect(() => {
    if (linkedSession && linkedSession !== selectedSessionId) setSelectedSessionId(linkedSession);
  }, [linkedSession, selectedSessionId, setSelectedSessionId]);

  const submit = async (content: string) => {
    if (!currentSession) return;
    try { await send(currentSession.id, content); setError(""); }
    catch (value) { setError(value instanceof Error ? value.message : "Unable to send message"); }
  };
  if (loading) return <div className="loading-screen">Loading Agora…</div>;

  const sessionList = <List
    dataSource={sessions}
    locale={{ emptyText: "No sessions yet" }}
    renderItem={(session) => <List.Item className={session.id === selectedSessionId ? "session-list-item selected" : "session-list-item"} onClick={() => { setSelectedSessionId(session.id); setMobileMenuOpen(false); }}>
      <List.Item.Meta title={session.display_name} description={<Space size={4}><Tag color={session.state === "running" ? "success" : "default"}>{session.state}</Tag><Text type="secondary">{session.role || "agent"}</Text></Space>} />
    </List.Item>}
  />;

  return <Layout className="app-layout">
    <Header className="app-header">
      <Space size="middle"><Button className="mobile-menu-button" type="text" icon={<MenuOutlined />} onClick={() => setMobileMenuOpen(true)} /><div><Text className="eyebrow">LOCAL AGENT MANAGEMENT</Text><Title level={3}>{coordination?.name ?? "Agora"}</Title></div></Space>
      <Space><Tag icon={<WifiOutlined />} color="success">Connected</Tag><Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>New session</Button></Space>
    </Header>
    <Content className="app-content">
      {error && <Alert className="error-banner" type="error" closable message={error} onClose={() => setError("")} />}
      <Alert className="product-notice" type="info" showIcon message="Agora keeps Claude Code running in its native terminal while this page provides a responsive observation and messaging surface." />
      {sessions.length === 0 || createOpen ? <div className="setup-wrap"><SessionCreate onCreate={addSession} onCreated={() => { setCreateOpen(false); void refresh(); }} disabled={!coordination} /></div> : <div className="workspace-grid">
        <Sider className="session-sider" width={272}><Card title="Sessions" extra={<Button type="text" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)} />} bordered={false}>{sessionList}</Card></Sider>
        <main className="session-main">
          <MemberPanel session={currentSession} selected={Boolean(currentSession)} onSelect={() => undefined} />
          <EventStream events={events} />
          <TerminalSnapshot snapshot={terminal.snapshot} error={terminal.error} loading={terminal.loading} />
          <MessageComposer disabled={!currentSession?.capabilities.can_send_input} onSend={submit} />
        </main>
      </div>}
    </Content>
    <Drawer title="Sessions" placement="left" open={mobileMenuOpen} onClose={() => setMobileMenuOpen(false)} width={320}>{sessionList}</Drawer>
  </Layout>;
}
