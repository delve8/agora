import { useState } from "react";
import { Button, Card, Form, Input, Space, Typography } from "antd";
import { FolderOpenOutlined, RocketOutlined } from "@ant-design/icons";

const { Text, Title } = Typography;

type SessionCreateProps = { onCreate: (input: { workspace: string; display_name: string; role: string }) => Promise<unknown>; onCreated: () => void; disabled: boolean };

export function SessionCreate({ onCreate, onCreated, disabled }: SessionCreateProps) {
  const [busy, setBusy] = useState(false);
  const [form] = Form.useForm<{ workspace: string; display_name: string; role: string }>();
  const submit = async (values: { workspace: string; display_name: string; role: string }) => {
    setBusy(true);
    try { await onCreate(values); form.resetFields(["workspace"]); onCreated(); }
    catch { /* the parent displays the create error */ }
    finally { setBusy(false); }
  };
  return <Card className="setup-card">
    <Space direction="vertical" size={4}>
      <Text type="secondary">NEW SESSION</Text>
      <Title level={2}>Start a Claude Code session</Title>
      <Text type="secondary">Agora keeps the native Claude Code terminal experience while giving you a responsive web view for history, activity and explicit input.</Text>
    </Space>
    <Form form={form} layout="vertical" onFinish={submit} initialValues={{ display_name: "Claude Code", role: "agent" }} className="setup-form">
      <Form.Item name="workspace" label="Workspace directory" rules={[{ required: true, message: "Enter an existing workspace path" }]}>
        <Input prefix={<FolderOpenOutlined />} placeholder="/path/to/workspace" />
      </Form.Item>
      <Form.Item name="display_name" label="Display name"><Input /></Form.Item>
      <Form.Item name="role" label="Role"><Input placeholder="agent, reviewer, implementer…" /></Form.Item>
      <Button type="primary" htmlType="submit" icon={<RocketOutlined />} loading={busy} disabled={disabled} block>Start session</Button>
    </Form>
  </Card>;
}
