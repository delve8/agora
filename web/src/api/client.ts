import type { Coordination, Device, Event, Message, PTYSnapshot, Session } from "../types";

let accessToken: (() => Promise<string | undefined>) | undefined;
let invalidateAccessToken: (() => Promise<void>) | undefined;

export function setAccessTokenProvider(
  provider: (() => Promise<string | undefined>) | undefined,
  invalidate?: (() => Promise<void>) | undefined,
) {
  accessToken = provider;
  invalidateAccessToken = invalidate;
}

async function request<T>(path: string, init?: RequestInit, retry = false): Promise<T> {
  const token = accessToken ? await accessToken() : undefined;
  const headers = new Headers(init?.headers);
  headers.set("Content-Type", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const response = await fetch(path, { ...init, headers });
  const text = await response.text();
  let body: T & { error?: string };
  try { body = JSON.parse(text) as T & { error?: string }; }
  catch { throw new Error(response.ok ? "Server returned invalid JSON" : `request failed: ${response.status}`); }
  // A stale Logto access token can survive a refresh in the browser SDK. API
  // requests are safe to retry once because authentication is checked before
  // the handler is entered. The invalidation callback obtains a fresh token
  // for the configured Agora resource before the retry.
  if (response.status === 401 && !retry && invalidateAccessToken) {
    await invalidateAccessToken();
    return request<T>(path, init, true);
  }
  if (!response.ok) throw new Error(body.error || `request failed: ${response.status}`);
  return body as T;
}

export type StateResponse = { coordination: Coordination; sessions: Session[] };
export type PrincipalResponse = { principal: { user_id: string; display_name?: string; email?: string }; auth_mode: string };

export function loadMe() { return request<PrincipalResponse>("/api/me"); }
export function loadState() { return request<StateResponse>("/api/state"); }
export type PairCodeResponse = { code: string; expires_at: string };
export function createPairCode() { return request<PairCodeResponse>("/api/devices/pair-codes", { method: "POST" }); }
export function listDevices() { return request<Device[]>("/api/devices"); }
export function renameDevice(deviceId: string, name: string) { return request<{ renamed: boolean }>(`/api/devices/${encodeURIComponent(deviceId)}/name`, { method: "POST", body: JSON.stringify({ name }) }); }
export function revokeDevice(deviceId: string) { return request<{ revoked: boolean }>(`/api/devices/${encodeURIComponent(deviceId)}/revoke`, { method: "POST" }); }
export type CreateSessionInput = { workspace: string; display_name: string; role: string; agent: string; daemon_id?: string };
export function createSession(coordinationId: string, input: CreateSessionInput) { return request<Session>(`/api/coordinations/${coordinationId}/sessions`, { method: "POST", body: JSON.stringify(input) }); }
export function loadEvents(sessionId: string, options?: { limit?: number; before?: string }) {
  const query = new URLSearchParams();
  if (options?.limit) query.set("limit", String(options.limit));
  if (options?.before) query.set("before", options.before);
  const suffix = query.toString() ? `?${query.toString()}` : "";
  return request<Event[]>(`/api/sessions/${encodeURIComponent(sessionId)}/events${suffix}`);
}
export function resumeSession(sessionId: string) { return request<Session>(`/api/sessions/${encodeURIComponent(sessionId)}/resume`, { method: "POST" }); }
export function stopSession(sessionId: string) { return request<{ accepted: boolean }>(`/api/sessions/${encodeURIComponent(sessionId)}/stop`, { method: "POST" }); }
export function loadPTYSnapshot(sessionId: string, signal?: AbortSignal) { return request<PTYSnapshot>(`/api/sessions/${encodeURIComponent(sessionId)}/pty/snapshot`, { signal }); }
export function sendMessage(sessionId: string, content: string) { return request<Message>(`/api/sessions/${encodeURIComponent(sessionId)}/messages`, { method: "POST", body: JSON.stringify({ content }) }); }
// SSE via fetch + ReadableStream instead of EventSource, so the bearer token
// can be attached as an Authorization header (EventSource cannot set headers).
// The server frames each event as "data: <json>\n\n" with ":" comment keep-alives.
export function subscribe(sessionId: string, onEvent: (event: Event) => void, onError: () => void) {
  const controller = new AbortController();
  void (async () => {
    try {
      const token = accessToken ? await accessToken() : undefined;
      const response = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/events/stream`, {
        signal: controller.signal,
        headers: token ? { Authorization: `Bearer ${token}` } : undefined,
      });
      if (!response.ok || !response.body) {
        if (!controller.signal.aborted) onError();
        return;
      }
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      let data = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, "\n");
        let boundary: number;
        while ((boundary = buffer.indexOf("\n\n")) !== -1) {
          const block = buffer.slice(0, boundary);
          buffer = buffer.slice(boundary + 2);
          for (const line of block.split("\n")) {
            if (line.startsWith("data:")) data += line.slice(5).replace(/^\s/, "") + "\n";
          }
          if (data) {
            const payload = data.slice(0, -1);
            data = "";
            try { onEvent(JSON.parse(payload) as Event); } catch { onError(); }
          }
        }
      }
      if (!controller.signal.aborted) onError();
    } catch (error) {
      if ((error as Error).name !== "AbortError") onError();
    }
  })();
  return () => controller.abort();
}
