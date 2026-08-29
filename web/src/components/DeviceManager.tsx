import { useCallback, useRef, useState } from "react";
import { Alert, Badge, Button, Collapse, Divider, Drawer, Empty, Popconfirm, Space, Spin, Statistic, Table, Tooltip, Typography } from "antd";
import { CheckOutlined, CopyOutlined, DesktopOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import { createPairCode, renameDevice, revokeDevice } from "../api/client";
import type { Device } from "../types";

const { Paragraph, Text } = Typography;

function relativeTime(value: string | undefined): string {
  if (!value) return "—";
  const then = Date.parse(value);
  if (Number.isNaN(then)) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (seconds < 60) return "刚刚";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  return `${days} 天前`;
}

type DeviceManagerProps = {
  devices: Device[];
  refresh: () => Promise<void>;
  onRevoked?: () => Promise<void>;
  onOpen?: () => void;
};

export function DeviceManager({ devices, refresh, onRevoked, onOpen }: DeviceManagerProps) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // Pairing state, folded in from the former AddDevice component so the device
  // drawer is the single place to manage and add devices.
  const [code, setCode] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [copiedCommand, setCopiedCommand] = useState(false);
  const [pairingBusy, setPairingBusy] = useState(false);
  const [pairingError, setPairingError] = useState("");
  const [pairingOpen, setPairingOpen] = useState(false);
  const pairingRequest = useRef(0);
  const activeDevices = devices.filter((device) => !device.revoked_at);
  const revokedDevices = devices.filter((device) => device.revoked_at);

  const issue = useCallback(async () => {
    const requestId = pairingRequest.current + 1;
    pairingRequest.current = requestId;
    setPairingBusy(true);
    setPairingError("");
    setCopied(false);
    setCopiedCommand(false);
    try {
      const value = await createPairCode();
      if (pairingRequest.current !== requestId) return;
      setCode(value.code);
      setExpiresAt(value.expires_at);
    } catch (cause) {
      if (pairingRequest.current !== requestId) return;
      setCode(null);
      setExpiresAt(null);
      setPairingError(cause instanceof Error ? cause.message : "无法生成配对码");
    } finally {
      if (pairingRequest.current === requestId) setPairingBusy(false);
    }
  }, []);

  const copy = async () => {
    if (!code) return;
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
    } catch {
      /* clipboard unavailable; the code stays visible for manual copy */
    }
  };

  const command = code ? `AGORA_SERVER_URL=${window.location.origin} agora daemon --pair ${code}` : "";

  const copyCommand = async () => {
    if (!command) return;
    try {
      await navigator.clipboard.writeText(command);
      setCopiedCommand(true);
    } catch {
      /* clipboard unavailable; the command stays visible for manual copy */
    }
  };

  const close = () => {
    pairingRequest.current += 1;
    setOpen(false);
    setPairingOpen(false);
    setPairingBusy(false);
    setCode(null);
    setExpiresAt(null);
    setCopied(false);
    setCopiedCommand(false);
    setPairingError("");
  };

  const expirePairingCode = () => {
    pairingRequest.current += 1;
    setPairingBusy(false);
    setCode(null);
    setExpiresAt(null);
    setCopied(false);
  };

  const openPairing = () => {
    setPairingOpen(true);
    setPairingError("");
    setCode(null);
    setExpiresAt(null);
    void issue();
  };

  const doRename = async (device: Device, name: string) => {
    const next = name.trim();
    if (!next || next === device.name) return;
    setBusy(true);
    setError("");
    try {
      await renameDevice(device.device_id, next);
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "重命名失败");
      await refresh();
    } finally {
      setBusy(false);
    }
  };

  const doRevoke = async (device: Device) => {
    setBusy(true);
    setError("");
    try {
      await revokeDevice(device.device_id);
      await refresh();
      await onRevoked?.();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "撤销设备失败");
    } finally {
      setBusy(false);
    }
  };

  const columns = [
    {
      title: "名称",
      dataIndex: "name",
      key: "name",
      render: (name: string, device: Device) => {
        if (device.revoked_at) {
          return <span><Text type="secondary">{name || device.device_id}</Text> <span className="device-revoked-label">已撤销</span></span>;
        }
        return <Typography.Text
          editable={{ onChange: (value) => void doRename(device, value) }}
          title="点击重命名"
        >{name || device.device_id}</Typography.Text>;
      },
    },
    {
      title: "设备 ID",
      dataIndex: "device_id",
      key: "device_id",
      render: (id: string) => <Tooltip title={id}><code className="device-id">{id.length > 24 ? id.slice(0, 24) + "…" : id}</code></Tooltip>,
    },
    {
      title: "状态",
      key: "status",
      width: 90,
      render: (_: unknown, device: Device) => {
        if (device.revoked_at) return <Text type="secondary">已撤销</Text>;
        return device.connected ? <Badge status="success" text="在线" /> : <Badge status="default" text="离线" />;
      },
    },
    {
      title: "最后活跃",
      dataIndex: "last_seen_at",
      key: "last_seen_at",
      width: 110,
      render: (value: string | undefined) => <Text type="secondary">{relativeTime(value)}</Text>,
    },
    {
      title: "操作",
      key: "actions",
      width: 90,
      render: (_: unknown, device: Device) => device.revoked_at ? null : (
        <Popconfirm
          title="撤销该设备？"
          description="撤销后该 daemon 无法再连接，需要重新配对。"
          okText="撤销"
          okButtonProps={{ danger: true }}
          onConfirm={() => void doRevoke(device)}
        >
          <Button size="small" danger loading={busy}>撤销</Button>
        </Popconfirm>
      ),
    },
  ];

  const auditColumns = [
    {
      title: "名称",
      dataIndex: "name",
      key: "name",
      render: (name: string, device: Device) => <Text type="secondary">{name || device.device_id}</Text>,
    },
    {
      title: "设备 ID",
      dataIndex: "device_id",
      key: "device_id",
      render: (id: string) => <Tooltip title={id}><code className="device-id">{id.length > 24 ? id.slice(0, 24) + "…" : id}</code></Tooltip>,
    },
    {
      title: "撤销时间",
      dataIndex: "revoked_at",
      key: "revoked_at",
      render: (value: string | undefined) => <Text type="secondary">{value ? new Date(value).toLocaleString() : "—"}</Text>,
    },
  ];

  const activeColumns = columns;

  return <>
    <Button className="device-manager-trigger" icon={<DesktopOutlined />} htmlType="button" onClick={() => { onOpen?.(); setOpen(true); }}>设备</Button>
    <Drawer
      title="设备 / Devices"
      className="device-drawer"
      width={640}
      open={open}
      maskClosable={false}
      destroyOnHidden={false}
      onClose={close}
    >
      {error && <Alert className="device-error" type="error" showIcon closable message={error} onClose={() => setError("")} />}
      {activeDevices.length === 0 ? (
        <Empty className="device-empty" description="还没有可用设备。点击“添加设备”配对一台 daemon。" />
      ) : (
        <Table
          className="device-table"
          rowKey="device_id"
          columns={activeColumns}
          dataSource={activeDevices}
          pagination={false}
          size="small"
        />
      )}
      {revokedDevices.length > 0 && <Collapse
        className="device-audit"
        ghost
        items={[{
          key: "revoked",
          label: `已撤销设备（${revokedDevices.length}）`,
          children: <Table
            className="device-table"
            rowKey="device_id"
            columns={auditColumns}
            dataSource={revokedDevices}
            pagination={false}
            size="small"
          />,
        }]}
      />}
      <Divider>添加设备</Divider>
      <Button type="primary" htmlType="button" icon={<PlusOutlined />} loading={pairingBusy} onClick={openPairing}>添加设备</Button>
      {pairingOpen && <div className="pairing-panel">
        <Divider>配对信息</Divider>
        <Space direction="vertical" size={12} style={{ width: "100%" }}>
          {pairingBusy && <div className="pairing-loading"><Spin size="small" /><Text type="secondary">正在生成配对码…</Text></div>}
          {pairingError && <Alert type="error" showIcon message="获取配对码失败" description={pairingError} action={<Button size="small" onClick={() => void issue()}>重试</Button>} />}
          {!pairingBusy && !pairingError && code && expiresAt && <>
            <Paragraph type="secondary">
              在目标机器上运行下面的命令，把这个 daemon 绑定到当前账号。配对码仅此一次有效，10 分钟内过期。
            </Paragraph>
            <div className="pair-code-row">
              <code className="pair-code">{code}</code>
              <Button size="small" icon={copied ? <CheckOutlined /> : <CopyOutlined />} onClick={() => void copy()}>{copied ? "已复制" : "复制"}</Button>
            </div>
            <div className="pair-expiry">
              <Statistic.Countdown title="有效期至" value={Date.parse(expiresAt)} onFinish={expirePairingCode} format="mm:ss" />
            </div>
            <div>
              <Text type="secondary">终端里运行：</Text>
              <div className="pair-command-row">
                <pre className="pair-command">{command}</pre>
                <Button
                  size="small"
                  icon={copiedCommand ? <CheckOutlined /> : <CopyOutlined />}
                  onClick={() => void copyCommand()}
                >
                  {copiedCommand ? "已复制" : "复制"}
                </Button>
              </div>
            </div>
            <Paragraph type="secondary" style={{ marginBottom: 0 }}>
              配对成功后 daemon 会立即连接，刷新会话列表即可看到它的会话。
            </Paragraph>
            <Button icon={<ReloadOutlined />} onClick={() => void issue()}>重新生成</Button>
          </>}
          {!pairingError && !code && expiresAt === null && <Text type="secondary">正在准备一次性配对码。</Text>}
        </Space>
      </div>}
    </Drawer>
  </>;
}

