#!/bin/sh
# Agora daemon installer.
#
# Generated and served by the Agora Server, which fills in the Server URL and
# the download base URL below. Typical use, from the Add device panel:
#
#   curl -fsSL https://agora.example.com/download/install.sh | sh -s -- --pair <code>
#
# The script downloads the daemon for this machine, remembers the pairing, and
# installs a user-level background service (systemd --user on Linux, a launchd
# LaunchAgent on macOS) so the daemon keeps running and starts on login.
set -eu

SERVER_URL="{{ .ServerURL }}"
BASE_URL="{{ .BaseURL }}"
PAIR_CODE=""
INSTALL_DIR="${AGORA_INSTALL_DIR:-$HOME/.local/bin}"
WITH_SERVICE=1

usage() {
	cat <<'USAGE'
usage: install.sh [--pair <code>] [--server <url>] [--install-dir <dir>] [--no-service]

  --pair <code>        pairing code from the Agora Web UI (Add device)
  --server <url>       Agora Server base URL (defaults to the one this script came from)
  --install-dir <dir>  where to put the agora binary (default: ~/.local/bin)
  --no-service         install the binary only; do not set up a background service
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
		--pair) PAIR_CODE="${2:-}"; shift 2 ;;
		--pair=*) PAIR_CODE="${1#*=}"; shift ;;
		--server) SERVER_URL="${2:-}"; shift 2 ;;
		--server=*) SERVER_URL="${1#*=}"; shift ;;
		--install-dir) INSTALL_DIR="${2:-}"; shift 2 ;;
		--install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
		--no-service) WITH_SERVICE=0; shift ;;
		-h|--help) usage; exit 0 ;;
		*) echo "install.sh: unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done

[ -n "$SERVER_URL" ] || { echo "install.sh: no Server URL; pass --server <url>" >&2; exit 1; }
SERVER_URL=$(printf '%s' "$SERVER_URL" | sed 's:/*$::')
BASE_URL=$(printf '%s' "$BASE_URL" | sed 's:/*$::')
[ -n "$BASE_URL" ] || BASE_URL="$SERVER_URL"

command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || {
	echo "install.sh: curl or wget is required" >&2
	exit 1
}

fetch() { # <url> <destination>
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		return 1
	fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux|darwin) ;;
	*) echo "install.sh: unsupported operating system: $os" >&2; exit 1 ;;
esac

machine=$(uname -m)
case "$machine" in
	x86_64|amd64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) echo "install.sh: unsupported architecture: $machine" >&2; exit 1 ;;
esac

artifact="agora-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $artifact ..."
if ! fetch "$BASE_URL/download/$artifact" "$tmp/agora"; then
	echo "install.sh: failed to download $BASE_URL/download/$artifact" >&2
	exit 1
fi
chmod +x "$tmp/agora"

# Checksum verification is best effort: an older Server may not publish one.
if fetch "$BASE_URL/download/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	expected=$(awk -v name="$artifact" '$2 == name { print $1 }' "$tmp/checksums.txt" | head -1)
	if [ -n "$expected" ]; then
		if command -v sha256sum >/dev/null 2>&1; then
			actual=$(sha256sum "$tmp/agora" | awk '{ print $1 }')
		elif command -v shasum >/dev/null 2>&1; then
			actual=$(shasum -a 256 "$tmp/agora" | awk '{ print $1 }')
		else
			actual=""
		fi
		if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
			echo "install.sh: checksum mismatch for $artifact" >&2
			echo "  expected $expected" >&2
			echo "  got      $actual" >&2
			exit 1
		fi
	fi
fi

mkdir -p "$INSTALL_DIR"
if command -v install >/dev/null 2>&1; then
	install -m 0755 "$tmp/agora" "$INSTALL_DIR/agora"
else
	cp "$tmp/agora" "$INSTALL_DIR/agora"
	chmod 0755 "$INSTALL_DIR/agora"
fi
BIN="$INSTALL_DIR/agora"
echo "Installed $BIN"

case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*) echo "Note: add $INSTALL_DIR to PATH to run 'agora' directly." ;;
esac

if [ -n "$PAIR_CODE" ]; then
	echo "Pairing with $SERVER_URL ..."
	AGORA_SERVER_URL="$SERVER_URL" "$BIN" pair "$PAIR_CODE"
else
	echo "No pairing code given; run '$BIN pair <code>' once you have one."
fi

if [ "$WITH_SERVICE" = 0 ]; then
	echo "Skipping background service (--no-service). Start it with: $BIN daemon"
	exit 0
fi

if [ "$os" = "linux" ]; then
	if ! command -v systemctl >/dev/null 2>&1; then
		echo "install.sh: systemctl not found; start the daemon manually: $BIN daemon" >&2
		exit 0
	fi
	unit_dir="$HOME/.config/systemd/user"
	mkdir -p "$unit_dir"
	cat > "$unit_dir/agora-daemon.service" <<UNIT
[Unit]
Description=Agora Daemon
Documentation=$SERVER_URL
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN daemon
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
	systemctl --user daemon-reload
	systemctl --user enable --now agora-daemon.service
	if command -v loginctl >/dev/null 2>&1; then
		if ! loginctl enable-linger "$(id -un)" >/dev/null 2>&1; then
			echo "Note: run 'sudo loginctl enable-linger $(id -un)' so the daemon starts without an interactive login." >&2
		fi
	fi
	echo "Installed and started systemd user service agora-daemon.service"
	echo "  status: systemctl --user status agora-daemon"
	echo "  logs:   journalctl --user -u agora-daemon -f"
elif [ "$os" = "darwin" ]; then
	agents="$HOME/Library/LaunchAgents"
	mkdir -p "$agents" "$HOME/.agora"
	plist="$agents/com.delve8.agora.daemon.plist"
	cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.delve8.agora.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>$BIN</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$HOME/.agora/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>$HOME/.agora/daemon.log</string>
</dict>
</plist>
PLIST
	launchctl unload "$plist" >/dev/null 2>&1 || true
	launchctl load "$plist"
	echo "Installed and started LaunchAgent com.delve8.agora.daemon"
	echo "  status: launchctl list | grep agora"
	echo "  logs:   tail -f $HOME/.agora/daemon.log"
fi
