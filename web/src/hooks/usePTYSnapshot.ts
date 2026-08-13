import { useEffect, useState } from "react";
import { loadPTYSnapshot } from "../api/client";
import type { PTYSnapshot } from "../types";

const POLL_INTERVAL_MS = 750;

type PTYSnapshotState = { snapshot: PTYSnapshot | null; error: string; loading: boolean };

export function usePTYSnapshot(sessionId: string) {
  const [state, setState] = useState<PTYSnapshotState>({ snapshot: null, error: "", loading: false });
  useEffect(() => {
    if (!sessionId) { setState({ snapshot: null, error: "", loading: false }); return; }
    let cancelled = false;
    let timer: number | undefined;
    const controller = new AbortController();
    const poll = async () => {
      if (cancelled) return;
      setState((current) => ({ ...current, loading: current.snapshot === null }));
      try {
        const snapshot = await loadPTYSnapshot(sessionId, controller.signal);
        if (!cancelled) setState({ snapshot, error: "", loading: false });
      } catch (value) {
        if (!cancelled && !controller.signal.aborted) setState((current) => ({ snapshot: current.snapshot, error: value instanceof Error ? value.message : "PTY observation unavailable", loading: false }));
      } finally {
        if (!cancelled) timer = window.setTimeout(() => void poll(), POLL_INTERVAL_MS);
      }
    };
    void poll();
    return () => { cancelled = true; controller.abort(); if (timer !== undefined) window.clearTimeout(timer); };
  }, [sessionId]);
  return state;
}
