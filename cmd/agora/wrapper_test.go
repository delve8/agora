package main

import (
	"errors"
	"testing"
)

// The Claude wrapper cannot carry provider options into a managed session: the
// Daemon builds the Claude argv itself. Anything option-shaped is a Claude CLI
// invocation, so it must run as the real binary instead of failing here.
func TestClaudeWrapperPassesThroughCLIOptions(t *testing.T) {
	cases := [][]string{
		{"--help"},
		{"-h"},
		{"--version"},
		{"-p", "summarise the diff"},
		{"--print", "summarise the diff"},
		{"--resume"},
		{"--model", "opus", "explain this"},
	}
	for _, args := range cases {
		err := runWrapper(args)
		var passthrough passthroughAgentError
		if !errors.As(err, &passthrough) {
			t.Errorf("claude %v: error = %v, want a passthrough answer for the PATH wrapper", args, err)
			continue
		}
		if passthrough.ExitCode() != 76 {
			t.Errorf("claude %v: exit code = %d, want 76", args, passthrough.ExitCode())
		}
	}
}

// A prompt is the one thing the Claude wrapper contributes to a managed session,
// so prompts stay managed.
func TestClaudeWrapperKeepsPromptsManaged(t *testing.T) {
	cases := [][]string{
		{},
		{"fix the failing test"},
		{"fix", "the", "failing", "test"},
	}
	for _, args := range cases {
		if !claudeRunsAnAgentSession(args) {
			t.Errorf("claude %v: treated as a CLI invocation, want a managed Agent session", args)
		}
	}
}

// Claude's own CLI commands act on Claude Code itself. The wrapper would
// otherwise turn `claude update` into a session whose initial prompt is the word
// "update". The list mirrors the "CLI commands" table of the Claude Code CLI
// reference; this test pins the surface it was checked against.
func TestClaudeWrapperPassesThroughManagementCommands(t *testing.T) {
	commands := []string{
		"update", "gateway", "install", "auth", "agents", "attach", "auto-mode",
		"daemon", "doctor", "import", "logs", "mcp", "plugin", "plugins",
		"project", "remote-control", "respawn", "rm", "self-hosted-runner",
		"setup-token", "stop", "kill", "ultrareview",
	}
	for _, command := range commands {
		if _, ok := claudeManagementCommands[command]; !ok {
			t.Errorf("claude %s: missing from the management command set", command)
		}
		if claudeRunsAnAgentSession([]string{command}) {
			t.Errorf("claude %s: treated as an initial prompt, want a CLI passthrough", command)
		}
		// The command word is only a command in front of its arguments.
		if !claudeRunsAnAgentSession([]string{"explain", command}) {
			t.Errorf("claude explain %s: treated as a CLI command, want a managed session", command)
		}
	}
	if len(claudeManagementCommands) != len(commands) {
		t.Fatalf("management command set holds %d entries, the reference lists %d", len(claudeManagementCommands), len(commands))
	}
}

// The wrapper runs in the user's terminal, so it is the only party that can tell
// the Daemon what that terminal is. Without it a managed Agent has no TERM at
// all (the Daemon is a service) and renders for 16 colours.
func TestWrapperForwardsTheRequestingTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.9")
	t.Setenv("AGORA_PI_BINARY", "/tmp/pi")

	values := terminalEnv()
	for key, want := range map[string]string{
		"TERM": "xterm-256color", "COLORTERM": "truecolor",
		"TERM_PROGRAM": "iTerm.app", "TERM_PROGRAM_VERSION": "3.6.9",
	} {
		if values[key] != want {
			t.Errorf("terminal[%s] = %q, want %q", key, values[key], want)
		}
	}

	// A terminal that reports nothing sends nothing, rather than an empty
	// identity the Agent would have to interpret.
	for _, key := range []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION"} {
		t.Setenv(key, "")
	}
	if values := terminalEnv(); values != nil {
		t.Fatalf("terminalEnv() = %v, want nil without a terminal", values)
	}
}
