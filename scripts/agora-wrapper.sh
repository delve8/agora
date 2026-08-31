#!/usr/bin/env bash
# Generic PATH wrapper for Agora-managed native Agent sessions.
#
# Install it under an agent name, for example:
#   ln -sf /path/to/agora/scripts/agora-wrapper.sh ~/.local/bin/pi
# Then `pi` creates the session through Agora and attaches to the PTY owned by
# the local Daemon. The script itself never starts the real Agent binary.
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
    for candidate in "$script_dir/../bin/agora" "$script_dir/../agora"; do
      if [[ -x "$candidate" ]]; then
        agora_bin="$candidate"
        break
      fi
    done
  fi
fi

if [[ -z "$agora_bin" || ! -x "$agora_bin" ]]; then
  echo "agora wrapper: Agora binary not found; set AGORA_BIN or put agora on PATH" >&2
  exit 1
fi

case "$agent" in
  pi)
    exec "$agora_bin" pi-wrapper "$@"
    ;;
  claude|claude-code)
    exec "$agora_bin" wrapper "$@"
    ;;
esac
