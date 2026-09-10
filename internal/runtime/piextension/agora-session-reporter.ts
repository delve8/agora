/**
 * Agora session reporter.
 *
 * The Agora Session Host injects this file into managed Agent processes as
 * `pi -e <path>`. The provider then tells Agora which session the Agent is
 * using, so the Daemon can rebind immediately and exactly.
 *
 * Without it Agora has to infer a context switch from terminal keystrokes and
 * transcript growth, which cannot be exact: `/resume` writes nothing to any
 * transcript, a TUI submits a command chosen from its completion menu rather
 * than the typed prefix, and input methods stream preedit text that never
 * reaches the stored message.
 *
 * Reporting is best effort by design. A missing Daemon, a missing socket or any
 * error here must never affect the Agent, so every failure is swallowed.
 */

import { connect } from "node:net";
import { homedir } from "node:os";
import { join } from "node:path";

const HOST_ENV = "AGORA_HOST_ID";
const SOCKET_ENV = "AGORA_DAEMON_SOCKET";
const REPORT_TYPE = "session.report";
const TIMEOUT_MS = 2000;

// The Daemon's local control socket, resolved the same way the terminal wrapper
// resolves it.
function daemonSocket() {
  const configured = String(process.env[SOCKET_ENV] ?? "").trim();
  if (configured) return configured;
  return join(homedir(), ".agora", "daemon.sock");
}

// report sends one session report to the Daemon and never throws.
function report(payload) {
  const hostID = String(process.env[HOST_ENV] ?? "").trim();
  if (!hostID) return;
  let socket;
  try {
    socket = connect(daemonSocket());
  } catch {
    return;
  }
  try {
    socket.unref();
    socket.setTimeout(TIMEOUT_MS, () => socket.destroy());
    socket.on("error", () => socket.destroy());
    socket.on("connect", () => {
      try {
        socket.end(JSON.stringify({ type: REPORT_TYPE, host_id: hostID, ...payload }) + "\n");
      } catch {
        socket.destroy();
      }
    });
  } catch {
    try {
      socket.destroy();
    } catch {
      // ignore
    }
  }
}

// sessionFileOf reads the provider's authoritative session file, when the
// context exposes it.
function sessionFileOf(ctx) {
  try {
    return String(ctx?.sessionManager?.getSessionFile?.() ?? "");
  } catch {
    return "";
  }
}

function sessionIDOf(ctx) {
  try {
    return String(ctx?.sessionManager?.getSessionId?.() ?? "");
  } catch {
    return "";
  }
}

function sessionNameOf(ctx) {
  try {
    return String(ctx?.sessionManager?.getSessionName?.() ?? "");
  } catch {
    return "";
  }
}

export default function (pi) {
  // Fired before /new or /resume performs the switch. This is the earliest
  // authoritative signal, and for "resume" it already names the target file.
  pi.on("session_before_switch", async (event) => {
    report({
      reason: String(event?.reason ?? "switch"),
      target_session_file: String(event?.targetSessionFile ?? ""),
    });
  });

  // Fired after the switch completed, for every source: startup, /new,
  // /resume, /fork and /clone. The context now reports the current session.
  pi.on("session_start", async (event, ctx) => {
    report({
      reason: String(event?.reason ?? "startup"),
      session_file: sessionFileOf(ctx),
      session_id: sessionIDOf(ctx),
      session_name: sessionNameOf(ctx),
      previous_session_file: String(event?.previousSessionFile ?? ""),
    });
  });
}
