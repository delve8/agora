package runtime

import (
	"context"
	"strings"
	"testing"
)

// A Daemon started as a service has no TERM, and an Agent without one renders
// for 16 colours instead of the terminal's real capabilities.
func TestAgentEnvDefaultsTheTerminal(t *testing.T) {
	env := agentEnv(context.Background(), []string{"PATH=/usr/bin"})
	if got := envLookup(env, "TERM"); got != defaultTerminal {
		t.Fatalf("TERM = %q, want %q", got, defaultTerminal)
	}
	if got := envLookup(env, "PATH"); got != "/usr/bin" {
		t.Fatalf("PATH = %q, want the base environment preserved", got)
	}
}

// The client that asks for the session knows the terminal; the Daemon does not.
func TestAgentEnvUsesTheRequestingTerminal(t *testing.T) {
	ctx := WithTerminalEnv(context.Background(), map[string]string{
		"TERM":                 "xterm-256color",
		"COLORTERM":            "truecolor",
		"TERM_PROGRAM":         "iTerm.app",
		"TERM_PROGRAM_VERSION": "3.6.9",
	})
	env := agentEnv(ctx, []string{"TERM=dumb", "PATH=/usr/bin"})
	if got := envLookup(env, "TERM"); got != "xterm-256color" {
		t.Fatalf("TERM = %q, want the client's terminal", got)
	}
	if got := envLookup(env, "COLORTERM"); got != "truecolor" {
		t.Fatalf("COLORTERM = %q, want truecolor", got)
	}
	if got := envLookup(env, "TERM_PROGRAM"); got != "iTerm.app" {
		t.Fatalf("TERM_PROGRAM = %q, want the client's emulator", got)
	}
	count := 0
	for _, item := range env {
		if envKey(item) == "TERM" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("TERM appears %d times in %q, want the Daemon's value replaced", count, env)
	}
}

// A client must not be able to push arbitrary variables into the Agent.
func TestTerminalEnvIsSanitized(t *testing.T) {
	ctx := WithTerminalEnv(context.Background(), map[string]string{
		"TERM":                 "xterm\nLD_PRELOAD=/tmp/evil",
		"COLORTERM":            strings.Repeat("x", 128),
		"PATH":                 "/tmp/evil",
		"TERM_PROGRAM":         "iTerm.app",
		"TERM_PROGRAM_VERSION": "3.6.9",
	})
	env := agentEnv(ctx, []string{"PATH=/usr/bin"})
	if got := envLookup(env, "TERM"); got != defaultTerminal {
		t.Fatalf("TERM = %q, want the default after a rejected value", got)
	}
	if got := envLookup(env, "COLORTERM"); got != "" {
		t.Fatalf("COLORTERM = %q, want an over-long value rejected", got)
	}
	if got := envLookup(env, "PATH"); got != "/usr/bin" {
		t.Fatalf("PATH = %q, want it untouched", got)
	}
	// A rejected key must not suppress the ones that are fine.
	if got := envLookup(env, "TERM_PROGRAM"); got != "iTerm.app" {
		t.Fatalf("TERM_PROGRAM = %q, want it kept", got)
	}
}

// The Daemon may have been started from a terminal itself (make start); that
// value is a better default than the fallback.
func TestAgentEnvKeepsAnInheritedTerminal(t *testing.T) {
	env := agentEnv(context.Background(), []string{"TERM=screen-256color"})
	if got := envLookup(env, "TERM"); got != "screen-256color" {
		t.Fatalf("TERM = %q, want the Daemon's own value preserved", got)
	}
}

func envLookup(env []string, key string) string {
	value, _ := envValue(env, key)
	return value
}
