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
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
	"github.com/delve8/agora/internal/terminal"
)

func TestPTYManagerReportsProcessExit(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewPTYManager(binary, dir)
	defer manager.Close()
	exits := make(chan PTYExit, 1)
	manager.SetExitHandler(func(value PTYExit) { exits <- value })
	if _, err := manager.Launch("sess-1", dir, "claude-1"); err != nil {
		t.Fatal(err)
	}
	if !manager.IsRunning("sess-1") {
		t.Fatal("expected session to be running")
	}
	select {
	case value := <-exits:
		if value.AgoraID != "sess-1" || value.ClaudeSession != "claude-1" || value.ExitCode != 0 {
			t.Fatalf("unexpected exit: %+v", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process exit was not reported")
	}
	if manager.IsRunning("sess-1") {
		t.Fatal("exited session still reported running")
	}
}

func TestPTYManagerStopReportsExit(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewPTYManager(binary, dir)
	defer manager.Close()
	exits := make(chan PTYExit, 1)
	manager.SetExitHandler(func(value PTYExit) { exits <- value })
	if _, err := manager.Launch("sess-stop", dir, "claude-stop"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop("sess-stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-exits:
		if value.AgoraID != "sess-stop" {
			t.Fatalf("unexpected exit: %+v", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopped process did not exit")
	}
	if manager.IsRunning("sess-stop") {
		t.Fatal("stopped session still reported running")
	}
}

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
	if err := os.WriteFile(historyPath, []byte("{\"type\":\"user\",\"uuid\":\"u1\",\"sessionId\":\"claude-1\",\"cwd\":"+quote(workspace)+",\"message\":{\"content\":\"hello\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args.txt")
	binary := filepath.Join(home, "claude-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nsleep 2\n"
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
	manager := NewManager(db, adapter.NewClaudeCodeAdapter(binary), NewPTYManager(binary, home))
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
	if resumed.ID != original.ID || !resumed.Capabilities.CanSendInput || !manager.IsRunning(original.ID) {
		t.Fatalf("unexpected resumed session: %+v", resumed)
	}
	deadline := time.Now().Add(3 * time.Second)
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
	deadline = time.Now().Add(3 * time.Second)
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

func TestManagerResumeUsesMemoryStoreForHistorySession(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args.txt")
	binary := filepath.Join(home, "claude-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nsleep 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(NewMemoryStore(), adapter.NewClaudeCodeAdapter(binary), NewPTYManager(binary, home))
	defer manager.Close()
	original := session.Session{
		ID:              "daemon/daemon-1/claude://claude-1",
		DaemonID:        "daemon-1",
		Agent:           "claude",
		AgentSessionID:  "claude://claude-1",
		ClaudeSessionID: "claude-1",
		Workspace:       workspace,
		DisplayName:     "History session",
		State:           session.StateStopped,
		Source:          session.SourceHistory,
		Capabilities:    session.Capabilities{CanReadHistory: true, CanResume: true},
	}
	resumed, err := manager.ResumeSession(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != original.ID || resumed.Source != session.SourceManaged || !resumed.Capabilities.CanSendInput || !manager.IsRunning(original.ID) {
		t.Fatalf("unexpected resumed session: %+v", resumed)
	}

	deadline := time.Now().Add(3 * time.Second)
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
	stored, err := manager.GetSession(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Capabilities.CanSendInput {
		t.Fatalf("stored resumed session cannot accept input: %+v", stored.Capabilities)
	}
	if err := manager.Send(context.Background(), stored, message.Message{ID: "msg-1", Content: "hello"}); err != nil {
		t.Fatalf("send after resume failed: %v", err)
	}
	stored, err = manager.GetSession(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClaudeSessionID != original.ClaudeSessionID || stored.Workspace != workspace {
		t.Fatalf("resume metadata was not preserved: %+v", stored)
	}
}

func TestObserverUsesMemoryStoreCursorForResumedSession(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(project, "claude-2.jsonl")
	if err := os.WriteFile(historyPath, []byte("{\"type\":\"user\",\"uuid\":\"u1\",\"sessionId\":\"claude-2\",\"cwd\":"+quote(workspace)+",\"message\":{\"content\":\"old\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args.txt")
	binary := filepath.Join(home, "claude-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nsleep 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(NewMemoryStore(), adapter.NewClaudeCodeAdapter(binary), NewPTYManager(binary, home))
	defer manager.Close()
	original := session.Session{
		ID:              "daemon/daemon-2/claude://claude-2",
		DaemonID:        "daemon-2",
		Agent:           "claude",
		AgentSessionID:  "claude://claude-2",
		ClaudeSessionID: "claude-2",
		Workspace:       workspace,
		DisplayName:     "History session",
		State:           session.StateStopped,
		Source:          session.SourceHistory,
		Capabilities:    session.Capabilities{CanReadHistory: true, CanResume: true},
	}
	resumed, err := manager.ResumeSession(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}

	ch, unsubscribe := manager.Subscribe(resumed.CoordinationID)
	defer unsubscribe()
	file, err := os.OpenFile(historyPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"type\":\"user\",\"uuid\":\"u2\",\"sessionId\":\"claude-2\",\"cwd\":" + quote(workspace) + ",\"message\":{\"content\":\"new\"}}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case item, ok := <-ch:
			if !ok {
				t.Fatal("subscription channel was closed")
			}
			if item.SessionID == resumed.ID && item.Content == "new" {
				stored, storedErr := manager.GetSession(context.Background(), resumed.ID)
				if storedErr != nil {
					t.Fatal(storedErr)
				}
				if stored.State == session.StateStopped || stored.Connection == session.ConnectionUnavailable {
					t.Fatalf("observation state was not updated: %+v", stored)
				}
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("observer did not publish appended JSONL event")
}

func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func TestPTYObservationRecordsBoundedFramesAndSnapshot(t *testing.T) {
	observation := newPTYObservation()
	observation.capacity = 2
	if err := observation.emulator.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	observation.record([]byte("\x1b[2;1Hworld"))
	observation.record([]byte("\x1b[2Jok"))
	observation.record([]byte("!"))

	if got := len(observation.frames); got != 2 {
		t.Fatalf("expected bounded frame history, got %d", got)
	}
	if observation.frames[0].Sequence != 2 || observation.frames[1].Sequence != 3 {
		t.Fatalf("unexpected frame sequences: %d, %d", observation.frames[0].Sequence, observation.frames[1].Sequence)
	}
	snapshot := observation.snapshot()
	if snapshot.Sequence != 3 || !snapshot.Healthy {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Lines == nil {
		t.Fatal("expected screen lines")
	}
}

func TestPTYObservationSnapshotTypeIsStable(t *testing.T) {
	var _ terminal.Snapshot = terminal.Snapshot{}
}
