import { useCallback, useEffect, useRef, useState } from "react";
import { createSession, loadEvents, loadState, resumeSession, sendMessage, stopSession, subscribe } from "../api/client";
import type { CreateSessionInput } from "../api/client";
import type { Coordination, Event, Session } from "../types";

const INITIAL_EVENT_COUNT = 60;
const OLDER_EVENT_PAGE_SIZE = 50;

function linkedSessionFromLocation() {
  if (typeof window === "undefined") return "";
  const match = window.location.pathname.match(/^\/sessions\/([^/]+)/);
  return match ? decodeURIComponent(match[1]) : "";
}

export function useCoordination() {
  const [coordination, setCoordination] = useState<Coordination | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedSessionId, setSelectedSessionId] = useState("");
  const sessionsRef = useRef<Session[]>([]);
  const selectedSessionIdRef = useRef("");
  const [events, setEvents] = useState<Event[]>([]);
  sessionsRef.current = sessions;
  selectedSessionIdRef.current = selectedSessionId;
  const [hasOlderEvents, setHasOlderEvents] = useState(false);
  const [loadingOlderEvents, setLoadingOlderEvents] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async (background = false) => {
    if (!background) setLoading(true);
    try {
      const state = await loadState();
      setCoordination(state.coordination);
      const previousSessions = sessionsRef.current;
      const previousSelectedID = selectedSessionIdRef.current;
      setSessions(state.sessions);
      const linkedSession = linkedSessionFromLocation();
      let nextSelectedID = previousSelectedID && state.sessions.some((session) => session.id === previousSelectedID) ? previousSelectedID : "";
      // A runtime /resume changes the canonical session ID while the PTY and
      // process continue running. The old ID disappears from /api/state, so
      // preserving only the string ID would make the UI jump to the first
      // session (or show a different workspace). Match the replacement by
      // the stable live-process tuple and follow the rebind automatically.
      if (!nextSelectedID && previousSelectedID) {
        const previous = previousSessions.find((session) => session.id === previousSelectedID);
        const replacement = previous && (previous.process_id ?? 0) > 0
          ? state.sessions.find((session) => session.id !== previousSelectedID && session.daemon_id === previous.daemon_id && session.agent === previous.agent && session.workspace === previous.workspace && session.process_id === previous.process_id && session.capabilities.can_send_input)
          : undefined;
        nextSelectedID = replacement?.id ?? "";
      }
      const fallbackID = linkedSession && state.sessions.some((session) => session.id === linkedSession)
        ? linkedSession
        : state.sessions[0]?.id ?? "";
      setSelectedSessionId(nextSelectedID || fallbackID);
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

  useEffect(() => {
    if (!selectedSessionId) { setEvents([]); return; }
    const selected = sessions.find((session) => session.id === selectedSessionId);
    if (!selected) { setEvents([]); return; }
    setEvents([]);
    setHasOlderEvents(false);
    setLoadingOlderEvents(false);
    let cancelled = false;
    let firstLoad = true;
    let historyLoaded = false;
    const loadCurrentEvents = (showError: boolean) => {
      if (!canReadHistory) return;
      void loadEvents(selectedSessionId, { limit: INITIAL_EVENT_COUNT }).then((value) => {
        if (cancelled) return;
        setEvents((current) => {
          const byId = new Map(current.map((item) => [item.id, item]));
          for (const item of value) byId.set(item.id, item);
          return [...byId.values()].sort((a, b) => a.created_at.localeCompare(b.created_at));
        });
        if (firstLoad) {
          setHasOlderEvents(value.length >= INITIAL_EVENT_COUNT);
          firstLoad = false;
          historyLoaded = true;
        }
        if (showError) setError("");
      }).catch((value) => {
        if (showError && !cancelled) setError(value instanceof Error ? value.message : "Unable to load events");
      });
    };
    loadCurrentEvents(true);
    // The Pi RPC stream is best-effort, while its JSONL history is the
    // authoritative transcript. Poll Pi history so messages written by an
    // external Pi process (or emitted during an observer startup race) appear
    // without requiring a manual session switch or page refresh.
    // Retry the first history request for every provider. During a server or
    // daemon restart the session list may become available before the session
    // route/history authorization does; a transient 403/502 must not leave the
    // conversation permanently empty after the error disappears.
    const historyTimer = canReadHistory ? window.setInterval(() => {
      if (!historyLoaded || selected.agent === "pi") loadCurrentEvents(false);
    }, 2000) : undefined;
    const unsubscribe = canStream ? subscribe(selectedSessionId, (value) => {
      setEvents((current) => {
        if (current.some((item) => item.id === value.id)) return current;
        return [...current, value].sort((a, b) => a.created_at.localeCompare(b.created_at));
      });
    }, () => { if (!cancelled) setError("Event stream disconnected; retrying on refresh"); }) : () => {};
    return () => { cancelled = true; if (historyTimer !== undefined) window.clearInterval(historyTimer); unsubscribe(); };
  }, [selectedSessionId, canReadHistory, canStream]);

  const loadOlderEvents = useCallback(async () => {
    if (!selectedSessionId || !canReadHistory || !hasOlderEvents || loadingOlderEvents || events.length === 0) return;
    const oldest = events[0];
    setLoadingOlderEvents(true);
    try {
      const value = await loadEvents(selectedSessionId, { limit: OLDER_EVENT_PAGE_SIZE, before: oldest.id });
      setEvents((current) => {
        const byId = new Map(value.map((item) => [item.id, item]));
        for (const item of current) byId.set(item.id, item);
        return [...byId.values()].sort((a, b) => a.created_at.localeCompare(b.created_at));
      });
      setHasOlderEvents(value.length >= OLDER_EVENT_PAGE_SIZE);
    } catch (value) {
      setError(value instanceof Error ? value.message : "Unable to load older events");
    } finally {
      setLoadingOlderEvents(false);
    }
  }, [canReadHistory, events, hasOlderEvents, loadingOlderEvents, selectedSessionId]);

  const addSession = useCallback(async (input: CreateSessionInput) => {
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

  return { coordination, sessions, currentSession, selectedSessionId, setSelectedSessionId, events, hasOlderEvents, loadingOlderEvents, loadOlderEvents, loading, error, setError, addSession, resume, stop, send, refresh };
}
