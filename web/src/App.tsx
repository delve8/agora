import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Alert, Avatar, Button, Cascader, Drawer, Dropdown, Layout, Select, Space, Switch, Tooltip } from "antd";
import { DesktopOutlined, EyeInvisibleOutlined, EyeOutlined, HistoryOutlined, LogoutOutlined, MenuOutlined, PlayCircleOutlined, PlusOutlined, PoweroffOutlined, RobotOutlined, StarFilled, StarOutlined, UserOutlined } from "@ant-design/icons";
import { EventStream } from "./components/EventStream";
import { MessageComposer } from "./components/MessageComposer";
import { SessionCreate } from "./components/SessionCreate";
import { SessionManager } from "./components/SessionManager";
import { TerminalSnapshot } from "./components/TerminalSnapshot";
import { DeviceManager } from "./components/DeviceManager";
import { useAuthActions } from "./AuthProvider";
import { useCoordination } from "./hooks/useCoordination";
import { useDevices } from "./hooks/useDevices";
import { usePTYSnapshot } from "./hooks/usePTYSnapshot";
import { sessionLabel } from "./sessionName";
import { AgentBadge } from "./components/AgentBadge";

const { Header, Content } = Layout;

type SessionOption = { value: string; label: ReactNode; children?: SessionOption[] };

// Starred sessions move into a dedicated group at the top of the selector so
// they lead the list regardless of which workspace they belong to.
const STARRED_GROUP = "★ 收藏";

function workspaceLabel(workspace: string) {
  return workspace || "未知工作区";
}

function sessionGroupLabel(session: { workspace: string; starred?: boolean }) {
  return session.starred ? STARRED_GROUP : workspaceLabel(session.workspace);
}

function sessionOptionLabel(session: { display_name: string; id: string; agent?: string; daemon_id?: string; starred?: boolean; capabilities: { can_resume: boolean; can_stream: boolean } }, deviceLabel: string) {
  const label = sessionLabel(session.display_name, session.id);
  const parts: ReactNode[] = [];
  if (session.starred) parts.push(<StarFilled key="star" className="session-star-icon" />);
  parts.push(<AgentBadge key="agent" agent={session.agent} compact />, <span key="label">{label}</span>);
  if (deviceLabel) parts.push(<span key="device" className="session-device-badge" title={session.daemon_id}>{deviceLabel}</span>);
  if (session.capabilities.can_resume && !session.capabilities.can_stream) {
    parts.push(<Tooltip key="inactive" title="会话未运行，可恢复"><HistoryOutlined className="session-inactive-icon" /></Tooltip>);
  }
  return <Space size={6}>{parts}</Space>;
}

