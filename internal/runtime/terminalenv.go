package runtime

import (
	"context"
	"strings"
)

// terminalEnvKeys are the client terminal variables a wrapper may forward. The
// set is deliberately small: these are the ones a managed Agent uses to decide
// how much colour it may emit and which emulator it is talking to, and the
// Daemon has no terminal of its own to supply them.
var terminalEnvKeys = []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION"}

// defaultTerminal is what an Agent gets when nothing else is known. A Daemon
// started as a service has no TERM at all, and an Agent that sees none falls
// back to 16 colours and cannot recognise the emulator it runs under.
const defaultTerminal = "xterm-256color"

type terminalEnvContextKey struct{}

// WithTerminalEnv attaches the requesting client's terminal identity to a create
// request. It is request-scoped metadata that crosses the Daemon/Server boundary
// with the create frame, which is why it travels on the context instead of
// through every create signature.
func WithTerminalEnv(ctx context.Context, terminal map[string]string) context.Context {
	if len(terminal) == 0 {
		return ctx
	}
	clean := sanitizeTerminalEnv(terminal)
	if len(clean) == 0 {
		return ctx
	}
	return context.WithValue(ctx, terminalEnvContextKey{}, clean)
}

// TerminalEnvFrom returns the sanitized terminal identity attached to ctx.
func TerminalEnvFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(terminalEnvContextKey{}).(map[string]string)
	return value
}

// agentEnv returns the environment for a managed Agent: the Daemon's own
// environment (sans provider-specific variables) with a usable terminal, plus
// the terminal identity of the client that asked for the session.
func agentEnv(ctx context.Context, base []string) []string {
	return applyTerminalEnv(withDefaultTerminal(base), TerminalEnvFrom(ctx))
}

// withDefaultTerminal makes sure the environment names a terminal, so an Agent
// spawned by a service does not have to guess. Anything already set wins.
func withDefaultTerminal(base []string) []string {
	if _, ok := envValue(base, "TERM"); ok {
		return base
	}
	return append(append([]string(nil), base...), "TERM="+defaultTerminal)
}

// applyTerminalEnv overrides base with the client's terminal identity. The
// client wins over anything inherited through the Daemon, because the Daemon's
// environment belongs to a service, not to a terminal.
func applyTerminalEnv(base []string, terminal map[string]string) []string {
	if len(terminal) == 0 {
		return base
	}
	env := make([]string, 0, len(base)+len(terminalEnvKeys))
	for _, item := range base {
		if _, override := terminal[envKey(item)]; override {
			continue
		}
		env = append(env, item)
	}
	for _, key := range terminalEnvKeys {
		if value, ok := terminal[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// sanitizeTerminalEnv keeps only known keys with single-line, bounded values, so
// a client cannot inject arbitrary environment variables into an Agent.
func sanitizeTerminalEnv(terminal map[string]string) map[string]string {
	clean := make(map[string]string, len(terminalEnvKeys))
	for _, key := range terminalEnvKeys {
		value := strings.TrimSpace(terminal[key])
		if value == "" || len(value) > 64 {
			continue
		}
		if strings.ContainsAny(value, "\x00\n\r") {
			continue
		}
		clean[key] = value
	}
	return clean
}

func envKey(item string) string {
	if index := strings.IndexByte(item, '='); index >= 0 {
		return item[:index]
	}
	return item
}

func envValue(base []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range base {
		if strings.HasPrefix(item, prefix) {
			return item[len(prefix):], true
		}
	}
	return "", false
}
