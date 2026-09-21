package main

import (
	"fmt"
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
	if sessionID == "" && !piRunsAnAgentSession(args) {
		// Pi's own CLI commands and one-shot switches are not Agent sessions:
		// there is no TUI to drive and nothing to attach to later. Handing them
		// to the local Daemon would create a session around a command that
		// exits on its own (`pi update`, `pi install`, `pi -p ...`). The PATH
		// wrapper runs the real Pi binary for this exit code instead.
		return passthroughAgentError{agent: "pi"}
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

// piManagementCommands act on Pi itself. They never open an interactive
// session, so Agora has nothing to own. The set mirrors the Commands block of
// `pi --help` and the "Package Commands" section of Pi's README (pi 0.85.1);
// Pi decides one of these is a command only when it is the first argument it
// sees. Maintained by hand: a subcommand and a prompt cannot be told apart by
// shape, so there is no version-independent way to derive it.
var piManagementCommands = map[string]bool{
	"install":   true,
	"remove":    true,
	"uninstall": true,
	"update":    true,
	"list":      true,
	"config":    true,
	"auth":      true,
}

// piOneShotOptions make Pi print and exit instead of opening a TUI. `--print`
// is the documented scripting mode, so its output must reach the caller's stdout
// directly; a PTY would rewrite every newline and Agora would own a session
// around a command that already finished.
var piOneShotOptions = map[string]bool{
	"--help": true, "-h": true,
	"--version": true, "-v": true,
	"--print": true, "-p": true,
	"--export": true, "--list-models": true,
}

// piNonInteractiveModes are the `--mode` values that stream events instead of
// opening the Agent TUI. `--mode text` is Pi's default and stays managed, so an
// explicit mode is only read for these two values.
var piNonInteractiveModes = map[string]bool{"json": true, "rpc": true}

// piRunsAnAgentSession reports whether this invocation starts something a
// managed session can own. It mirrors Pi's own reading of argv, so `pi update`
// runs the updater while `pi -- update` (everything after `--` is a message) and
// `pi "update that file"` stay Agent sessions.
func piRunsAnAgentSession(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if piOneShotOptions[arg] {
			return false
		}
		if arg == "--mode" {
			if index+1 < len(args) && piNonInteractiveModes[args[index+1]] {
				return false
			}
			index++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--mode="); ok && piNonInteractiveModes[value] {
			return false
		}
	}
	if len(args) == 0 || args[0] == "--" {
		return true
	}
	return !piManagementCommands[args[0]]
}

func looksLikeAgoraSessionID(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "sess-") || strings.HasPrefix(value, "daemon/")
}

// passthroughAgentError tells the PATH wrapper that this invocation is a
// provider CLI command, not an Agent session, so it must run the original binary
// directly instead of attaching. The exit code is distinct from EX_TEMPFAIL (75,
// which means the local Daemon was unreachable) so the wrapper can report the
// right reason.
type passthroughAgentError struct {
	agent string
}

func (e passthroughAgentError) Error() string {
	return fmt.Sprintf("%s: not an Agent session", e.agent)
}

func (e passthroughAgentError) ExitCode() int { return 76 }
