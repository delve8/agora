import type { Event, Session } from "./types";

const GENERATED_NAMES = new Set(["", "New session", "Claude Code", "Claude Code Proxy"]);
const MAX_SESSION_NAME_LENGTH = 96;

export function isGeneratedSessionName(value: string) {
  return GENERATED_NAMES.has(value.trim());
}

export function describeSessionMessage(content: string) {
  const compact = content.replace(/\s+/g, " ").trim();
  if (!compact) return "";
  return compact.length > MAX_SESSION_NAME_LENGTH ? `${compact.slice(0, MAX_SESSION_NAME_LENGTH)}…` : compact;
}

export function derivedSessionName(session: Session, values: Event[]) {
  if (session.display_name_source === "custom") return null;
  const latestTitle = [...values].reverse().find((item) => item.kind === "ai-title" && item.content.trim());
  const firstUser = values.find((item) => item.kind === "user" && item.content.trim());
  const source = latestTitle ? "ai_title" : firstUser ? "first_user" : null;
  const name = describeSessionMessage(latestTitle?.content ?? firstUser?.content ?? "");
  if (!source || !name) return null;
  if (session.display_name_source === "ai_title" && source === "first_user") return null;
  if (!session.display_name_source && !isGeneratedSessionName(session.display_name)) return null;
  return { name, source } as const;
}

export function sessionLabel(displayName: string, id: string) {
  return isGeneratedSessionName(displayName) ? "New session" : displayName || id;
}
