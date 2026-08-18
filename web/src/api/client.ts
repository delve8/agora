import type { Coordination, Event, Message, PTYSnapshot, Session } from "../types";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) }, ...init });
  const text = await response.text();
  let body: T & { error?: string };
  try { body = JSON.parse(text) as T & { error?: string }; }
  catch { throw new Error(response.ok ? "Server returned invalid JSON" : `request failed: ${response.status}`); }
  if (!response.ok) throw new Error(body.error || `request failed: ${response.status}`);
  return body as T;
}

export type StateResponse = { coordination: Coordination; sessions: Session[] };

export function loadState() { return request<StateResponse>("/api/state"); }
export function createSession(coordinationId: string, input: { workspace: string; display_name: string; role: string }) { return request<Session>(`/api/coordinations/${coordinationId}/sessions`, { method: "POST", body: JSON.stringify(input) }); }
export function loadEvents(sessionId: string) { return request<Event[]>(`/api/sessions/${encodeURIComponent(sessionId)}/events`); }
export function resumeSession(sessionId: string) { return request<Session>(`/api/sessions/${encodeURIComponent(sessionId)}/resume`, { method: "POST" }); }
export function stopSession(sessionId: string) { return request<{ accepted: boolean }>(`/api/sessions/${encodeURIComponent(sessionId)}/stop`, { method: "POST" }); }
export function loadPTYSnapshot(sessionId: string, signal?: AbortSignal) { return request<PTYSnapshot>(`/api/sessions/${encodeURIComponent(sessionId)}/pty/snapshot`, { signal }); }
export function sendMessage(sessionId: string, content: string) { return request<Message>(`/api/sessions/${encodeURIComponent(sessionId)}/messages`, { method: "POST", body: JSON.stringify({ content }) }); }
export function subscribe(sessionId: string, onEvent: (event: Event) => void, onError: () => void) {
  const source = new EventSource(`/api/sessions/${encodeURIComponent(sessionId)}/events/stream`);
  source.onmessage = (message) => { try { onEvent(JSON.parse(message.data) as Event); } catch { onError(); } };
  source.onerror = onError;
  return () => source.close();
}
