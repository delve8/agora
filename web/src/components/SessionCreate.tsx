import { useState } from "react";
import { Button, Card, Form, Input, Select, Space, Typography } from "antd";
import { FolderOpenOutlined, RocketOutlined } from "@ant-design/icons";
import { AgentBadge } from "./AgentBadge";
import type { CreateSessionInput } from "../api/client";
import type { Device } from "../types";

const { Text, Title } = Typography;

type SessionCreateProps = { onCreate: (input: CreateSessionInput) => Promise<unknown>; onCreated: () => void; disabled: boolean; devices: Device[] };

export function SessionCreate({ onCreate, onCreated, disabled, devices }: SessionCreateProps) {
  const [busy, setBusy] = useState(false);
  const [form] = Form.useForm<CreateSessionInput>();
  const submit = async (values: CreateSessionInput) => {
    setBusy(true);
    try { await onCreate(values); form.resetFields(["workspace"]); onCreated(); }
    catch { /* the parent displays the create error */ }
    finally { setBusy(false); }
  };
  // Offline devices stay listed but disabled, so the user sees which machines
  // cannot currently host a new session. The offline label is appended via
  // optionRender.
  const activeDevices = devices.filter((device) => !device.revoked_at);
  const deviceOptions = activeDevices
    .map((device) => ({ value: device.device_id, label: device.name || device.device_id, disabled: !device.connected }));
  return <Card className="setup-card">
    <Space direction="vertical" size={4}>
      <Text type="secondary">NEW SESSION</Text>
      <Title level={2}>Start an Agent session</Title>
      <Text type="secondary">Choose an Agent and workspace. Agora keeps the native Agent experience while giving you a responsive web view for history, activity and explicit input.</Text>
    </Space>
    <Form form={form} layout="vertical" onFinish={submit} initialValues={{ agent: "claude-code", display_name: "New session", role: "agent" }} className="setup-form">
      <Form.Item name="agent" label="Agent" rules={[{ required: true, message: "Select an agent" }]}>
        <Select options={["claude-code", "pi"].map((value) => ({ value, label: <AgentBadge agent={value} /> }))} />
      </Form.Item>
      {activeDevices.length > 0 && <Form.Item name="daemon_id" label="运行设备" tooltip="先选择这台会话运行在哪台 daemon 上；留空则由服务端自动挑选一台在线的。">
        <Select
          allowClear
          placeholder="自动（任意在线设备）"
          options={deviceOptions}
          optionRender={(option) => {
            const device = devices.find((item) => item.device_id === option.value);
            return <span>{option.label}{device && !device.connected ? "（离线）" : ""}</span>;
          }}
        />
      </Form.Item>}
      <Form.Item name="workspace" label="Workspace directory" rules={[{ required: true, message: "Enter an existing workspace path" }]}>
        <Input prefix={<FolderOpenOutlined />} placeholder="/path/to/workspace" />
      </Form.Item>
      <Form.Item name="display_name" label="Display name"><Input /></Form.Item>
      <Form.Item name="role" label="Role"><Input placeholder="agent, reviewer, implementer…" /></Form.Item>
      <Button type="primary" htmlType="submit" icon={<RocketOutlined />} loading={busy} disabled={disabled} block>Start session</Button>
    </Form>
  </Card>;
}
