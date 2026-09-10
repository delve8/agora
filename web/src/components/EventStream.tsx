import { useLayoutEffect, useRef, useState } from "react";
import { Alert, Badge, Button, Card, Collapse, Space, Tag, Tooltip, Typography } from "antd";
import { BulbOutlined, CheckCircleOutlined, CodeOutlined, ExclamationCircleOutlined, GlobalOutlined, LinkOutlined, ThunderboltOutlined, UserOutlined } from "@ant-design/icons";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Event } from "../types";
import { AgentBadge } from "./AgentBadge";

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

function cleanThinking(value: string) {
  // Some adapters expose the protocol marker as text. It is presentation
  // metadata, not part of the assistant's message.
  return value.replace(/<\/?thinking>/gi, "").trim();
}

/* function LegacyClaudeIcon() {
  return <svg viewBox="0 0 24 24" width="1em" height="1em" aria-hidden="true" focusable="false" fill="currentColor">
    <path d="M13.04 2.25h-2.08l-.35 6.18-2.99-5.42-1.8 1.04 2.79 5.07-5.18-3.09-1.04 1.8 5.36 3.2H1.5v2.08h6.23l-5.34 3.08 1.04 1.8 5.18-2.99-2.79 5.07 1.8 1.04 2.99-5.42.35 6.06h2.08l.35-6.06 2.99 5.42 1.8-1.04-2.79-5.07 5.18 2.99 1.04-1.8-5.34-3.08h6.23v-2.08h-6.25l5.36-3.2-1.04-1.8-5.18 3.09 2.79-5.07-1.8-1.04-2.99 5.42-.35-6.18Z" />
  </svg>;
}
*/

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

function EventMeta({ event, user, agent }: { event: Event; user: boolean; agent?: string }) {
  const label = user ? "You" : "Agent";
  return <Space className="event-meta" size={8} wrap>
    {user ? <Tag color="blue" title={label} aria-label={label} icon={<UserOutlined />} /> : <AgentBadge agent={agent} compact />}
    {event.subtype && <Text type="secondary">{event.subtype}</Text>}
    <Text type="secondary">{formatEventDate(event.created_at)}</Text>
  </Space>;
}

function TextEvent({ event, agent }: EventRendererProps & { agent?: string }) {
  const [expanded, setExpanded] = useState(false);
  const content = cleanThinking(event.content || event.summary || "");
  const long = content.length > 900;
  const body = long && !expanded ? `${content.slice(0, 900)}…` : content;
  return <Card className={`event-card event-${event.role || event.kind}`} size="small" bordered={false}>
    <EventMeta event={event} user={event.role === "user" || event.kind === "user"} agent={agent} />
    <div className="event-markdown"><ReactMarkdown remarkPlugins={[remarkGfm]}>{body}</ReactMarkdown></div>
    {long && <Button type="link" size="small" onClick={() => setExpanded((value) => !value)}>{expanded ? "Show less" : "Show full message"}</Button>}
  </Card>;
}

