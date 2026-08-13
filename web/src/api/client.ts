import type { Coordination, Event, Message, PTYSnapshot, Session } from "../types";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) }, ...init });
  const body = (await response.json()) as T & { error?: string };
  if (!response.ok) throw new Error(body.error || `request failed: ${response.status}`);
  return body as T;
}

export type StateResponse = { coordination: Coordination; sessions: Session[] };

export function loadState() { return request<StateResponse>("/api/state"); }
export function createSession(coordinationId: string, input: { workspace: string; display_name: string; role: string }) { return request<Session>(`/api/coordinations/${coordinationId}/sessions`, { method: "POST", body: JSON.stringify(input) }); }
export function loadEvents(sessionId: string) { return request<Event[]>(`/api/sessions/${sessionId}/events`); }
export function loadPTYSnapshot(sessionId: string, signal?: AbortSignal) { return request<PTYSnapshot>(`/api/sessions/${sessionId}/pty/snapshot`, { signal }); }
export function sendMessage(sessionId: string, content: string) { return request<Message>(`/api/sessions/${sessionId}/messages`, { method: "POST", body: JSON.stringify({ content }) }); }
export function subscribe(sessionId: string, onEvent: (event: Event) => void, onError: () => void) {
  const source = new EventSource(`/api/sessions/${encodeURIComponent(sessionId)}/events/stream`);
  source.onmessage = (message) => { try { onEvent(JSON.parse(message.data) as Event); } catch { onError(); } };
  source.onerror = onError;
  return () => source.close();
}
