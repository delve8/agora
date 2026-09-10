import type { Session } from "./types";

const GENERATED_NAMES = new Set(["", "New session", "Claude Code", "Claude Code Proxy"]);

export function isGeneratedSessionName(value: string) {
  return GENERATED_NAMES.has(value.trim());
}

// Session names are owned by the Server. It derives them from the complete
// provider transcript (a `/name` rename, an AI title, or the first user
// message), persists the source, and pushes updates to every client.
//
// The Web UI deliberately does not derive names from the events it has loaded:
// it only ever sees a window of the conversation, so "the first user message"
// there is usually a recent message rather than the session's opening one.
// Deriving locally made the label flip every few seconds between that guess and
// the Server's value, because history polling and the state poll re-applied
// their own writer in turn.
export function sessionLabel(displayName: string, id: string) {
  return isGeneratedSessionName(displayName) ? "New session" : displayName || id;
}

export type { Session };
