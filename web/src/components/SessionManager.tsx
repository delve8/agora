import { useState } from "react";
import { Alert, Button, Drawer, Empty, Popconfirm, Space, Table, Tooltip, Typography } from "antd";
import { DeleteOutlined, StarFilled, StarOutlined } from "@ant-design/icons";
import type { Session } from "../types";
import { AgentBadge } from "./AgentBadge";
import { sessionLabel } from "../sessionName";

const { Text } = Typography;

function sessionRunning(session: Session): boolean {
  return (session.process_id ?? 0) > 0 && ["running", "waiting", "starting"].includes(session.state);
}

function stateLabel(value: string): string {
  switch (value) {
    case "running": return "运行中";
    case "starting": return "启动中";
    case "waiting": return "等待中";
    case "failed": return "失败";
    case "stopped": return "已停止";
    case "stale": return "失联";
    default: return value || "未知";
  }
}

type SessionManagerProps = {
  sessions: Session[];
  onSelect: (sessionId: string) => void;
  onStar: (session: Session, starred: boolean) => Promise<unknown>;
  onRename: (session: Session, name: string) => Promise<unknown>;
  onDelete: (session: Session) => Promise<unknown>;
  onOpen?: () => void;
};

// SessionManager is the single place to manage the session list: pick one,
// star it, give it an alias, or permanently delete its provider files. Deletion
// is only offered for stopped sessions because a running Agent still holds the
// transcript open.
export function SessionManager({ sessions, onSelect, onStar, onRename, onDelete, onOpen }: SessionManagerProps) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const run = async (key: string, action: () => Promise<unknown>) => {
    setBusy(key);
    setError("");
    try {
      await action();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "操作失败");
    } finally {
      setBusy("");
    }
  };

  const columns = [
    {
      title: "",
      key: "star",
      width: 44,
      render: (_: unknown, session: Session) => (
        <Tooltip title={session.starred ? "取消星标" : "标记为关注"}>
          <Button
            type="text"
            aria-label={session.starred ? "取消星标" : "标记为关注"}
            icon={session.starred ? <StarFilled style={{ color: "#faad14" }} /> : <StarOutlined />}
            loading={busy === `star:${session.id}`}
            onClick={(event) => { event.stopPropagation(); void run(`star:${session.id}`, () => onStar(session, !session.starred)); }}
          />
        </Tooltip>
      ),
    },
    {
      title: "会话",
      dataIndex: "display_name",
      key: "display_name",
      render: (_: unknown, session: Session) => (
        <Typography.Text
          editable={{ onChange: (value) => void run(`name:${session.id}`, () => onRename(session, value)) }}
          title="点击修改别名"
          onClick={(event) => event.stopPropagation()}
        >
          {sessionLabel(session.display_name, session.id)}
        </Typography.Text>
      ),
    },
    {
      title: "工作区",
      dataIndex: "workspace",
      key: "workspace",
      render: (workspace: string) => <Tooltip title={workspace}><Text type="secondary">{workspace || "未知工作区"}</Text></Tooltip>,
    },
    {
      title: "Agent",
      dataIndex: "agent",
      key: "agent",
      width: 90,
      render: (agent: string) => <AgentBadge agent={agent} compact />,
    },
    {
      title: "状态",
      dataIndex: "state",
      key: "state",
      width: 90,
      render: (value: string) => <Text type="secondary">{stateLabel(value)}</Text>,
    },
    {
      title: "操作",
      key: "actions",
      width: 80,
      render: (_: unknown, session: Session) => {
        const running = sessionRunning(session);
        return (
          <Popconfirm
            title="删除该会话？"
            description="将永久删除该会话在设备上的历史文件，且无法恢复。不会删除工作区代码。"
            okText="删除"
            okButtonProps={{ danger: true }}
            disabled={running}
            onConfirm={() => void run(`delete:${session.id}`, () => onDelete(session))}
          >
            <Tooltip title={running ? "运行中的会话不能删除，请先停止" : "删除会话文件"}>
              <span onClick={(event) => event.stopPropagation()}>
                <Button size="small" danger disabled={running} loading={busy === `delete:${session.id}`} icon={<DeleteOutlined />}>删除</Button>
              </span>
            </Tooltip>
          </Popconfirm>
        );
      },
    },
  ];

  return <>
    <Button className="session-manager-trigger" icon={<StarOutlined />} htmlType="button" onClick={() => { onOpen?.(); setOpen(true); }}>会话</Button>
    <Drawer
      title="会话管理"
      className="session-manager-drawer"
      width={720}
      open={open}
      maskClosable={false}
      destroyOnHidden={false}
      onClose={() => { setOpen(false); setError(""); }}
    >
      {error && <Alert className="session-manager-error" type="error" showIcon closable message={error} onClose={() => setError("")} />}
      {sessions.length === 0 ? (
        <Empty className="session-manager-empty" description="还没有会话。" />
      ) : (
        <Table
          className="session-manager-table"
          rowKey="id"
          columns={columns}
          dataSource={sessions}
          pagination={false}
          size="small"
          onRow={(session) => ({ onClick: () => { onSelect(session.id); setOpen(false); }, style: { cursor: "pointer" } })}
        />
      )}
      <Space direction="vertical" size={6} style={{ width: "100%", marginTop: 12 }}>
        <Text type="secondary">星标会话会排在列表最前，用来表示你当前关注的会话。别名只影响 Agora 中显示的名字。</Text>
      </Space>
    </Drawer>
  </>;
}
