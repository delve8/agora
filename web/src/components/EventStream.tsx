import { useState } from "react";
import { Alert, Badge, Button, Card, Collapse, Space, Tag, Typography } from "antd";
import { CodeOutlined, ExclamationCircleOutlined, SettingOutlined } from "@ant-design/icons";
import ReactMarkdown from "react-markdown";
import type { Event } from "../types";

const { Paragraph, Text } = Typography;

const NOISE_KINDS = new Set(["queue-operation", "attachment", "file-history-snapshot", "ai-title", "last-prompt"]);

type EventRendererProps = { event: Event };

function eventType(event: Event) {
  return event.type || (event.kind === "tool" ? "tool_call" : event.kind === "result" ? "tool_result" : event.kind);
}

function shortText(value: string, length = 180) {
  const compact = value.replace(/\s+/g, " ").trim();
  return compact.length > length ? `${compact.slice(0, length)}…` : compact;
}

function EventMeta({ event, label, color }: { event: Event; label: string; color?: string }) {
  return <Space className="event-meta" size={8} wrap>
    <Tag color={color}>{label}</Tag>
    {event.subtype && <Text type="secondary">{event.subtype}</Text>}
    <Text type="secondary">{event.source || "agora"}</Text>
    <Text type="secondary">{new Date(event.created_at).toLocaleTimeString()}</Text>
  </Space>;
}

function TextEvent({ event }: EventRendererProps) {
  const [expanded, setExpanded] = useState(false);
  const content = event.content || event.summary || "";
  const long = content.length > 900;
  const body = long && !expanded ? `${content.slice(0, 900)}…` : content;
  return <Card className={`event-card event-${event.role || event.kind}`} size="small" bordered={false}>
    <EventMeta event={event} label={event.role === "user" || event.kind === "user" ? "You" : "Claude"} color={event.role === "user" || event.kind === "user" ? "blue" : "geekblue"} />
    <div className="event-markdown"><ReactMarkdown>{body}</ReactMarkdown></div>
    {long && <Button type="link" size="small" onClick={() => setExpanded((value) => !value)}>{expanded ? "Show less" : "Show full message"}</Button>}
  </Card>;
}

function ThinkingEvent({ event }: EventRendererProps) {
  const content = event.thinking || event.content || event.summary || "Thinking activity";
  return <Collapse className="event-collapse event-thinking" ghost items={[{
    key: event.id,
    label: <Space><SettingOutlined /><span>Thinking progress</span><Text type="secondary">{shortText(content, 80)}</Text></Space>,
    children: <Paragraph type="secondary" className="preserved-text">{content}</Paragraph>,
  }]} />;
}

function ToolEvent({ event, result }: EventRendererProps & { result?: boolean }) {
  const input = event.tool_input || event.tool_output || event.content || "";
  const label = result ? "Tool result" : "Tool call";
  const color = result ? (event.is_error ? "error" : "success") : "purple";
  return <Collapse className={`event-collapse event-tool ${result ? "event-tool-result" : ""}`} ghost items={[{
    key: event.id,
    label: <Space><CodeOutlined /><Tag color={color}>{label}</Tag><Text strong>{event.tool_name || event.subtype || "Claude tool"}</Text><Text type="secondary">{shortText(event.summary || input)}</Text></Space>,
    children: <div className="tool-details">
      {event.summary && <Paragraph>{event.summary}</Paragraph>}
      {input && <pre>{input}</pre>}
    </div>,
  }]} />;
}

function ErrorEvent({ event }: EventRendererProps) {
  return <Alert className="event-alert" type="error" showIcon icon={<ExclamationCircleOutlined />} message={event.summary || "Session error"} description={<div className="preserved-text">{event.content}</div>} />;
}

function StatusEvent({ event }: EventRendererProps) {
  return <div className="status-event"><Badge status={event.kind === "error" ? "error" : "processing"} /><Text type="secondary">{event.summary || event.content || event.subtype || event.kind}</Text><Text type="secondary">{new Date(event.created_at).toLocaleTimeString()}</Text></div>;
}

export function EventRenderer({ event }: EventRendererProps) {
  const type = eventType(event);
  if (NOISE_KINDS.has(event.kind) || type === "metadata") return null;
  if (type === "thinking" || event.kind === "thinking") return <ThinkingEvent event={event} />;
  if (type === "tool_call" || event.kind === "tool") return <ToolEvent event={event} />;
  if (type === "tool_result" || event.kind === "result") return <ToolEvent event={event} result />;
  if (type === "error" || event.kind === "error") return <ErrorEvent event={event} />;
  if (type === "status" || event.kind === "system") return <StatusEvent event={event} />;
  return <TextEvent event={event} />;
}

export function isVisibleEvent(event: Event) {
  return !NOISE_KINDS.has(event.kind) && event.type !== "metadata";
}

export function EventStream({ events }: { events: Event[] }) {
  const visible = events.filter(isVisibleEvent);
  return <Card className="event-stream" title={<Space><span>Activity</span><Badge count={visible.length} showZero color="#1677ff" /></Space>} extra={<Text type="secondary">Live</Text>}>
    <div className="events" aria-live="polite">
      {visible.length === 0 ? <div className="empty-state"><Text type="secondary">Start a session and send a message; activity will appear here.</Text></div> : visible.map((event) => <div className="event-row" key={event.id}><EventRenderer event={event} /></div>)}
    </div>
  </Card>;
}
