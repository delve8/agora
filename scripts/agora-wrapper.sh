#!/usr/bin/env bash
# Generic PATH wrapper for Agora-managed native Agent sessions.
#
# Install it under an agent name, for example:
#   ln -sf /path/to/agora/scripts/agora-wrapper.sh ~/.local/bin/pi
# Then `pi` creates the session through Agora and attaches to the PTY owned by
# the local Daemon. If the Daemon is unavailable, the script logs the fallback
# and starts the real Agent binary directly.
set -euo pipefail

invoked_as="${AGORA_WRAPPER_NAME:-$(basename -- "$0")}"
agent="${AGORA_WRAPPER_AGENT:-${AGORA_AGENT:-}}"

if [[ -z "$agent" ]]; then
  case "$invoked_as" in
    pi|pi-wrapper|agora-pi)
      agent="pi"
      ;;
    claude|claude-code|wrapper|agora-claude)
      agent="claude"
      ;;
    *)
      echo "agora wrapper: cannot infer agent from '$invoked_as'; set AGORA_WRAPPER_AGENT=pi or claude" >&2
      exit 2
      ;;
  esac
fi

case "$agent" in
  pi|claude|claude-code)
    ;;
  *)
    echo "agora wrapper: unsupported agent '$agent' (expected pi or claude)" >&2
    exit 2
    ;;
esac

agora_bin="${AGORA_BIN:-}"
if [[ -z "$agora_bin" ]]; then
  if command -v agora >/dev/null 2>&1; then
    agora_bin="$(command -v agora)"
  else
    script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
    for candidate in "$script_dir/agora" "$script_dir/../bin/agora" "$script_dir/../agora"; do
      if [[ -x "$candidate" ]]; then
        agora_bin="$candidate"
        break
      fi
    done
  fi
fi

is_existing_agora_session() {
  [[ "${1:-}" == sess-* || "${1:-}" == daemon/* ]]
}

is_agora_wrapper() {
  local candidate="$1"
  [[ "$(basename -- "$candidate")" == "agora-wrapper.sh" ]] && return 0
  grep -q "Generic PATH wrapper for Agora-managed native Agent sessions" "$candidate" 2>/dev/null
}

resolve_original_agent() {
  local configured="$1"
  local name="$2"
  local candidate dir target
  target="$name"
  [[ -n "$configured" ]] && target="$configured"

  if [[ -n "$configured" && "$configured" == */* ]]; then
    if [[ ! -x "$configured" || -d "$configured" ]]; then
      echo "agora wrapper: configured $name binary is not executable: $configured" >&2
      return 1
    fi
    if is_agora_wrapper "$configured"; then
      echo "agora wrapper: configured $name binary points to the Agora wrapper: $configured" >&2
      return 1
    fi
    printf '%s\n' "$configured"
    return 0
  fi

  # Search every PATH entry instead of using `command -v`, because the first
  # result may be this wrapper installed under `pi` or `claude`.
  local -a path_entries=()
  IFS=: read -ra path_entries <<< "${PATH:-}"
  for dir in "${path_entries[@]}"; do
    [[ -z "$dir" ]] && dir="."
    candidate="$dir/$target"
    if [[ -x "$candidate" && ! -d "$candidate" ]] && ! is_agora_wrapper "$candidate"; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  case "$name" in
    pi) echo "agora wrapper: original pi executable not found; set AGORA_PI_BINARY" >&2 ;;
    claude) echo "agora wrapper: original claude executable not found; set AGORA_CLAUDE_BINARY" >&2 ;;
  esac
  return 1
}

run_original_agent() {
  local reason="${1:-local daemon is unavailable}"
  shift || true
  local original_name="$agent"
  local configured=""
  case "$agent" in
    pi) configured="${AGORA_PI_BINARY:-${PI_BINARY:-}}" ;;
    claude|claude-code)
      original_name="claude"
      configured="${AGORA_CLAUDE_BINARY:-${CLAUDE_BINARY:-}}"
      ;;
  esac
  local original
  if ! original="$(resolve_original_agent "$configured" "$original_name")"; then
    exit 1
  fi
  echo "agora wrapper: $reason; starting original $original_name directly: $original" >&2
  exec "$original" "$@"
}

# If the local Daemon is not listening, do not make the native Agent
# unusable. Start the original provider directly and preserve all arguments.
daemon_socket="${AGORA_DAEMON_SOCKET:-${HOME:-}/.agora/daemon.sock}"
if [[ ! -S "$daemon_socket" ]]; then
  if is_existing_agora_session "${1:-}"; then
    echo "agora wrapper: local daemon is unavailable; cannot attach to existing session ${1}" >&2
    exit 1
  fi
  run_original_agent "local daemon is unavailable" "$@"
fi

if [[ -n "$agora_bin" && -x "$agora_bin" ]]; then
  # Exit code 75 (EX_TEMPFAIL) means the local Daemon could not be reached, and
  # 76 means the invocation is a provider CLI command rather than an Agent
  # session (`pi update`, `pi -p ...`). Both run the original binary unchanged;
  # validation and Agent errors must still be returned to the caller.
  if "$agora_bin" wrap "$agent" "$@"; then
    exit 0
  else
    status=$?
  fi
  if ! is_existing_agora_session "${1:-}"; then
    case "$status" in
      75) run_original_agent "local daemon is unavailable" "$@" ;;
      76) run_original_agent "$agent was invoked as a CLI command, not an Agent session" "$@" ;;
    esac
  fi
  exit "$status"
fi

if is_existing_agora_session "${1:-}"; then
  echo "agora wrapper: Agora binary not found; cannot attach to existing session ${1}" >&2
  exit 1
fi
run_original_agent "Agora binary not found" "$@"
