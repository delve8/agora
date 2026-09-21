package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

// Resuming a history conversation starts it again under a Session Host, keeps
// the canonical Agora id, and reports the exit through the host monitor.
func TestManagerResumeUsesSameSessionAndPersistsExit(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(project, "claude-1.jsonl")
	if err := os.WriteFile(historyPath, []byte("{\"type\":\"user\",\"uuid\":\"u1\",\"sessionId\":\"claude-1\",\"cwd\":"+quoteJSON(workspace)+",\"message\":{\"content\":\"hello\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args.txt")
	binary := filepath.Join(home, "claude-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nexec sleep 30\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(home, "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: time.Now().UTC()}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(db, adapter.NewClaudeCodeAdapter(binary), NewClaudeProvider(binary, home))
	manager.EnableSessionHosts(os.Args[0])
	defer manager.Close()

	histories, err := manager.HistorySessions(context.Background(), coord.ID, "local")
	if err != nil || len(histories) != 1 {
		t.Fatalf("unexpected histories: %+v, %v", histories, err)
	}
	original := histories[0]
	resumed, err := manager.ResumeSession(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	stopHost(t, manager, resumed.ID)
	if resumed.ID != original.ID || !resumed.Capabilities.CanSendInput || !manager.IsRunning(original.ID) {
		t.Fatalf("unexpected resumed session: %+v", resumed)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(argsPath)
		if readErr == nil && strings.TrimSpace(string(data)) == "--resume\nclaude-1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil || strings.TrimSpace(string(data)) != "--resume\nclaude-1" {
		t.Fatalf("unexpected resume arguments %q: %v", data, err)
	}

	// Stopping the Agent through its Host is what makes the exit visible.
	if err := manager.StopSession(resumed); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		stored, getErr := db.GetSession(context.Background(), original.ID)
		if getErr == nil && stored.State == session.StateStopped && stored.ProcessID == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	stored, _ := db.GetSession(context.Background(), original.ID)
	t.Fatalf("exit state was not persisted: %+v", stored)
}

// A manager without Session Hosts cannot own an Agent process at all, and says
// so instead of pretending to start one.
func TestManagerWithoutSessionHostsRefusesToCreate(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	manager := NewManager(NewMemoryStore(), adapter.NewClaudeCodeAdapter(""), NewClaudeProvider("claude", home))
	if _, err := manager.CreateManagedSessionWithAgentArgs(context.Background(), "sess-1", "coord-1", workspace, "New session", "terminal", "claude", nil); err == nil || !strings.Contains(err.Error(), "session hosts") {
		t.Fatalf("create without hosts = %v, want a clear refusal", err)
	}
	if _, err := manager.ResumeSession(context.Background(), session.Session{ID: "sess-1", Agent: "pi", Workspace: workspace, AgentSessionID: "pi://native"}); err == nil || !strings.Contains(err.Error(), "session hosts") {
		t.Fatalf("resume without hosts = %v, want a clear refusal", err)
	}
}

// stopHost stops the Session Host that owns id, without waiting for the test
// cleanup to notice a leftover Agent process.
func stopHost(t *testing.T, manager *Manager, id string) {
	t.Helper()
	client, ok := manager.hosts.Get(id)
	if !ok {
		return
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})
}

// The provider's argv is Agora's only contract with Claude: a fresh session gets
// an id, a resume gets --resume.
func TestClaudeProviderCommands(t *testing.T) {
	provider := NewClaudeProvider("/usr/bin/claude", t.TempDir())
	if got := strings.Join(provider.Command("claude-1"), " "); got != "/usr/bin/claude --resume claude-1" {
		t.Fatalf("resume command = %q", got)
	}
	if got := strings.Join(provider.FreshCommand("claude-2"), " "); got != "/usr/bin/claude --session-id claude-2" {
		t.Fatalf("fresh command = %q", got)
	}
	if got := strings.Join(provider.Command(""), " "); got != "/usr/bin/claude" {
		t.Fatalf("empty resume command = %q", got)
	}
}
