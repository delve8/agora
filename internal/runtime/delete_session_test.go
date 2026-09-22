package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/session"
)

// Claude keeps subagent transcripts and tool results in a directory named after
// the transcript. Deleting only the .jsonl would leave the bulk of the session
// on disk, so the directory has to go too.
func TestDeleteProviderHistoryRemovesClaudeSideData(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "claude-1.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sideData := filepath.Join(dir, "claude-1", "subagents")
	if err := os.MkdirAll(sideData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sideData, "agent.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteProviderHistory(session.Session{Agent: "claude", HistoryPath: transcript}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-1")); !os.IsNotExist(err) {
		t.Fatalf("claude side-data directory still exists: %v", err)
	}
}

// Deleting a history session removes its files and its stored row. A missing
// file is not an error: the transcript may already have been removed.
func TestDeleteSessionRemovesHistoryRowAndFile(t *testing.T) {
	store := NewMemoryStore()
	manager := NewPiProviderRuntime(store, "daemon-1", PiConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	transcript := filepath.Join(dir, "pi-1.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := session.Session{
		ID: "daemon/daemon-1/pi://native-1", DaemonID: "daemon-1", Agent: "pi",
		AgentSessionID: "pi://native-1", Workspace: dir, HistoryPath: transcript,
		DisplayName: "Pi session", Source: session.SourceHistory, State: session.StateStopped,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.CreateSession(ctx, value); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSession(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, value.ID); err == nil {
		t.Fatal("stored session survived deletion")
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists: %v", err)
	}
}
