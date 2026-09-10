package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/session"
)

func TestRekeySessionPreservesCursorAndNativeIdentity(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	if err := db.CreateCoordination(context.Background(), coordination.Coordination{ID: "coord-1", Name: "test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldID := "daemon/d/pi://old"
	value := session.Session{
		ID: oldID, CoordinationID: "coord-1", DaemonID: "d", Agent: "pi",
		AgentSessionID: "pi://old", Workspace: t.TempDir(), DisplayName: "Pi",
		Role: "terminal", State: session.StateRunning, Source: session.SourceManaged,
		Connection: session.ConnectionObserved, Capabilities: session.Capabilities{CanResume: true},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateSession(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveObservationCursor(context.Background(), ObservationCursor{SessionID: oldID, Path: "/tmp/old.jsonl", ByteOffset: 42, Line: 3, LastID: "evt-3"}); err != nil {
		t.Fatal(err)
	}

	newID := "daemon/d/pi://new"
	value.ID = newID
	value.AgentSessionID = newID[len("daemon/d/"):]
	value.HistoryPath = "/tmp/new.jsonl"
	if err := db.RekeySession(context.Background(), oldID, value); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(context.Background(), oldID); err == nil {
		t.Fatal("old session still exists")
	}
	stored, err := db.GetSession(context.Background(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AgentSessionID != "pi://new" || stored.HistoryPath != "/tmp/new.jsonl" {
		t.Fatalf("stored rekeyed session = %+v", stored)
	}
	cursor, err := db.GetObservationCursor(context.Background(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Path != "/tmp/old.jsonl" || cursor.ByteOffset != 42 || cursor.LastID != "evt-3" {
		t.Fatalf("rekeyed cursor = %+v", cursor)
	}
}

// A rebind target id can already exist: older Agora versions persisted rows that
// used the same canonical id, and a previously discovered history row may share
// it too. The rebind must replace that row rather than fail on a primary key
// conflict, otherwise the persisted session keeps the pre-rebind identity while
// the live binding has moved on.
func TestRekeySessionReplacesExistingTargetRow(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	if err := db.CreateCoordination(context.Background(), coordination.Coordination{ID: "coord-1", Name: "test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldID := "daemon/d/pi://fresh"
	newID := "daemon/d/pi://picked"
	live := session.Session{
		ID: oldID, CoordinationID: "coord-1", DaemonID: "d", Agent: "pi",
		AgentSessionID: "pi://fresh", Workspace: t.TempDir(), DisplayName: "Pi",
		Role: "terminal", State: session.StateRunning, Source: session.SourceManaged,
		Connection: session.ConnectionObserved, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateSession(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	// A legacy row for the target id, as left behind by an older version.
	legacy := live
	legacy.ID = newID
	legacy.AgentSessionID = "pi://picked"
	legacy.ProcessID = 4242
	legacy.DisplayName = "legacy"
	legacy.CreatedAt = now.Add(-24 * time.Hour)
	legacy.UpdatedAt = now.Add(-24 * time.Hour)
	if err := db.CreateSession(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveObservationCursor(context.Background(), ObservationCursor{SessionID: oldID, Path: "/tmp/picked.jsonl", ByteOffset: 7}); err != nil {
		t.Fatal(err)
	}

	value := live
	value.ID = newID
	value.AgentSessionID = "pi://picked"
	value.HistoryPath = "/tmp/picked.jsonl"
	if err := db.RekeySession(context.Background(), oldID, value); err != nil {
		t.Fatalf("rekey onto an existing row: %v", err)
	}
	if _, err := db.GetSession(context.Background(), oldID); err == nil {
		t.Fatal("old session still exists after the rekey")
	}
	stored, err := db.GetSession(context.Background(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AgentSessionID != "pi://picked" || stored.HistoryPath != "/tmp/picked.jsonl" {
		t.Fatalf("stored session = %+v, want the rebound values", stored)
	}
	if stored.ProcessID != live.ProcessID {
		t.Fatalf("stored process id = %d, want the live %d", stored.ProcessID, live.ProcessID)
	}
	cursor, err := db.GetObservationCursor(context.Background(), newID)
	if err != nil {
		t.Fatalf("cursor after rekey onto an existing row: %v", err)
	}
	if cursor.Path != "/tmp/picked.jsonl" || cursor.ByteOffset != 7 {
		t.Fatalf("cursor = %+v, want the moved cursor", cursor)
	}
}