export default function App() {
  const { coordination, sessions, currentSession, setSelectedSessionId, events, hasOlderEvents, loadingOlderEvents, loadOlderEvents, loading, error, setError, addSession, resume, stop, star, rename, remove, send, refresh } = useCoordination();
  const { devices, refresh: refreshDevices, deviceName } = useDevices();
  const auth = useAuthActions();
  const activeDevices = devices.filter((device) => !device.revoked_at);
  const canReadTerminal = currentSession?.capabilities.can_read_terminal ?? false;
  const [showTUI, setShowTUI] = useState(() => {
    try { return window.localStorage.getItem("agora.show-tui") === "true"; }
    catch { return false; }
  });
  const terminal = usePTYSnapshot(currentSession?.id ?? "", canReadTerminal && showTUI);
  const [createOpen, setCreateOpen] = useState(false);
  const [showProcessDetails, setShowProcessDetails] = useState(false);
  const [selectorOpen, setSelectorOpen] = useState(false);
  const [selectorPath, setSelectorPath] = useState<string[]>([]);
  const [resuming, setResuming] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [deviceFilter, setDeviceFilter] = useState<string | undefined>(undefined);
  const [agentFilter, setAgentFilter] = useState<string | undefined>(undefined);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);
  const [mobileWorkspace, setMobileWorkspace] = useState("");
  const [mobileSessionId, setMobileSessionId] = useState<string | undefined>(undefined);
  const closeMobileMenu = () => setMobileMenuOpen(false);
  const refreshAfterRevoke = useCallback(async () => {
    await refresh(true);
  }, [refresh]);

  useEffect(() => {
    try { window.localStorage.setItem("agora.show-tui", String(showTUI)); }
    catch { /* localStorage may be unavailable in private browsing */ }
  }, [showTUI]);

  useEffect(() => {
    if (!canReadTerminal && showTUI) setShowTUI(false);
  }, [canReadTerminal, showTUI]);

  useEffect(() => {
    if (deviceFilter && !devices.some((device) => device.device_id === deviceFilter && !device.revoked_at)) {
      setDeviceFilter(undefined);
    }
  }, [devices, deviceFilter]);

  const availableAgents = useMemo(() => [...new Set(sessions.map((session) => session.agent || "unknown"))].sort(), [sessions]);
  const filteredSessions = useMemo(() => sessions.filter((session) =>
    (!agentFilter || (session.agent || "unknown") === agentFilter) &&
    (!deviceFilter || session.daemon_id === deviceFilter),
  ), [sessions, agentFilter, deviceFilter]);
  useEffect(() => {
    if (agentFilter && !availableAgents.includes(agentFilter)) setAgentFilter(undefined);
  }, [agentFilter, availableAgents]);
  useEffect(() => {
    if (filteredSessions.length > 0 && currentSession && !filteredSessions.some((session) => session.id === currentSession.id)) {
      setSelectedSessionId(filteredSessions[0].id);
    }
  }, [currentSession, filteredSessions, setSelectedSessionId]);

  const sessionOptions = useMemo<SessionOption[]>(() => {
    const byWorkspace = new Map<string, SessionOption>();
    const activity = new Map<string, string>();
    const updatedAt = new Map<string, string>();
    const starred: SessionOption[] = [];
    for (const session of sessions) {
      if (deviceFilter && session.daemon_id !== deviceFilter) continue;
      if (agentFilter && (session.agent || "unknown") !== agentFilter) continue;
      updatedAt.set(session.id, session.updated_at);
      const option = { value: session.id, label: sessionOptionLabel(session, deviceName(session.daemon_id)) };
      if (session.starred) {
        starred.push(option);
        continue;
      }
      const workspace = workspaceLabel(session.workspace);
      let group = byWorkspace.get(workspace);
      if (!group) {
        group = { value: workspace, label: workspace, children: [] };
        byWorkspace.set(workspace, group);
      }
      group.children!.push(option);
      if (session.updated_at > (activity.get(workspace) ?? "")) activity.set(workspace, session.updated_at);
    }
    const byRecency = (a: SessionOption, b: SessionOption) => (updatedAt.get(String(b.value)) ?? "").localeCompare(updatedAt.get(String(a.value)) ?? "");
    starred.sort(byRecency);
    for (const group of byWorkspace.values()) group.children!.sort(byRecency);
    const groups = [...byWorkspace.values()].sort((a, b) => (activity.get(String(b.value)) ?? "").localeCompare(activity.get(String(a.value)) ?? ""));
    return starred.length > 0 ? [{ value: STARRED_GROUP, label: STARRED_GROUP, children: starred }, ...groups] : groups;
  }, [sessions, deviceFilter, agentFilter, deviceName]);

  const selectedPath = currentSession ? [sessionGroupLabel(currentSession), currentSession.id] : undefined;
  const mobileWorkspaceOptions = useMemo(() => sessionOptions.map((group) => ({ value: String(group.value), label: group.label })), [sessionOptions]);
  const mobileSessionOptions = useMemo(() => {
    const group = sessionOptions.find((item) => String(item.value) === mobileWorkspace);
    return group?.children ?? [];
  }, [mobileWorkspace, sessionOptions]);

  useEffect(() => {
    if (!mobileMenuOpen) return;
    setMobileWorkspace(currentSession ? sessionGroupLabel(currentSession) : String(mobileWorkspaceOptions[0]?.value ?? ""));
    setMobileSessionId(currentSession?.id);
  }, [currentSession?.id, currentSession?.workspace, currentSession?.starred, mobileMenuOpen, mobileWorkspaceOptions]);

  const submit = async (content: string) => {
    if (!currentSession) return;
    try { await send(currentSession.id, content); setError(""); }
    catch (value) { setError(value instanceof Error ? value.message : "消息发送失败"); }
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
  if (loading) return <div className="loading-screen">正在加载 Agora…</div>;

  const sessionSelector = (open: boolean, setOpen: (value: boolean) => void, path: string[], setPath: (value: string[]) => void) => <Cascader
    className="session-selector"
    options={sessionOptions}
    value={open && path.length ? path : selectedPath}
    open={open}
    onOpenChange={(nextOpen) => {
      setOpen(nextOpen);
      if (nextOpen) setPath(selectedPath ?? []);
      else setPath([]);
    }}
    onChange={(value) => {
      const path = value.map(String);
      const sessionId = path[1];
      if (sessionId) {
        setSelectedSessionId(sessionId);
        setPath([]);
        closeMobileMenu();
        window.setTimeout(() => setOpen(false), 0);
      } else {
        setPath(path);
      }
    }}
    placeholder="选择会话"
    displayRender={(labels) => labels.length > 1 ? <span className="session-selector-value"><span className="session-selector-workspace">{labels[0]}</span><span className="session-selector-sep"> / </span>{labels[1]}</span> : labels[0]}
    showSearch
    allowClear={false}
    changeOnSelect
  />;

  return <Layout className="app-layout">
    <Header className="app-header">
      <Space className="header-primary-controls">
        {availableAgents.length > 1 && <Select
          className="agent-filter"
          allowClear
          value={agentFilter}
          onChange={(value) => { setAgentFilter(value ?? undefined); closeMobileMenu(); }}
          placeholder={<Tooltip title="全部 Agent"><span className="agent-filter-placeholder" aria-label="全部 Agent"><RobotOutlined /></span></Tooltip>}
          options={availableAgents.map((agent) => ({ value: agent, label: <AgentBadge agent={agent} iconOnly /> }))}
        />}
        {activeDevices.length > 0 && <Select
          className="device-filter"
          allowClear
          placeholder="全部设备"
          value={deviceFilter}
          onChange={(value) => { setDeviceFilter(value ?? undefined); closeMobileMenu(); }}
          options={activeDevices.map((device) => ({ value: device.device_id, label: device.name || device.device_id }))}
        />}
        {sessions.length > 0 && !createOpen && <Space.Compact className="session-control header-session-control">
          {sessionSelector(selectorOpen, setSelectorOpen, selectorPath, setSelectorPath)}
          {currentSession?.capabilities.can_resume && !currentSession.capabilities.can_stream && <Tooltip title="恢复该 Agent 会话"><Button aria-label="Resume session" icon={<PlayCircleOutlined />} loading={resuming} onClick={() => void resumeCurrent()} /></Tooltip>}
          {currentSession?.capabilities.can_interrupt && <Tooltip title="停止该 Agent 会话"><Button danger aria-label="Stop session" icon={<PoweroffOutlined />} loading={stopping} onClick={() => void stopCurrent()} /></Tooltip>}
        </Space.Compact>}
      </Space>
      <Space className="header-actions">
        {canReadTerminal && <Tooltip title={showTUI ? "隐藏原生 TUI" : "显示原生 TUI（占满主区域）"}>
          <Switch
            className="tui-toggle"
            size="small"
            checked={showTUI}
            checkedChildren={<DesktopOutlined />}
            unCheckedChildren={<DesktopOutlined />}
            aria-label={showTUI ? "隐藏原生 TUI" : "显示原生 TUI"}
            onChange={setShowTUI}
          />
        </Tooltip>}
        <Tooltip title={showProcessDetails ? "隐藏过程详情" : "显示过程详情"}>
          <Switch
            className="process-details-toggle"
            size="small"
            checked={showProcessDetails}
            checkedChildren={<EyeOutlined />}
            unCheckedChildren={<EyeInvisibleOutlined />}
            aria-label={showProcessDetails ? "隐藏过程详情" : "显示过程详情"}
            onChange={setShowProcessDetails}
          />
        </Tooltip>
        <span className="desktop-session-manager"><SessionManager sessions={sessions} onSelect={setSelectedSessionId} onStar={(_session, value) => star(_session.id, value)} onRename={(session, name) => rename(session.id, name)} onDelete={(session) => remove(session.id)} /></span>
        <span className="desktop-device-manager"><DeviceManager devices={devices} refresh={refreshDevices} onRevoked={refreshAfterRevoke} /></span>
        <Button className="desktop-new-session" type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>新建会话</Button>
        {auth && <Dropdown
          trigger={["click"]}
          placement="bottomRight"
          menu={{
            items: [
              { key: "identity", label: <span className="header-user-identity">{auth.identity}</span>, disabled: true },
              { type: "divider" },
              { key: "signout", icon: <LogoutOutlined />, label: "退出登录", onClick: () => auth.signOut() },
            ],
          }}
        >
          <Button className="header-user" type="text" aria-label="账户菜单" icon={<Avatar size={22} icon={<UserOutlined />} />} />
        </Dropdown>}
      </Space>
      <Button
        className="mobile-menu-button"
        icon={<MenuOutlined />}
        aria-label="打开会话菜单"
        onClick={() => setMobileMenuOpen(true)}
      >
        {currentSession && <AgentBadge className="mobile-menu-agent" agent={currentSession.agent} compact />}
        <span className="mobile-menu-label">{currentSession ? sessionLabel(currentSession.display_name, currentSession.id) : "会话菜单"}</span>
      </Button>
      {canReadTerminal && <Tooltip title={showTUI ? "隐藏原生 TUI" : "显示原生 TUI（占满主区域）"}>
        <Switch
          className="mobile-tui-toggle"
          size="small"
          checked={showTUI}
          checkedChildren={<DesktopOutlined />}
          unCheckedChildren={<DesktopOutlined />}
          aria-label={showTUI ? "隐藏原生 TUI" : "显示原生 TUI"}
          onChange={setShowTUI}
        />
      </Tooltip>}
      <Tooltip title={showProcessDetails ? "隐藏过程详情" : "显示过程详情"}>
        <Switch
          className="mobile-process-details-toggle"
          size="small"
          checked={showProcessDetails}
          checkedChildren={<EyeOutlined />}
          unCheckedChildren={<EyeInvisibleOutlined />}
          aria-label={showProcessDetails ? "隐藏过程详情" : "显示过程详情"}
          onChange={setShowProcessDetails}
        />
      </Tooltip>
      <Drawer
        title="会话操作"
        placement="bottom"
        height="min(78vh, 620px)"
        open={mobileMenuOpen}
        onClose={closeMobileMenu}
        className="mobile-menu-drawer"
        extra={<Button type="text" onClick={closeMobileMenu}>完成</Button>}
      >
        <Space direction="vertical" size={14} className="mobile-menu-content">
          {availableAgents.length > 1 && <label className="mobile-menu-field"><span>Agent</span><Select
            className="mobile-menu-select"
            allowClear
            value={agentFilter}
            onChange={(value) => { setAgentFilter(value ?? undefined); closeMobileMenu(); }}
            placeholder="全部 Agent"
            options={availableAgents.map((agent) => ({ value: agent, label: <Space size={6}><AgentBadge agent={agent} /><span>{agent}</span></Space> }))}
          /></label>}
          {activeDevices.length > 0 && <label className="mobile-menu-field"><span>设备</span><Select
            className="mobile-menu-select"
            allowClear
            value={deviceFilter}
            onChange={(value) => { setDeviceFilter(value ?? undefined); closeMobileMenu(); }}
            placeholder="全部设备"
            options={activeDevices.map((device) => ({ value: device.device_id, label: device.name || device.device_id }))}
          /></label>}
          {sessions.length > 0 && !createOpen && <>
            <label className="mobile-menu-field"><span>工作区</span><Select
              className="mobile-menu-select"
              value={mobileWorkspace || undefined}
              options={mobileWorkspaceOptions}
              placeholder="选择工作区"
              onChange={(value) => { setMobileWorkspace(value); setMobileSessionId(undefined); }}
            /></label>
            <label className="mobile-menu-field"><span>会话</span><Space.Compact className="session-control mobile-session-control">
              <Select
                className="mobile-menu-select"
                value={mobileSessionId}
                options={mobileSessionOptions}
                placeholder={mobileWorkspace ? "选择会话" : "先选择工作区"}
                disabled={!mobileWorkspace}
                showSearch
                optionFilterProp="label"
                onChange={(value) => { setMobileSessionId(value); setSelectedSessionId(value); closeMobileMenu(); }}
              />
              {currentSession?.capabilities.can_resume && !currentSession.capabilities.can_stream && <Tooltip title="恢复该 Agent 会话"><Button aria-label="Resume session" icon={<PlayCircleOutlined />} loading={resuming} onClick={() => { closeMobileMenu(); void resumeCurrent(); }} /></Tooltip>}
              {currentSession?.capabilities.can_interrupt && <Tooltip title="停止该 Agent 会话"><Button danger aria-label="Stop session" icon={<PoweroffOutlined />} loading={stopping} onClick={() => { closeMobileMenu(); void stopCurrent(); }} /></Tooltip>}
            </Space.Compact></label>
          </>}
          {sessions.length > 0 && <SessionManager sessions={sessions} onSelect={setSelectedSessionId} onStar={(_session, value) => star(_session.id, value)} onRename={(session, name) => rename(session.id, name)} onDelete={(session) => remove(session.id)} onOpen={closeMobileMenu} />}
          <div className="mobile-menu-actions">
            <Button type="primary" block icon={<PlusOutlined />} onClick={() => { setCreateOpen(true); closeMobileMenu(); }}>新建会话</Button>
          </div>
          {auth && <div className="mobile-menu-auth">
            <span className="auth-identity"><UserOutlined /> {auth.identity}</span>
            <Button type="text" size="small" icon={<LogoutOutlined />} onClick={auth.signOut}>退出登录</Button>
          </div>}
          <DeviceManager devices={devices} refresh={refreshDevices} onRevoked={refreshAfterRevoke} onOpen={closeMobileMenu} />
        </Space>
      </Drawer>
    </Header>
    <Content className="app-content">
      {error && <Alert className="error-banner" type="error" closable message={error} onClose={() => setError("")} />}
      {sessions.length === 0 || createOpen ? <div className="setup-wrap"><SessionCreate onCreate={addSession} onCreated={() => { setCreateOpen(false); void refresh(); }} disabled={!coordination} devices={devices} /></div> : <div className="workspace-grid">
        <main className="session-main">
          <div className="session-heading">
            {currentSession && <Space size={8} wrap>
              <Tooltip title={currentSession.starred ? "取消星标" : "标记为关注"}>
                <Button
                  type="text"
                  size="small"
                  className="session-star-toggle"
                  aria-label={currentSession.starred ? "取消星标" : "标记为关注"}
                  icon={currentSession.starred ? <StarFilled style={{ color: "#faad14" }} /> : <StarOutlined />}
                  onClick={() => void star(currentSession.id, !currentSession.starred)}
                />
              </Tooltip>
              <AgentBadge agent={currentSession.agent} />
              <span className="session-heading-name">{sessionLabel(currentSession.display_name, currentSession.id)}</span>
            </Space>}
          </div>
          {showTUI && canReadTerminal ? <div className="tui-view">
            <TerminalSnapshot agent={currentSession?.agent} snapshot={terminal.snapshot} error={terminal.error} loading={terminal.loading} />
            {currentSession?.capabilities.can_send_input && <MessageComposer disabled={false} onSend={submit} />}
          </div> : <>
            <EventStream
              key={currentSession?.id}
              events={events}
              showProcessDetails={showProcessDetails}
              agent={currentSession?.agent}
              hasOlderEvents={hasOlderEvents}
              loadingOlder={loadingOlderEvents}
              onLoadOlder={() => void loadOlderEvents()}
              // A running session can exist before its provider transcript does
              // (a fresh session, or right after a context switch). Say so
              // instead of rendering an empty conversation.
              awaitingHistory={Boolean(currentSession && !currentSession.capabilities.can_read_history && (currentSession.capabilities.can_send_input || currentSession.capabilities.can_stream))}
            />
            {currentSession?.capabilities.can_send_input && <div className="live-session-controls"><MessageComposer disabled={false} onSend={submit} /></div>}
          </>}
        </main>
      </div>}
    </Content>
  </Layout>;
}
