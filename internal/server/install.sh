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

command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1 || {
	echo "install.sh: curl, wget or python3 is required" >&2
	exit 1
}

# Try every available downloader instead of committing to the first one found.
# Some corporate networks reset TLS connections whose ClientHello looks like
# curl's while allowing wget or python, so a curl that is installed but blocked
# must not stop the install.
download() { # <url> <destination>
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2" 2>/dev/null && return 0
	fi
	if command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1" && return 0
	fi
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])' "$1" "$2" && return 0
	fi
	return 1
}

# The daemon resolves the provider it launches itself, and service managers start
# it with a minimal environment (launchd gives `/usr/bin:/bin:/usr/sbin:/sbin`).
# A `pi` installed through nvm or Homebrew is then invisible, the daemon cannot
# start it, and the wrapper blocks until its socket read times out. Record the
# real provider executables at install time, skipping the Agora wrapper itself,
# and bake them plus this PATH into the service below.
resolve_agent() { # <name>
	name="$1"
	restore_ifs=$IFS
	IFS=:
	for dir in $PATH; do
		[ -n "$dir" ] || continue
		candidate="$dir/$name"
		[ -x "$candidate" ] || continue
		if [ -L "$candidate" ]; then
			target=$(readlink "$candidate" 2>/dev/null || true)
			case "$target" in *agora-wrapper.sh*) continue ;; esac
		fi
		if head -c 512 "$candidate" 2>/dev/null | grep -q "agora wrap"; then
			continue
		fi
		IFS=$restore_ifs
		printf '%s' "$candidate"
		return 0
	done
	IFS=$restore_ifs
	return 1
}

xml_escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

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
if ! download "$BASE_URL/download/$artifact" "$tmp/agora"; then
	echo "install.sh: failed to download $BASE_URL/download/$artifact" >&2
	exit 1
fi
chmod +x "$tmp/agora"

# Checksum verification is best effort: an older Server may not publish one.
if download "$BASE_URL/download/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
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

# Install the PATH wrapper next to the binary and expose it as `pi` and
# `claude`, so the native commands create Agora-managed sessions. An existing
# provider binary that is not our wrapper is never replaced.
if download "$BASE_URL/download/agora-wrapper.sh" "$tmp/agora-wrapper.sh"; then
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$tmp/agora-wrapper.sh" "$INSTALL_DIR/agora-wrapper.sh"
	else
		cp "$tmp/agora-wrapper.sh" "$INSTALL_DIR/agora-wrapper.sh"
		chmod 0755 "$INSTALL_DIR/agora-wrapper.sh"
	fi
	for name in pi claude; do
		target="$INSTALL_DIR/$name"
		if [ -L "$target" ] && [ "$(basename "$(readlink "$target")")" = "agora-wrapper.sh" ]; then
			ln -sf agora-wrapper.sh "$target"
			echo "Refreshed $target -> agora-wrapper.sh"
		elif [ -e "$target" ] || [ -L "$target" ]; then
			echo "Note: $target exists and is not the Agora wrapper; leaving it untouched." >&2
		else
			ln -sf agora-wrapper.sh "$target"
			echo "Installed $target -> agora-wrapper.sh"
		fi
	done
else
	echo "Note: could not download the PATH wrapper; pi and claude will not route through Agora." >&2
fi

case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*) echo "Note: add $INSTALL_DIR to PATH so 'agora', 'pi' and 'claude' resolve." ;;
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

SERVICE_PATH="$PATH"
PI_BINARY="$(resolve_agent pi || true)"
CLAUDE_BINARY="$(resolve_agent claude || true)"
if [ -n "$PI_BINARY" ]; then
	echo "Recording provider pi: $PI_BINARY"
fi
if [ -n "$CLAUDE_BINARY" ]; then
	echo "Recording provider claude: $CLAUDE_BINARY"
fi

if [ "$os" = "linux" ]; then
	if ! command -v systemctl >/dev/null 2>&1; then
		echo "install.sh: systemctl not found; start the daemon manually: $BIN daemon" >&2
		exit 0
	fi
	unit_dir="$HOME/.config/systemd/user"
	mkdir -p "$unit_dir"
	{
		cat <<UNIT
[Unit]
Description=Agora Daemon
Documentation=$SERVER_URL
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN daemon
Restart=always
RestartSec=5
Environment="PATH=$SERVICE_PATH"
UNIT
		if [ -n "$PI_BINARY" ]; then
			printf 'Environment="AGORA_PI_BINARY=%s"\n' "$PI_BINARY"
		fi
		if [ -n "$CLAUDE_BINARY" ]; then
			printf 'Environment="AGORA_CLAUDE_BINARY=%s"\n' "$CLAUDE_BINARY"
		fi
		cat <<UNIT

[Install]
WantedBy=default.target
UNIT
	} > "$unit_dir/agora-daemon.service"
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
	{
		cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.delve8.agora.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>$(xml_escape "$BIN")</string>
		<string>daemon</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>$(xml_escape "$SERVICE_PATH")</string>
PLIST
		if [ -n "$PI_BINARY" ]; then
			printf '\t\t<key>AGORA_PI_BINARY</key>\n\t\t<string>%s</string>\n' "$(xml_escape "$PI_BINARY")"
		fi
		if [ -n "$CLAUDE_BINARY" ]; then
			printf '\t\t<key>AGORA_CLAUDE_BINARY</key>\n\t\t<string>%s</string>\n' "$(xml_escape "$CLAUDE_BINARY")"
		fi
		cat <<PLIST
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$(xml_escape "$HOME/.agora/daemon.log")</string>
	<key>StandardErrorPath</key>
	<string>$(xml_escape "$HOME/.agora/daemon.log")</string>
</dict>
</plist>
PLIST
	} > "$plist"
	launchctl unload "$plist" >/dev/null 2>&1 || true
	launchctl load "$plist"
	echo "Installed and started LaunchAgent com.delve8.agora.daemon"
	echo "  status: launchctl list | grep agora"
	echo "  logs:   tail -f $HOME/.agora/daemon.log"
fi
