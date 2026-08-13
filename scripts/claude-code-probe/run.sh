#!/usr/bin/env bash
# Agora Phase 0 probe: capture real Claude Code stream-json behavior.
#
# Run scenarios 01-03 (pure input/output capture) by default. These do not
# require network or API credentials and record raw stdin/stdout/exit codes.
# Scenarios 04+ are optional (they drive real model turns and API calls):
#   CLAUDE_PROBE_RUN=1 ./scripts/claude-code-probe/run.sh
#
# Every output goes under /tmp/agora-probe-<timestamp>/ so nothing from a probe
# run is committed to the repository.
set -euo pipefail

CLAUDE_BIN="${CLAUDE_BIN:-claude}"
CLAUDE_VERSION="$("$CLAUDE_BIN" --version 2>/dev/null || echo unknown)"
WORK="$(mktemp -d /tmp/agora-probe-XXXXXX)"
mkdir -p "$WORK"

log() { printf '\n[probe] %s\n' "$*" >&2; }
run() { # name stdin_file
  local name="$1" stdin="$2"
  log "--- scenario: $name"
  log "command: $CLAUDE_BIN -p --output-format stream-json --input-format stream-json --verbose"
  log "stdin:  $(cat "$stdin")"
  log "version: $CLAUDE_VERSION"
  set +e
  "$CLAUDE_BIN" -p --output-format stream-json --input-format stream-json --verbose \
    <"$stdin" >"$WORK/$name.stdout" 2>"$WORK/$name.stderr"
  local code=$?
  set -e
  log "exit code: $code"
  log "stdout bytes: $(wc -c <"$WORK/$name.stdout")  stderr bytes: $(wc -c <"$WORK/$name.stderr")"
  log "stderr:"
  cat "$WORK/$name.stderr" >&2 || true
}

# --- Scenario 01: empty stdin -------------------------------------------------
printf '' >"$WORK/empty.jsonl"
run "01-empty" "$WORK/empty.jsonl"

# --- Scenario 02: minimal valid stream-json user message -----------------------
printf '{"type":"user","message":{"role":"user","content":"hi"}}\n' >"$WORK/minimal.jsonl"
run "02-minimal-user" "$WORK/minimal.jsonl"

# --- Scenario 03: invalid json --------------------------------------------------
printf 'not-json\n' >"$WORK/bad.jsonl"
run "03-invalid-json" "$WORK/bad.jsonl"

# --- Scenario 04 (optional): single real model turn -----------------------------
if [[ "${CLAUDE_PROBE_RUN:-0}" == "1" ]]; then
  log "running optional scenario 04 (real model turn, requires API)"
  printf '{"type":"user","message":{"role":"user","content":"Reply with the single word ok."}}\n' >"$WORK/turn.jsonl"
  run "04-real-turn" "$WORK/turn.jsonl"

  log "--- scenario: 05 session id + resume"
  set +e
  "$CLAUDE_BIN" -p --output-format stream-json --input-format stream-json \
    --session-id "$(uuidgen)" <"$WORK/turn.jsonl" >"$WORK/05-session.stdout" 2>"$WORK/05-session.stderr"
  echo "exit code: $?" >&2
  set -e
fi

log "probe artifacts: $WORK"
log "inspect with:  jq -c . '$WORK/02-minimal-user.stdout' | head"
