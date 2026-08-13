import { useCallback, useEffect, useState } from "react";
import { createSession, loadEvents, loadState, sendMessage, subscribe } from "../api/client";
import type { Coordination, Event, Session } from "../types";

export function useCoordination() {
  const [coordination, setCoordination] = useState<Coordination | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedSessionId, setSelectedSessionId] = useState("");
  const [events, setEvents] = useState<Event[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const state = await loadState();
      setCoordination(state.coordination);
      setSessions(state.sessions);
      setSelectedSessionId((current) => current && state.sessions.some((session) => session.id === current) ? current : state.sessions[0]?.id ?? "");
      setError("");
    } catch (value) { setError(value instanceof Error ? value.message : "Unable to load Agora"); }
    finally { setLoading(false); }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);
  const currentSession = sessions.find((session) => session.id === selectedSessionId);

  useEffect(() => {
    if (!selectedSessionId) { setEvents([]); return; }
    let cancelled = false;
    void loadEvents(selectedSessionId).then((value) => { if (!cancelled) setEvents(value); }).catch((value) => { if (!cancelled) setError(value instanceof Error ? value.message : "Unable to load events"); });
    const unsubscribe = subscribe(selectedSessionId, (value) => setEvents((current) => {
      if (current.some((item) => item.id === value.id)) return current;
      return [...current, value].sort((a, b) => a.created_at.localeCompare(b.created_at));
    }), () => { if (!cancelled) setError("Event stream disconnected; retrying on refresh"); });
    return () => { cancelled = true; unsubscribe(); };
  }, [selectedSessionId]);

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

  const send = useCallback(async (sessionId: string, content: string) => sendMessage(sessionId, content), []);

  return { coordination, sessions, currentSession, selectedSessionId, setSelectedSessionId, events, loading, error, setError, addSession, send, refresh };
}
