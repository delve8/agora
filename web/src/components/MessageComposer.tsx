import { useState } from "react";
import { Button, Input, Space } from "antd";
import { SendOutlined } from "@ant-design/icons";

export function MessageComposer({ disabled, onSend }: { disabled: boolean; onSend: (content: string) => Promise<void> }) {
  const [content, setContent] = useState("");
  const [sending, setSending] = useState(false);
  const submit = async () => {
    const value = content.trim();
    if (!value || disabled || sending) return;
    setSending(true);
    try { await onSend(value); setContent(""); } finally { setSending(false); }
  };
  return <div className="composer">
    <Input.TextArea value={content} disabled={disabled || sending} onChange={(event) => setContent(event.target.value)} onPressEnter={(event) => { if ((event.metaKey || event.ctrlKey) && !event.shiftKey) { event.preventDefault(); void submit(); } }} placeholder={disabled ? "Create a Claude Code session first" : "Ask Claude Code something… (⌘/Ctrl + Enter)"} autoSize={{ minRows: 1, maxRows: 6 }} />
    <Space align="end"><Button type="primary" icon={<SendOutlined />} loading={sending} onClick={() => void submit()} disabled={disabled || !content.trim()}>Send</Button></Space>
  </div>;
}
