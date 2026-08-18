import { useMemo, useState, type ReactNode } from "react";
import { Alert, Button, Cascader, Checkbox, Layout, Space, Tooltip } from "antd";
import { HistoryOutlined, PlayCircleOutlined, PlusOutlined, PoweroffOutlined } from "@ant-design/icons";
import { EventStream } from "./components/EventStream";
import { MessageComposer } from "./components/MessageComposer";
import { SessionCreate } from "./components/SessionCreate";
import { TerminalSnapshot } from "./components/TerminalSnapshot";
import { useCoordination } from "./hooks/useCoordination";
import { usePTYSnapshot } from "./hooks/usePTYSnapshot";
import { sessionLabel } from "./sessionName";

const { Header, Content } = Layout;

type SessionOption = { value: string; label: ReactNode; children?: SessionOption[] };

function workspaceLabel(workspace: string) {
  return workspace || "Unknown workspace";
}

function sessionOptionLabel(session: { display_name: string; id: string; capabilities: { can_resume: boolean; can_stream: boolean } }) {
  const label = sessionLabel(session.display_name, session.id);
  if (!session.capabilities.can_resume || session.capabilities.can_stream) return label;
  return <Space size={6}><span>{label}</span><Tooltip title="Session is inactive and can be resumed"><HistoryOutlined className="session-inactive-icon" /></Tooltip></Space>;
}

export default function App() {
  const { coordination, sessions, currentSession, setSelectedSessionId, events, loading, error, setError, addSession, resume, stop, send, refresh } = useCoordination();
  const terminal = usePTYSnapshot(currentSession?.id ?? "", currentSession?.capabilities.can_read_terminal ?? false);
  const [createOpen, setCreateOpen] = useState(false);
  const [showProcessDetails, setShowProcessDetails] = useState(false);
  const [selectorOpen, setSelectorOpen] = useState(false);
  const [selectorPath, setSelectorPath] = useState<string[]>([]);
  const [resuming, setResuming] = useState(false);
  const [stopping, setStopping] = useState(false);

  const sessionOptions = useMemo<SessionOption[]>(() => {
    const byWorkspace = new Map<string, SessionOption>();
    const activity = new Map<string, string>();
    for (const session of sessions) {
      const workspace = workspaceLabel(session.workspace);
      let group = byWorkspace.get(workspace);
      if (!group) {
        group = { value: workspace, label: workspace, children: [] };
        byWorkspace.set(workspace, group);
      }
      group.children!.push({ value: session.id, label: sessionOptionLabel(session) });
      if (session.updated_at > (activity.get(workspace) ?? "")) activity.set(workspace, session.updated_at);
    }
    for (const group of byWorkspace.values()) {
      group.children!.sort((a, b) => {
        const left = sessions.find((session) => session.id === a.value);
        const right = sessions.find((session) => session.id === b.value);
        return (right?.updated_at ?? "").localeCompare(left?.updated_at ?? "");
      });
    }
    return [...byWorkspace.values()].sort((a, b) => (activity.get(String(b.value)) ?? "").localeCompare(activity.get(String(a.value)) ?? ""));
  }, [sessions]);

  const selectedPath = currentSession ? [workspaceLabel(currentSession.workspace), currentSession.id] : undefined;
  const cascaderValue = selectorOpen && selectorPath.length ? selectorPath : selectedPath;

  const submit = async (content: string) => {
    if (!currentSession) return;
    try { await send(currentSession.id, content); setError(""); }
    catch (value) { setError(value instanceof Error ? value.message : "Unable to send message"); }
  };
  const resumeCurrent = async () => {
    if (!currentSession || resuming) return;
    setResuming(true);
    try { await resume(currentSession.id); }
    finally { setResuming(false); }
  };
  const stopCurrent = async () => {
    if (!currentSession || stopping) return;
    setStopping(true);
    try { await stop(currentSession.id); }
    finally { setStopping(false); }
  };
  if (loading) return <div className="loading-screen">Loading Agora…</div>;

  const sessionSelector = <Cascader
    className="session-selector"
    options={sessionOptions}
    value={cascaderValue}
    open={selectorOpen}
    onOpenChange={(open) => {
      setSelectorOpen(open);
      if (open) setSelectorPath(selectedPath ?? []);
      else setSelectorPath([]);
    }}
    onChange={(value) => {
      const path = value.map(String);
      const sessionId = path[1];
      if (sessionId) {
        setSelectedSessionId(sessionId);
        setSelectorPath([]);
        window.setTimeout(() => setSelectorOpen(false), 0);
      } else {
        setSelectorPath(path);
      }
    }}
    placeholder="Select a session"
    displayRender={(labels) => labels.length > 1 ? <span className="session-selector-value"><span className="session-selector-workspace">{labels[0]}</span><span className="session-selector-sep"> / </span>{labels[1]}</span> : labels[0]}
    showSearch
    allowClear={false}
    changeOnSelect
  />;

  return <Layout className="app-layout">
    <Header className="app-header">
      {sessions.length > 0 && !createOpen && <Space.Compact className="session-control header-session-control">
        {sessionSelector}
        {currentSession?.capabilities.can_resume && !currentSession.capabilities.can_stream && <Tooltip title="Resume this Claude session"><Button aria-label="Resume session" icon={<PlayCircleOutlined />} loading={resuming} onClick={() => void resumeCurrent()} /></Tooltip>}
        {currentSession?.capabilities.can_interrupt && <Tooltip title="Stop this Claude session"><Button danger aria-label="Stop session" icon={<PoweroffOutlined />} loading={stopping} onClick={() => void stopCurrent()} /></Tooltip>}
      </Space.Compact>}
      <Space className="header-actions"><Checkbox checked={showProcessDetails} onChange={(event) => setShowProcessDetails(event.target.checked)}>Process details</Checkbox><Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>New session</Button></Space>
    </Header>
    <Content className="app-content">
      {error && <Alert className="error-banner" type="error" closable message={error} onClose={() => setError("")} />}
      {sessions.length === 0 || createOpen ? <div className="setup-wrap"><SessionCreate onCreate={addSession} onCreated={() => { setCreateOpen(false); void refresh(); }} disabled={!coordination} /></div> : <div className="workspace-grid">
        <main className="session-main">
          <EventStream events={events} showProcessDetails={showProcessDetails} />
          {(currentSession?.capabilities.can_read_terminal || currentSession?.capabilities.can_send_input) && <div className="live-session-controls">
            {currentSession?.capabilities.can_read_terminal && <TerminalSnapshot snapshot={terminal.snapshot} error={terminal.error} loading={terminal.loading} />}
            {currentSession?.capabilities.can_send_input && <MessageComposer disabled={false} onSend={submit} />}
          </div>}
        </main>
      </div>}
    </Content>
  </Layout>;
}