function structuredEventIcon(name: string, thinking = false, result = false) {
  if (thinking) return <BulbOutlined />;
  if (result) return <CheckCircleOutlined />;
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

function ThinkingEvent({ event, agent }: EventRendererProps & { agent?: string }) {
  const content = cleanThinking(event.thinking || event.content || event.summary || "Thinking activity");
  return <Collapse defaultActiveKey={[]} className="event-collapse event-thinking" ghost items={[{
    key: event.id,
    label: <div className="structured-event-label"><Space size={8}><AgentBadge agent={agent} compact /><Tooltip title="Thinking"><span className="structured-event-icon">{structuredEventIcon("", true)}</span></Tooltip><Text type="secondary" ellipsis>{shortText(content, 100)}</Text></Space><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>,
    children: <Paragraph type="secondary" className="preserved-text">{content}</Paragraph>,
  }]} />;
}

function ToolEvent({ event, result, agent }: EventRendererProps & { result?: boolean; agent?: string }) {
  const details = result ? event.tool_output || event.content || "" : event.tool_input || event.content || "";
  const name = event.tool_name || event.subtype || "Tool";
  const label = result ? `${name} result` : name;
  return <Collapse defaultActiveKey={[]} className={`event-collapse event-tool ${result ? "event-tool-result" : ""}`} ghost items={[{
    key: event.id,
    label: <div className="structured-event-label"><Space size={8}><AgentBadge agent={agent} compact /><Tooltip title={label}><span className="structured-event-icon">{structuredEventIcon(name, false, result)}</span></Tooltip><Text type="secondary" ellipsis>{structuredSummary(event, details)}</Text></Space><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>,
    children: details ? <pre>{details}</pre> : <Text type="secondary">No details</Text>,
  }]} />;
}

function ErrorEvent({ event }: EventRendererProps) {
  return <Alert className="event-alert" type="error" showIcon icon={<ExclamationCircleOutlined />} message={event.summary || "Session error"} description={<div className="preserved-text">{event.content}</div>} />;
}

function StatusEvent({ event }: EventRendererProps) {
  return <div className="status-event"><Badge status={event.kind === "error" ? "error" : "processing"} /><Text type="secondary">{event.summary || event.content || event.subtype || event.kind}</Text><Text type="secondary">{formatEventDate(event.created_at)}</Text></div>;
}

export function EventRenderer({ event, agent }: EventRendererProps & { agent?: string }) {
  const type = eventType(event);
  if (NOISE_KINDS.has(event.kind) || type === "metadata") return null;
  if (type === "thinking" || event.kind === "thinking") return <ThinkingEvent event={event} agent={agent} />;
  if (type === "tool_result" || event.kind === "result") return <ToolEvent event={event} result agent={agent} />;
  if (type === "tool_call" || type === "tool_update" || event.kind === "tool") return <ToolEvent event={event} agent={agent} />;
  if (type === "error" || event.kind === "error") return <ErrorEvent event={event} />;
  if (type === "status" || event.kind === "system") return <StatusEvent event={event} />;
  return <TextEvent event={event} agent={agent} />;
}

export function isVisibleEvent(event: Event) {
  return !NOISE_KINDS.has(event.kind) && event.type !== "metadata";
}

function isProcessEvent(event: Event) {
  const type = eventType(event);
  return type === "thinking" || type === "tool_call" || type === "tool_update" || type === "tool_result" || type === "status" || event.kind === "thinking" || event.kind === "tool" || event.kind === "result" || event.kind === "system";
}

export function EventStream({ events, showProcessDetails, agent, hasOlderEvents, loadingOlder, onLoadOlder, awaitingHistory }: { events: Event[]; showProcessDetails: boolean; agent?: string; hasOlderEvents?: boolean; loadingOlder?: boolean; onLoadOlder?: () => void; awaitingHistory?: boolean }) {
  const visible = events.filter(isVisibleEvent);
  const processCount = visible.filter(isProcessEvent).length;
  const displayed = showProcessDetails ? visible : visible.filter((event) => !isProcessEvent(event));
  const scrollRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);
  const initialized = useRef(false);
  const olderAnchor = useRef<{ height: number; top: number } | null>(null);

  useLayoutEffect(() => {
    const element = scrollRef.current;
    if (!element) return;
    const anchor = olderAnchor.current;
    if (anchor) {
      // Keep the anchor while the older page is in flight. The loading state
      // itself must not consume it; restore after the new rows are mounted.
      if (loadingOlder) return;
      // Prepending older rows must not move the message currently under the
      // user's finger/viewport.
      element.scrollTop = element.scrollHeight - anchor.height + anchor.top;
      olderAnchor.current = null;
      return;
    }
    // On the first render for a session, show the latest messages. Subsequent
    // messages follow the bottom only while the user is already there.
    if (!initialized.current || stickToBottom.current) element.scrollTop = element.scrollHeight;
    initialized.current = true;
  }, [events, showProcessDetails, displayed.length, loadingOlder]);

  // With process details hidden, the first page can contain too few visible
  // messages to create a scrollbar. Load another page until the viewport can
  // scroll, so pulling upward on mobile still reaches older messages. A
  // ResizeObserver is needed here because mobile layout/keyboard changes can
  // happen after the first layout effect has run.
  useLayoutEffect(() => {
    const element = scrollRef.current;
    if (!element) return;
    const loadIfViewportHasRoom = () => {
      if (loadingOlder || !hasOlderEvents || !onLoadOlder || olderAnchor.current) return;
      // A page may be full of hidden thinking/tool events. Keep a small
      // visible-message floor so the user still gets a useful, scrollable
      // transcript when process details are off.
      if (displayed.length < 8 || element.scrollHeight <= element.clientHeight + 1) {
        olderAnchor.current = { height: element.scrollHeight, top: element.scrollTop };
        onLoadOlder();
      }
    };
    const frame = window.requestAnimationFrame(loadIfViewportHasRoom);
    const observer = typeof ResizeObserver !== "undefined" ? new ResizeObserver(loadIfViewportHasRoom) : undefined;
    observer?.observe(element);
    return () => {
      window.cancelAnimationFrame(frame);
      observer?.disconnect();
    };
  }, [displayed.length, hasOlderEvents, loadingOlder, onLoadOlder]);

  const requestOlder = () => {
    const element = scrollRef.current;
    if (!element || !hasOlderEvents || loadingOlder || !onLoadOlder || olderAnchor.current) return;
    olderAnchor.current = { height: element.scrollHeight, top: element.scrollTop };
    onLoadOlder();
  };

  const handleScroll = () => {
    const element = scrollRef.current;
    if (!element) return;
    const distanceFromBottom = element.scrollHeight - element.scrollTop - element.clientHeight;
    stickToBottom.current = distanceFromBottom < 48;
    if (element.scrollTop <= 32) requestOlder();
  };

  return <Card className="event-stream">
    <div ref={scrollRef} className="events" aria-live="polite" onScroll={handleScroll}>
      {loadingOlder && <div className="history-load-more"><Text type="secondary">加载更早的消息…</Text></div>}
      {displayed.length === 0 ? <div className="empty-state"><Text type="secondary">{processCount > 0 ? "Process details are hidden. Use Show process details to inspect them." : awaitingHistory ? "该会话还没有历史记录——Agent 写入第一条消息后即可查看。" : "Start a session and send a message; activity will appear here."}</Text></div> : displayed.map((event) => <div className="event-row" key={event.id}><EventRenderer event={event} agent={agent} /></div>)}
    </div>
  </Card>;
}
