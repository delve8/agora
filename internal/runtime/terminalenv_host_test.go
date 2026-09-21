package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/sessionhost"
)

// A managed Agent is spawned by the Daemon's session host, not by the client, so
// the only way its TERM can describe the user's terminal is for the create
// request to carry it all the way down to the process environment.
func TestHostedAgentGetsTheRequestingTerminal(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	reported := filepath.Join(workspace, "agent-terminal.txt")
	agent := filepath.Join(home, "fake-pi")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$TERM\" \"$COLORTERM\" \"$TERM_PROGRAM\" \"$TERM_PROGRAM_VERSION\" > '" + reported + "'\nexec sleep 30\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewDaemonManager(NewMemoryStore(), "daemon-1", adapter.NewClaudeCodeAdapter(""), NewClaudeProvider("", home))
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(NewPiProvider(PiConfig{Binary: agent, SessionDir: sessionDir}))
	defer manager.Close()

	ctx := WithTerminalEnv(context.Background(), map[string]string{
		"TERM":                 "xterm-256color",
		"COLORTERM":            "truecolor",
		"TERM_PROGRAM":         "iTerm.app",
		"TERM_PROGRAM_VERSION": "3.6.9",
	})
	if _, err := manager.CreateManagedSessionWithAgentArgs(ctx, "pending/session-terminal-env", "coord-1", workspace, "New session", "terminal", "pi", nil); err != nil {
		t.Fatalf("create hosted session: %v", err)
	}
	// The Host owns the Agent and outlives the Manager by design, so the test
	// stops it explicitly instead of leaving a process behind.
	var hosts []*sessionhost.Client
	for _, client := range manager.hosts.Clients() {
		hosts = append(hosts, client)
	}
	t.Cleanup(func() {
		for _, client := range hosts {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = client.Stop(stopCtx)
			cancel()
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(reported)
		if err == nil {
			if got := strings.TrimSpace(string(data)); got != "xterm-256color|truecolor|iTerm.app|3.6.9" {
				t.Fatalf("agent environment = %q, want the requesting terminal", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the agent never reported its environment")
}
