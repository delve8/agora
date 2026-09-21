package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
)

// Attaching paints the screen before streaming, and it asks for that screen over
// the session host's control socket. Snapshots have been served there since the
// first host release, so a host built before attach-time replay still shows what
// it drew; only the Daemon has to be current.
func TestHostedSessionSnapshotIsServedByTheHost(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(home, "fake-pi")
	// Paints once and then stays silent: only a snapshot can show this screen, so
	// the test fails if the host stops answering with its observation.
	script := "#!/bin/sh\nprintf 'PAINTED\\r\\n'\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store := NewMemoryStore()
	manager := NewDaemonManager(store, "daemon-1", adapter.NewClaudeCodeAdapter(""), NewClaudeProvider("", home))
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(NewPiProvider(PiConfig{Binary: agent, SessionDir: sessionDir}))
	defer manager.Close()

	created, err := manager.CreateManagedSessionWithAgentArgs(ctx, "pending/session-snapshot-test", "coord-1", workspace, "New session", "terminal", "pi", nil)
	if err != nil {
		t.Fatalf("create hosted session: %v", err)
	}
	client, ok := manager.hosts.Get(created.ID)
	if !ok {
		t.Fatalf("host not registered under %s", created.ID)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	deadline := time.Now().Add(15 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		snapshot, err := manager.Snapshot(created.ID)
		if err == nil {
			lines = snapshot.Lines
			if strings.Contains(strings.Join(lines, "\n"), "PAINTED") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host never served the screen it painted: %q", lines)
}
