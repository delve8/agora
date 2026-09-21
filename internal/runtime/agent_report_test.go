package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/sessionhost"
)

// newReporterTestManager builds a manager with one hosted Pi session running a
// stand-in agent, mirroring what the Daemon does at creation time: create with a
// provisional id, adopt the agent identity, then rekey to the canonical id.
func newReporterTestManager(t *testing.T) (*Manager, *sessionhost.Client, string, string) {
	t.Helper()
	home := t.TempDir()
	workspace := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(home, "fake-pi")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nwhile IFS= read -r line; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewDaemonManager(NewMemoryStore(), "daemon-1", adapter.NewClaudeCodeAdapter(""), NewClaudeProvider("", home))
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(NewPiProvider(PiConfig{Binary: agent, SessionDir: sessionDir}))

	ctx := context.Background()
	provisional := "pending/session-reporter-test"
	created, err := manager.CreateManagedSessionWithAgentArgs(ctx, provisional, "coord-1", workspace, "New session", "terminal", "pi", nil)
	if err != nil {
		t.Fatalf("create hosted session: %v", err)
	}
	nativeID := strings.TrimPrefix(created.AgentSessionID, "pi://")
	if _, err := manager.SetAgentIdentity(ctx, provisional, "pi", "pi://"+nativeID); err != nil {
		t.Fatalf("set agent identity: %v", err)
	}
	canonical, err := session.NewSessionID("daemon-1", "pi", "pi://"+nativeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RekeySession(ctx, provisional, canonical); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	client, ok := manager.hosts.Get(canonical)
	if !ok {
		t.Fatalf("host not registered under %s", canonical)
	}
	stored, err := manager.store.GetSession(ctx, canonical)
	if err != nil {
		t.Fatalf("session %s missing from the store after rekey: %v", canonical, err)
	}
	if stored.Agent != "pi" {
		t.Fatalf("stored session agent = %q, want pi", stored.Agent)
	}
	registered, err := client.State(ctx)
	if err != nil {
		t.Fatalf("host state: %v", err)
	}
	if registered.HostID == "" {
		t.Fatal("host metadata has no HostID; reports cannot be attributed")
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})
	return manager, client, workspace, sessionDir
}

func TestReportAgentSessionRebindsToReportedTranscript(t *testing.T) {
	manager, client, workspace, sessionDir := newReporterTestManager(t)
	ctx := context.Background()

	const picked = "b1aa0f72-f174-4ac9-a0c5-6da7fc53bf78"
	transcript := filepath.Join(sessionDir, "2026-01-01T00-00-00-000Z_"+picked+".jsonl")
	header := `{"type":"session","id":"` + picked + `","cwd":` + quoteJSON(workspace) + `}` + "\n"
	if err := os.WriteFile(transcript, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}

	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := metadata.SessionID

	value, err := manager.ReportAgentSession(ctx, AgentSessionReport{
		HostID:          metadata.HostID,
		Reason:          "startup",
		SessionFile:     transcript,
		NativeSessionID: picked,
		SessionName:     "picked session",
	})
	if err != nil {
		t.Fatalf("ReportAgentSession: %v", err)
	}

	want, err := session.NewSessionID("daemon-1", "pi", "pi://"+picked)
	if err != nil {
		t.Fatal(err)
	}
	if value.ID != want {
		t.Fatalf("reported session id = %q, want %q", value.ID, want)
	}
	if value.HistoryPath != transcript {
		t.Fatalf("history path = %q, want %q", value.HistoryPath, transcript)
	}
	if value.DisplayName != "picked session" {
		t.Fatalf("display name = %q, want the reported name", value.DisplayName)
	}
	if _, err := manager.store.GetSession(ctx, before); err == nil {
		t.Fatalf("old session %s still exists after the report", before)
	}
	if _, ok := manager.hosts.Get(want); !ok {
		t.Fatalf("host was not moved to %s", want)
	}
	// The Host metadata is the Daemon-visible binding and must follow too.
	updated, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionID != want || updated.HistoryPath != transcript {
		t.Fatalf("host metadata not updated: session=%q history=%q", updated.SessionID, updated.HistoryPath)
	}
	// The observer starts at the end of existing history: the user already has
	// that transcript, and replaying it would emit it as live events.
	cursor, err := manager.store.GetObservationCursor(ctx, want)
	if err != nil {
		t.Fatalf("observation cursor: %v", err)
	}
	info, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ByteOffset != info.Size() {
		t.Fatalf("cursor = %d, want %d (end of the reported transcript)", cursor.ByteOffset, info.Size())
	}

	// Reporting the same session again is a no-op, so a provider that emits both
	// session_before_switch and session_start does not rebind twice.
	again, err := manager.ReportAgentSession(ctx, AgentSessionReport{
		HostID:          metadata.HostID,
		Reason:          "resume",
		SessionFile:     transcript,
		NativeSessionID: picked,
	})
	if err != nil {
		t.Fatalf("second ReportAgentSession: %v", err)
	}
	if again.ID != want {
		t.Fatalf("second report changed the session: %q", again.ID)
	}
}

