package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
)

// A handoff from a Claude session to Pi must produce a real Pi transcript the
// target can resume, seeded with the deterministic dossier rather than the raw
// source conversation.
func TestHandoffSessionWritesTargetTranscript(t *testing.T) {
	ctx := context.Background()
	sourceHome := t.TempDir()
	sourceWorkspace := t.TempDir()
	sourcePath, err := (adapter.ClaudeSessionWriter{HomeDir: sourceHome, Version: "2.1.278"}).WriteSessionFile(
		sourceWorkspace,
		"238a79da-8b26-47a4-9e60-17049067a2bc",
		[]adapter.HandoffMessage{
			{Role: "user", Content: "实现会话管理功能"},
			{Role: "assistant", Content: "已完成会话管理，待办是补测试。"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	source := session.Session{
		ID: "daemon/daemon-1/claude://238a79da-8b26-47a4-9e60-17049067a2bc", DaemonID: "daemon-1",
		Agent: "claude", AgentSessionID: "claude://238a79da-8b26-47a4-9e60-17049067a2bc",
		Workspace: sourceWorkspace, HistoryPath: sourcePath, Source: session.SourceHistory,
	}

	sessionDir := t.TempDir()
	targetWorkspace := t.TempDir()
	manager := NewPiProviderRuntime(NewMemoryStore(), "daemon-2", PiConfig{SessionDir: sessionDir})

	value, err := manager.HandoffSession(ctx, source, "pi", targetWorkspace, "daemon-2", "coord-1")
	if err != nil {
		t.Fatal(err)
	}
	if value.Agent != "pi" || value.DaemonID != "daemon-2" {
		t.Fatalf("handoff session = %+v", value)
	}
	if !strings.HasPrefix(value.AgentSessionID, "pi://") {
		t.Fatalf("agent session id = %q", value.AgentSessionID)
	}
	nativeID := strings.TrimPrefix(value.AgentSessionID, "pi://")
	if found := adapter.FindPiHistoryBySessionID(sessionDir, nativeID); found != value.HistoryPath {
		t.Fatalf("handoff transcript not discoverable: %q", found)
	}
	summary, err := adapter.ReadAllHistory(ctx, value.HistoryPath, value.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) == 0 {
		t.Fatal("handoff transcript has no parsed history")
	}
	joined := ""
	for _, item := range summary {
		joined += item.Content
	}
	if !strings.Contains(joined, "交接档案") || !strings.Contains(joined, "实现会话管理功能") {
		t.Fatalf("handoff transcript is missing the dossier: %q", joined)
	}

	stored, err := manager.store.GetSession(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HistoryPath != value.HistoryPath || stored.DisplayName == "" {
		t.Fatalf("stored handoff = %+v", stored)
	}

	// A provider without a writer must fail loudly instead of writing junk.
	if _, err := manager.HandoffSession(ctx, source, "opencode", targetWorkspace, "daemon-2", "coord-1"); err == nil {
		t.Fatal("expected opencode handoff to be rejected")
	}
}

// The dossier's environment section must not leak a zero timestamp into the
// rendering when the source was discovered with incomplete metadata.
func TestHandoffSessionHandlesSparseSource(t *testing.T) {
	ctx := context.Background()
	sourcePath, err := (adapter.ClaudeSessionWriter{HomeDir: t.TempDir()}).WriteSessionFile(
		t.TempDir(), "11111111-1111-4111-8111-111111111111",
		[]adapter.HandoffMessage{{Role: "user", Content: "只做了一点"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	source := session.Session{ID: "daemon/d/claude://11111111-1111-4111-8111-111111111111", Agent: "claude", HistoryPath: sourcePath, Source: session.SourceHistory, CreatedAt: time.Now().UTC()}
	manager := NewPiProviderRuntime(NewMemoryStore(), "d", PiConfig{SessionDir: t.TempDir()})
	if _, err := manager.HandoffSession(ctx, source, "pi", t.TempDir(), "d", "coord-1"); err != nil {
		t.Fatal(err)
	}
}
