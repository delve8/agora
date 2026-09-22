package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/auth"
	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/session"
)

func TestMigrationDropsLegacyEventsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agora.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TABLE events (id TEXT PRIMARY KEY, content TEXT NOT NULL); INSERT INTO events(id,content) VALUES('evt-1','secret transcript')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	err = db.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&name)
	if err != sql.ErrNoRows {
		t.Fatalf("events table still exists: name=%q err=%v", name, err)
	}
}

func TestListSessionsDoesNotHoldRowsWhileLoadingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agora.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	value := session.Session{ID: "sess-1", CoordinationID: coord.ID, Agent: "claude-code", ExternalID: "external-1", Workspace: t.TempDir(), DisplayName: "Claude", State: session.StateCreated, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateSession(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	values, err := db.ListSessions(context.Background(), coord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != value.ID {
		t.Fatalf("unexpected sessions: %+v", values)
	}
}

func TestListObservedSessionsDoesNotHoldRowsWhileLoadingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agora.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		value := session.Session{ID: fmt.Sprintf("sess-%d", i), CoordinationID: coord.ID, Agent: "claude-code", ExternalID: fmt.Sprintf("external-%d", i), Workspace: t.TempDir(), DisplayName: "Claude", Source: session.SourceExternal, State: session.StateWaiting, CreatedAt: now, UpdatedAt: now}
		if err := db.CreateSession(context.Background(), value); err != nil {
			t.Fatal(err)
		}
	}
	// With a single DB connection this would previously deadlock: the id scan
	// held the only connection while GetSession waited for the same one.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	values, err := db.ListObservedSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 observed sessions, got %d", len(values))
	}
}

func TestSessionPreferencesRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	user, err := db.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}

	key := "pi://native-1"
	if _, err := db.GetSessionPreference(ctx, user.UserID, key); err != sql.ErrNoRows {
		t.Fatalf("missing preference error = %v, want sql.ErrNoRows", err)
	}
	if err := db.UpsertSessionPreference(ctx, SessionPreference{UserID: user.UserID, SessionKey: key, DisplayName: "我的会话", Starred: true}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetSessionPreference(ctx, user.UserID, key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DisplayName != "我的会话" || !stored.Starred {
		t.Fatalf("stored preference = %+v", stored)
	}

	// A second upsert must update in place, not duplicate the row.
	if err := db.UpsertSessionPreference(ctx, SessionPreference{UserID: user.UserID, SessionKey: key, DisplayName: "", Starred: true}); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListSessionPreferences(ctx, user.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].DisplayName != "" || !listed[0].Starred {
		t.Fatalf("preferences = %+v", listed)
	}

	// A canonical-id key follows a rekey.
	if err := db.UpsertSessionPreference(ctx, SessionPreference{UserID: user.UserID, SessionKey: "daemon/d/pi://old", Starred: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameSessionPreferenceKey(ctx, "daemon/d/pi://old", "daemon/d/pi://new"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSessionPreference(ctx, user.UserID, "daemon/d/pi://new"); err != nil {
		t.Fatalf("rekeyed preference not found: %v", err)
	}
	if _, err := db.GetSessionPreference(ctx, user.UserID, "daemon/d/pi://old"); err != sql.ErrNoRows {
		t.Fatalf("old preference key survived rekey: %v", err)
	}

	if err := db.DeleteSessionPreference(ctx, user.UserID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSessionPreference(ctx, user.UserID, key); err != sql.ErrNoRows {
		t.Fatalf("preference survived delete: %v", err)
	}
}

func TestDeleteSessionPreferenceByKeyRemovesEveryUser(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	local, err := db.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.FindByClaims(ctx, auth.ProvisionClaims{Provider: auth.ProviderLogto, Subject: "other"})
	if err != nil {
		t.Fatal(err)
	}
	key := "pi://shared"
	for _, userID := range []string{local.UserID, other.UserID} {
		if err := db.UpsertSessionPreference(ctx, SessionPreference{UserID: userID, SessionKey: key, DisplayName: "别名", Starred: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteSessionPreferenceByKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []string{local.UserID, other.UserID} {
		if _, err := db.GetSessionPreference(ctx, userID, key); err != sql.ErrNoRows {
			t.Fatalf("preference for %s survived global delete: %v", userID, err)
		}
	}
	// Deleting an unknown or empty key is a no-op, not an error.
	if err := db.DeleteSessionPreferenceByKey(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

// A resumed or newly started session advertises different capabilities than the
// history row it came from. They only ever reach the database through
// UpdateSessionObservation, so that update has to carry them; otherwise the Web
// UI keeps offering a history row's (or a starting row's) capabilities.
func TestUpdateSessionObservationPersistsCapabilities(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateCoordination(ctx, coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	value := session.Session{
		ID: "daemon/local/claude://native-1", CoordinationID: "coord-1", Agent: "claude",
		Workspace: t.TempDir(), State: session.StateStopped, Source: session.SourceHistory,
		Capabilities: session.Capabilities{CanReadHistory: true, CanResume: true},
	}
	if err := db.CreateSession(ctx, value); err != nil {
		t.Fatal(err)
	}
	value.State = session.StateRunning
	value.Source = session.SourceManaged
	value.ProcessID = 42
	value.Capabilities = session.Capabilities{CanStart: true, CanSendInput: true, CanInterrupt: true}
	if err := db.UpdateSessionObservation(ctx, value); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetSession(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Capabilities.CanSendInput || stored.Capabilities.CanReadHistory {
		t.Fatalf("capabilities = %+v, want the updated set", stored.Capabilities)
	}
	if stored.State != session.StateRunning || stored.ProcessID != 42 {
		t.Fatalf("stored session = %+v, want the observed state", stored)
	}
}