// /resume commonly targets a session Agora already listed as history. The
// canonical id therefore already exists, and rebind must replace that row
// instead of leaving the live process on the pre-resume identity.
func TestReportAgentSessionRebindsOntoExistingHistoryRow(t *testing.T) {
	manager, client, workspace, sessionDir := newReporterTestManager(t)
	ctx := context.Background()

	const picked = "c2bb1f83-f285-5bda-b1d6-6eb8fd64c089"
	transcript := filepath.Join(sessionDir, "2026-01-02T00-00-00-000Z_"+picked+".jsonl")
	header := `{"type":"session","id":"` + picked + `","cwd":` + quoteJSON(workspace) + `}` + "\n"
	if err := os.WriteFile(transcript, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := session.NewSessionID("daemon-1", "pi", "pi://"+picked)
	if err != nil {
		t.Fatal(err)
	}
	history := session.Session{
		ID: want, CoordinationID: "coord-1", DaemonID: "daemon-1", Agent: "pi",
		AgentSessionID: "pi://" + picked, Workspace: workspace, DisplayName: "older conversation",
		Role: "history", State: session.StateStopped, Source: session.SourceHistory,
		HistoryPath: transcript, CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}
	if err := manager.store.CreateSession(ctx, history); err != nil {
		t.Fatal(err)
	}

	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := metadata.SessionID
	value, err := manager.ReportAgentSession(ctx, AgentSessionReport{
		HostID:          metadata.HostID,
		Reason:          "resume",
		SessionFile:     transcript,
		NativeSessionID: picked,
		SessionName:     "older conversation",
	})
	if err != nil {
		t.Fatalf("ReportAgentSession onto an existing history row: %v", err)
	}
	if value.ID != want {
		t.Fatalf("rebound session id = %q, want %q", value.ID, want)
	}
	if value.State != session.StateRunning || !value.Capabilities.CanSendInput {
		t.Fatalf("rebound session is not live: %+v", value)
	}
	if value.DisplayName != "older conversation" {
		t.Fatalf("display name = %q, want the history row's name", value.DisplayName)
	}
	if _, err := manager.store.GetSession(ctx, before); err == nil {
		t.Fatalf("old session %s still exists after rebinding onto history", before)
	}
	if _, ok := manager.hosts.Get(want); !ok {
		t.Fatalf("host was not moved onto %s", want)
	}
}

// A provider can move to a session whose transcript does not exist yet (/new).
// The report still carries the exact session id, so Agora binds it immediately
// and lets the observer fill in the transcript when it appears.
func TestReportAgentSessionBindsFreshSessionWithoutTranscript(t *testing.T) {
	manager, client, _, _ := newReporterTestManager(t)
	ctx := context.Background()
	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const fresh = "0f9c5a11-2222-4333-8444-555566667777"

	value, err := manager.ReportAgentSession(ctx, AgentSessionReport{
		HostID:          metadata.HostID,
		Reason:          "new",
		NativeSessionID: fresh,
	})
	if err != nil {
		t.Fatalf("ReportAgentSession: %v", err)
	}
	want, err := session.NewSessionID("daemon-1", "pi", "pi://"+fresh)
	if err != nil {
		t.Fatal(err)
	}
	if value.ID != want || value.HistoryPath != "" {
		t.Fatalf("fresh session binding = %q history=%q, want %q with no history", value.ID, value.HistoryPath, want)
	}
	updated, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionID != want || updated.HistoryPath != "" {
		t.Fatalf("host metadata not updated: session=%q history=%q", updated.SessionID, updated.HistoryPath)
	}
}

// /new reports the future JSONL path before Pi creates the file. Agora must
// bind the native id immediately, but must not advertise a transcript that
// cannot be opened yet.
func TestReportAgentSessionIgnoresMissingTranscriptPath(t *testing.T) {
	manager, client, _, sessionDir := newReporterTestManager(t)
	ctx := context.Background()
	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const fresh = "81fe4903-f2e1-41e9-970f-75d63d17053a"
	missing := filepath.Join(sessionDir, "2026-09-12T09-31-51-937Z_"+fresh+".jsonl")

	value, err := manager.ReportAgentSession(ctx, AgentSessionReport{
		HostID:          metadata.HostID,
		Reason:          "new",
		SessionFile:     missing,
		NativeSessionID: fresh,
	})
	if err != nil {
		t.Fatalf("ReportAgentSession: %v", err)
	}
	if value.HistoryPath != "" {
		t.Fatalf("missing transcript was bound as history: %q", value.HistoryPath)
	}
	if value.Capabilities.CanReadHistory {
		t.Fatalf("a missing transcript advertised history: %+v", value.Capabilities)
	}
	events, err := manager.PiHistoryForSession(ctx, value, 0)
	if err != nil {
		t.Fatalf("history request for a missing transcript: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("missing transcript returned %d events", len(events))
	}
}

// A report naming a Host the Daemon does not manage must not rebind anything.
func TestReportAgentSessionIgnoresUnknownHost(t *testing.T) {
	manager, _, _, _ := newReporterTestManager(t)
	if _, err := manager.ReportAgentSession(context.Background(), AgentSessionReport{
		HostID:          "host-unknown",
		NativeSessionID: "whatever",
	}); err == nil {
		t.Fatal("a report for an unknown host was accepted")
	}
}

func quoteJSON(value string) string {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(body)
}

// A managed session exists before its provider writes anything, and a context
// switch can point at a transcript that is not on disk yet. Claiming history
// there shows the user an empty conversation that looks like lost history, so
// the capability must follow the transcript.
func TestManagedSessionAdvertisesHistoryOnlyWithATranscript(t *testing.T) {
	manager, _, _, sessionDir := newReporterTestManager(t)
	ctx := context.Background()
	id, ok := manager.sessionIDForHost(func() string {
		for _, client := range manager.hosts.Clients() {
			return client.Metadata().HostID
		}
		return ""
	}())
	if !ok {
		t.Fatal("managed session not found for its host")
	}
	value, err := manager.store.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if value.HistoryPath != "" {
		t.Fatalf("test session unexpectedly has history: %q", value.HistoryPath)
	}
	live, err := manager.LiveSessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var listed session.Session
	for _, candidate := range live {
		if candidate.ID == id {
			listed = candidate
		}
	}
	if listed.ID == "" {
		t.Fatalf("session %s is missing from the live list", id)
	}
	if listed.Capabilities.CanReadHistory {
		t.Fatalf("a session without a transcript advertised history: %+v", listed.Capabilities)
	}

	// Once the provider persists the transcript the observer resolves it and the
	// capability turns on.
	native := strings.TrimPrefix(value.NativeSessionURI(), "pi://")
	transcript := filepath.Join(sessionDir, "2026-01-01T00-00-00-000Z_"+native+".jsonl")
	header := `{"type":"session","id":"` + native + `","cwd":` + quoteJSON(value.Workspace) + `}` + "\n"
	record := `{"type":"message","id":"u1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(header+record), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		live, err := manager.LiveSessions(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range live {
			if candidate.ID == id && candidate.Capabilities.CanReadHistory {
				if candidate.HistoryPath != transcript {
					t.Fatalf("history path = %q, want %q", candidate.HistoryPath, transcript)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("history capability never turned on after the transcript appeared")
}
