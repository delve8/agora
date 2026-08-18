import { useState } from "react";
import { Alert, Badge, Button, Card, Collapse, Space, Tag, Typography } from "antd";
import { BulbOutlined, CodeOutlined, ExclamationCircleOutlined, GlobalOutlined, LinkOutlined, ThunderboltOutlined, UserOutlined } from "@ant-design/icons";
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

function ClaudeIcon() {
  return <svg viewBox="0 0 24 24" width="1em" height="1em" aria-hidden="true" focusable="false" fill="currentColor">
    <path d="M13.04 2.25h-2.08l-.35 6.18-2.99-5.42-1.8 1.04 2.79 5.07-5.18-3.09-1.04 1.8 5.36 3.2H1.5v2.08h6.23l-5.34 3.08 1.04 1.8 5.18-2.99-2.79 5.07 1.8 1.04 2.99-5.42.35 6.06h2.08l.35-6.06 2.99 5.42 1.8-1.04-2.79-5.07 5.18 2.99 1.04-1.8-5.34-3.08h6.23v-2.08h-6.25l5.36-3.2-1.04-1.8-5.18 3.09 2.79-5.07-1.8-1.04-2.99 5.42-.35-6.18Z" />
  </svg>;
}

function formatEventDate(value: string) {
  return new Date(value).toLocaleString(undefined, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function EventMeta({ event, user }: { event: Event; user: boolean }) {
  const label = user ? "You" : "Claude";
  return <Space className="event-meta" size={8} wrap>
    <Tag color={user ? "blue" : "volcano"} title={label} aria-label={label} icon={user ? <UserOutlined /> : <ClaudeIcon />} />
    {event.subtype && <Text type="secondary">{event.subtype}</Text>}
    <Text type="secondary">{formatEventDate(event.created_at)}</Text>
  </Space>;
}

function TextEvent({ event }: EventRendererProps) {
  const [expanded, setExpanded] = useState(false);
  const content = event.content || event.summary || "";
  const long = content.length > 900;
  const body = long && !expanded ? `${content.slice(0, 900)}…` : content;
  return <Card className={`event-card event-${event.role || event.kind}`} size="small" bordered={false}>
    <EventMeta event={event} user={event.role === "user" || event.kind === "user"} />
    <div className="event-markdown"><ReactMarkdown>{body}</ReactMarkdown></div>
    {long && <Button type="link" size="small" onClick={() => setExpanded((value) => !value)}>{expanded ? "Show less" : "Show full message"}</Button>}
  </Card>;
}

function structuredEventIcon(name: string, thinking = false) {
  if (thinking) return <BulbOutlined />;
  switch (name.toLowerCase()) {
    case "skill": return <ThunderboltOutlined />;
    case "websearch": return <GlobalOutlined />;
    case "webfetch": return <LinkOutlined />;
    default: return <CodeOutlined />;
  }
}

function structuredSummary(event: Event, value: string) {
  if (event.tool_name === "Skill") {
    try {
      const input = JSON.parse(event.tool_input || "{}");
      return input.skill || "Load a skill";
    } catch { return "Load a skill"; }
  }
  if (event.tool_name === "WebSearch") {
    try {
      const input = JSON.parse(event.tool_input || "{}");
      return input.query || shortText(value, 100);
    } catch { return shortText(value, 100); }
  }
  if (event.tool_name === "WebFetch") {
    try {
      const input = JSON.parse(event.tool_input || "{}");
      return input.url || shortText(value, 100);
    } catch { return shortText(value, 100); }
  }
  return shortText(event.summary || value, 100);
}

function ThinkingEvent({ event }: EventRendererProps) {
  const content = event.thinking || event.content || event.summary || "Thinking activity";
  return <Collapse className="event-collapse event-thinking" ghost items={[{
    key: event.id,
    label: <div className="structured-event-label"><Space size={8}>{structuredEventIcon("", true)}<Text strong>Thinking</Text><Text type="secondary" ellipsis>{shortText(content, 100)}</Text></Space><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>,
    children: <Paragraph type="secondary" className="preserved-text">{content}</Paragraph>,
  }]} />;
}

function ToolEvent({ event, result }: EventRendererProps & { result?: boolean }) {
  const details = result ? event.tool_output || event.content || "" : event.tool_input || event.content || "";
  const name = event.tool_name || event.subtype || "Tool";
  const label = result ? `${name} result` : name;
  return <Collapse className={`event-collapse event-tool ${result ? "event-tool-result" : ""}`} ghost items={[{
    key: event.id,
    label: <div className="structured-event-label"><Space size={8}>{structuredEventIcon(name)}<Text strong>{label}</Text><Text type="secondary" ellipsis>{structuredSummary(event, details)}</Text></Space><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>,
    children: details ? <pre>{details}</pre> : <Text type="secondary">No details</Text>,
  }]} />;
}

function ErrorEvent({ event }: EventRendererProps) {
  return <Alert className="event-alert" type="error" showIcon icon={<ExclamationCircleOutlined />} message={event.summary || "Session error"} description={<div className="preserved-text">{event.content}</div>} />;
}

function StatusEvent({ event }: EventRendererProps) {
  return <div className="status-event"><Badge status={event.kind === "error" ? "error" : "processing"} /><Text type="secondary">{event.summary || event.content || event.subtype || event.kind}</Text><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>;
}

export function EventRenderer({ event }: EventRendererProps) {
  const type = eventType(event);
  if (NOISE_KINDS.has(event.kind) || type === "metadata") return null;
  if (type === "thinking" || event.kind === "thinking") return <ThinkingEvent event={event} />;
  if (type === "tool_result" || event.kind === "result") return <ToolEvent event={event} result />;
  if (type === "tool_call" || event.kind === "tool") return <ToolEvent event={event} />;
  if (type === "error" || event.kind === "error") return <ErrorEvent event={event} />;
  if (type === "status" || event.kind === "system") return <StatusEvent event={event} />;
  return <TextEvent event={event} />;
}

export function isVisibleEvent(event: Event) {
  return !NOISE_KINDS.has(event.kind) && event.type !== "metadata";
}

function isProcessEvent(event: Event) {
  const type = eventType(event);
  return type === "thinking" || type === "tool_call" || type === "tool_result" || type === "status" || event.kind === "thinking" || event.kind === "tool" || event.kind === "result" || event.kind === "system";
}

export function EventStream({ events, showProcessDetails }: { events: Event[]; showProcessDetails: boolean }) {
  const visible = events.filter(isVisibleEvent);
  const processCount = visible.filter(isProcessEvent).length;
  const displayed = showProcessDetails ? visible : visible.filter((event) => !isProcessEvent(event));
  return <Card className="event-stream">
    <div className="events" aria-live="polite">
      {displayed.length === 0 ? <div className="empty-state"><Text type="secondary">{processCount > 0 ? "Process details are hidden. Use Show process details to inspect them." : "Start a session and send a message; activity will appear here."}</Text></div> : displayed.map((event) => <div className="event-row" key={event.id}><EventRenderer event={event} /></div>)}
    </div>
  </Card>;
}
