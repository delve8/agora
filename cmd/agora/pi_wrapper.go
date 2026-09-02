package main

import (
	"strings"

	"github.com/delve8/agora/internal/protocol"
)

// runPiWrapper creates (or attaches to) a Pi session and renders its native
// TUI through the same PTY attach path used by the Claude wrapper.
func runPiWrapper(args []string) error {
	sessionID := ""
	if len(args) > 0 && looksLikeAgoraSessionID(args[0]) {
		sessionID = strings.TrimSpace(args[0])
		args = args[1:]
	}

	var attached protocol.WrapperResponse
	var err error
	if sessionID == "" {
		// Pi owns its complete CLI contract. Do not parse, filter, or rewrite
		// arguments here; this keeps --session and future Pi flags working.
		attached, err = createDaemonWrapperSessionWithArgs("pi", args)
	} else {
		// An Agora session ID is wrapper syntax, not a Pi argument. It is the
		// only argument interpreted by this command, and attaches directly to
		// the already-running managed PTY.
		attached, err = attachDaemonWrapperSession(sessionID)
	}
	if err != nil {
		return err
	}
	return runAttachSocket(attached.Socket)
}

func looksLikeAgoraSessionID(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "sess-") || strings.HasPrefix(value, "daemon/")
}
