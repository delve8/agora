import { useCallback, useEffect, useState } from "react";
import { createSession, loadEvents, loadState, resumeSession, sendMessage, stopSession, subscribe } from "../api/client";
import type { Coordination, Event, Session } from "../types";
import { derivedSessionName } from "../sessionName";

function linkedSessionFromLocation() {
  if (typeof window === "undefined") return "";
  const match = window.location.pathname.match(/^\/sessions\/([^/]+)/);
  return match ? decodeURIComponent(match[1]) : "";
}

export function useCoordination() {
  const [coordination, setCoordination] = useState<Coordination | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedSessionId, setSelectedSessionId] = useState("");
  const [events, setEvents] = useState<Event[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async (background = false) => {
    if (!background) setLoading(true);
    try {
      const state = await loadState();
      setCoordination(state.coordination);
      setSessions(state.sessions);
      const linkedSession = linkedSessionFromLocation();
      setSelectedSessionId((current) => current && state.sessions.some((session) => session.id === current) ? current : linkedSession && state.sessions.some((session) => session.id === linkedSession) ? linkedSession : state.sessions[0]?.id ?? "");
      setError("");
    } catch (value) { if (!background) setError(value instanceof Error ? value.message : "Unable to load Agora"); }
    finally { if (!background) setLoading(false); }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(true), 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);
  const currentSession = sessions.find((session) => session.id === selectedSessionId);
  const canReadHistory = currentSession?.capabilities.can_read_history ?? false;
  const canStream = currentSession?.capabilities.can_stream ?? false;

  const applyDerivedName = useCallback((sessionId: string, values: Event[]) => {
    setSessions((current) => current.map((session) => {
      if (session.id !== sessionId) return session;
      const derived = derivedSessionName(session, values);
      return derived ? { ...session, display_name: derived.name, display_name_source: derived.source } : session;
    }));
  }, []);

  useEffect(() => {
    if (!selectedSessionId) { setEvents([]); return; }
    const selected = sessions.find((session) => session.id === selectedSessionId);
    if (!selected) { setEvents([]); return; }
    setEvents([]);
    let cancelled = false;
    if (canReadHistory) {
      void loadEvents(selectedSessionId).then((value) => {
        if (cancelled) return;
        setEvents((current) => {
          const byId = new Map(value.map((item) => [item.id, item]));
          for (const item of current) byId.set(item.id, item);
          return [...byId.values()].sort((a, b) => a.created_at.localeCompare(b.created_at));
        });
        applyDerivedName(selectedSessionId, value);
      }).catch((value) => { if (!cancelled) setError(value instanceof Error ? value.message : "Unable to load events"); });
    }
    const unsubscribe = canStream ? subscribe(selectedSessionId, (value) => {
      setEvents((current) => {
        if (current.some((item) => item.id === value.id)) return current;
        const next = [...current, value].sort((a, b) => a.created_at.localeCompare(b.created_at));
        if (value.kind === "user" || value.kind === "ai-title") applyDerivedName(selectedSessionId, next);
        return next;
      });
    }, () => { if (!cancelled) setError("Event stream disconnected; retrying on refresh"); }) : () => {};
    return () => { cancelled = true; unsubscribe(); };
  }, [selectedSessionId, canReadHistory, canStream, applyDerivedName]);

  const addSession = useCallback(async (input: { workspace: string; display_name: string; role: string }) => {
    if (!coordination) throw new Error("coordination is not ready");
    try {
      const session = await createSession(coordination.id, input);
      setSessions((current) => current.some((item) => item.id === session.id) ? current : [...current, session]);
      setSelectedSessionId(session.id);
      setError("");
      return session;
    } catch (value) {
      const message = value instanceof Error ? value.message : "Unable to create session";
      setError(message);
      throw value;
    }
  }, [coordination]);

  const resume = useCallback(async (sessionId: string) => {
    try {
      const resumed = await resumeSession(sessionId);
      setSessions((current) => current.map((item) => item.id === resumed.id ? resumed : item));
      setSelectedSessionId(resumed.id);
      setError("");
      return resumed;
    } catch (value) {
      const message = value instanceof Error ? value.message : "Unable to resume session";
      setError(message);
      throw value;
    }
  }, []);

  const stop = useCallback(async (sessionId: string) => {
    try {
      await stopSession(sessionId);
      setError("");
      await refresh(true);
    } catch (value) {
      const message = value instanceof Error ? value.message : "Unable to stop session";
      setError(message);
      throw value;
    }
  }, [refresh]);

  const send = useCallback(async (sessionId: string, content: string) => sendMessage(sessionId, content), []);

  return { coordination, sessions, currentSession, selectedSessionId, setSelectedSessionId, events, loading, error, setError, addSession, resume, stop, send, refresh };
}
