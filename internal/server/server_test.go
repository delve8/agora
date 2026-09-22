package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/auth"
	"github.com/delve8/agora/internal/config"
	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

// sessionAPIPath builds a session API URL the way real clients do. Canonical
// Session IDs contain "://", and an unescaped double slash makes net/http's
// ServeMux clean the path and answer 307 instead of routing the request.
func sessionAPIPath(id, suffix string) string {
	return "/api/sessions/" + url.PathEscape(id) + suffix
}

func seedDaemonLiveSession(srv *Server, daemonID string, summary protocol.SessionSummary) {
	srv.daemons.mu.Lock()
	if srv.daemons.devices[daemonID] == nil {
		srv.daemons.devices[daemonID] = &daemonConnection{id: daemonID}
	}
	if srv.daemons.sessions[daemonID] == nil {
		srv.daemons.sessions[daemonID] = make(map[string]protocol.SessionSummary)
	}
	srv.daemons.sessions[daemonID][summary.SessionID] = summary
	srv.daemons.mu.Unlock()
}

func seedDaemonHistorySession(srv *Server, daemonID string, summary protocol.HistorySessionSummary) {
	srv.daemons.mu.Lock()
	if srv.daemons.devices[daemonID] == nil {
		srv.daemons.devices[daemonID] = &daemonConnection{id: daemonID}
	}
	if srv.daemons.history[daemonID] == nil {
		srv.daemons.history[daemonID] = make(map[string]protocol.HistorySessionSummary)
	}
	srv.daemons.history[daemonID][summary.SessionID] = summary
	srv.daemons.mu.Unlock()
}

func TestWebhookConfigurationSupportsMultipleTargets(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	for _, body := range []string{`{"provider":"generic","label":"one","url":"https://example.test/one?secret=abc"}`, `{"provider":"feishu","label":"two","url":"https://example.test/two"}`} {
		resp := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/webhooks", bytes.NewBufferString(body)))
		if resp.Code != http.StatusCreated {
			t.Fatalf("create webhook returned %d: %s", resp.Code, resp.Body.String())
		}
	}
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/webhooks", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("list webhooks returned %d: %s", resp.Code, resp.Body.String())
	}
	var values []struct {
		ID, URL string
		Enabled bool
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || !values[0].Enabled || strings.Contains(values[0].URL, "secret=abc") {
		t.Fatalf("webhook list = %+v", values)
	}
	// Toggle and delete through the authenticated API using the first target ID.
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	update := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/webhooks/"+listed[0].ID, bytes.NewBufferString(`{"enabled":false}`))
	srv.HTTP.Handler.ServeHTTP(update, req)
	if update.Code != http.StatusOK {
		t.Fatalf("disable webhook returned %d: %s", update.Code, update.Body.String())
	}
	remove := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(remove, httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+listed[0].ID, nil))
	if remove.Code != http.StatusOK {
		t.Fatalf("delete webhook returned %d: %s", remove.Code, remove.Body.String())
	}
}

func TestStateReportsDaemonLiveSessionMetadata(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	// daemon mode: the local manager owns no PTY, so only connected daemons
	// contribute sessions.
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{
		SessionID: "daemon/daemon-1/claude://claude-1", DaemonID: "daemon-1", Agent: "claude",
		AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1",
		Workspace: "/tmp/workspace", DisplayName: "Latest title", DisplayNameSource: session.DisplayNameSourceAITitle,
		State: session.StateWaiting, Connection: session.ConnectionObserved, PID: 42,
		CreatedAt: now, UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %+v", state.Sessions)
	}
	got := state.Sessions[0]
	if got.DisplayName != "Latest title" || got.DisplayNameSource != session.DisplayNameSourceAITitle || got.Workspace != "/tmp/workspace" {
		t.Fatalf("metadata not reported: %+v", got)
	}
	if got.State != session.StateWaiting || got.ProcessID != 42 || !got.Capabilities.CanStream || !got.Capabilities.CanSendInput {
		t.Fatalf("live state not reported: %+v", got)
	}
}

func TestStatePreservesCustomDisplayName(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{
		SessionID: "daemon/daemon-1/claude://claude-1", DaemonID: "daemon-1", Agent: "claude",
		AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1",
		Workspace: "/tmp/workspace", DisplayName: "My custom name", DisplayNameSource: session.DisplayNameSourceCustom,
		State: session.StateWaiting, Connection: session.ConnectionObserved, PID: 42,
		CreatedAt: now, UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].DisplayName != "My custom name" || state.Sessions[0].DisplayNameSource != session.DisplayNameSourceCustom {
		t.Fatalf("custom name was not preserved: %+v", state.Sessions)
	}
}

func TestGetEventsReadsManagedJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-08-15T10:00:00Z\",\"message\":{\"content\":\"from jsonl\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	value := session.Session{ID: "sess-1", CoordinationID: coord.ID, Agent: "claude-code", Workspace: t.TempDir(), DisplayName: "New session", State: session.StateWaiting, Source: session.SourceManaged, HistoryPath: path, Capabilities: session.Capabilities{CanReadHistory: true}, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateSession(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", ""))
	srv := New(":0", db, manager)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/sess-1/events", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var values []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0]["content"] != "from jsonl" {
		t.Fatalf("unexpected events: %+v", values)
	}
}

func TestStateListsEphemeralHistoryWithoutPersisting(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-tmp-history-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "claude-history.jsonl")
	content := "{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-08-15T10:00:00Z\",\"cwd\":\"/tmp/history-workspace\",\"sessionId\":\"claude-history\",\"message\":{\"content\":\"history request\"}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: time.Now().UTC()}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", home))
	srv := New(":0", db, manager)
	stateReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	stateResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(stateResp, stateReq)
	if stateResp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", stateResp.Code, stateResp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(stateResp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].Source != session.SourceHistory || state.Sessions[0].DisplayName != "history request" {
		t.Fatalf("unexpected discovered sessions: %+v", state.Sessions)
	}
	value := state.Sessions[0]
	if !value.Capabilities.CanReadHistory || !value.Capabilities.CanResume || value.Capabilities.CanStream || value.Capabilities.CanSendInput || value.Capabilities.CanReadTerminal {
		t.Fatalf("unexpected history capabilities: %+v", value.Capabilities)
	}
	stored, err := db.ListSessions(context.Background(), coord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("ephemeral history was persisted: %+v", stored)
	}
	eventsReq := httptest.NewRequest(http.MethodGet, sessionAPIPath(value.ID, "/events"), nil)
	eventsResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(eventsResp, eventsReq)
	if eventsResp.Code != http.StatusOK {
		t.Fatalf("expected history 200, got %d: %s", eventsResp.Code, eventsResp.Body.String())
	}
	var events []map[string]any
	if err := json.Unmarshal(eventsResp.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0]["content"] != "history request" {
		t.Fatalf("unexpected history events: %+v", events)
	}
	streamReq := httptest.NewRequest(http.MethodGet, sessionAPIPath(value.ID, "/events/stream"), nil)
	streamResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(streamResp, streamReq)
	if streamResp.Code != http.StatusConflict {
		t.Fatalf("expected stream conflict, got %d: %s", streamResp.Code, streamResp.Body.String())
	}
}

func TestStateDeduplicatesDaemonLiveAndHistory(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{
		SessionID: "daemon/daemon-1/claude://claude-1", DaemonID: "daemon-1", Agent: "claude",
		AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1",
		Workspace: "/tmp/workspace", DisplayName: "Managed", DisplayNameSource: session.DisplayNameSourceCustom,
		State: session.StateWaiting, Connection: session.ConnectionObserved, PID: 42,
		CreatedAt: now, UpdatedAt: now,
	})
	seedDaemonHistorySession(srv, "daemon-1", protocol.HistorySessionSummary{
		SessionID: "daemon/daemon-1/claude://claude-1", DaemonID: "daemon-1", Agent: "claude",
		AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1",
		Workspace: "/tmp/workspace", DisplayName: "History name", DisplayNameSource: session.DisplayNameSourceAITitle,
		CreatedAt: now, UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "daemon/daemon-1/claude://claude-1" {
		t.Fatalf("live and history were duplicated: %+v", state.Sessions)
	}
	if state.Sessions[0].DisplayName != "Managed" {
		t.Fatalf("live metadata was clobbered by history: %+v", state.Sessions[0])
	}
}

func TestStateDeduplicatesLiveAndHistoryByNativeURIWhenIDsDiffer(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-uri", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-uri", protocol.SessionSummary{
		SessionID: "live-id", DaemonID: "daemon-uri", Agent: "pi", AgentSessionID: "pi://native-uri",
		Workspace: "/tmp/pi", DisplayName: "TUI", State: session.StateRunning,
		Connection: session.ConnectionObserved, PID: 42, CreatedAt: now, UpdatedAt: now,
	})
	seedDaemonHistorySession(srv, "daemon-uri", protocol.HistorySessionSummary{
		SessionID: "history-id", DaemonID: "daemon-uri", Agent: "pi", AgentSessionID: "pi://native-uri",
		Workspace: "/tmp/pi", DisplayName: "History", CreatedAt: now, UpdatedAt: now,
	})
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "live-id" {
		t.Fatalf("live and history were duplicated: %+v", state.Sessions)
	}
}

func TestStateDeduplicatesLegacyLiveAndHistoryByWorkspace(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-workspace", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-workspace", protocol.SessionSummary{
		SessionID: "legacy-live", DaemonID: "daemon-workspace", Agent: "pi",
		Workspace: "/tmp/pi-workspace", DisplayName: "TUI", State: session.StateRunning,
		Connection: session.ConnectionObserved, PID: 42, CreatedAt: now, UpdatedAt: now,
	})
	seedDaemonHistorySession(srv, "daemon-workspace", protocol.HistorySessionSummary{
		SessionID: "canonical-history", DaemonID: "daemon-workspace", Agent: "pi", AgentSessionID: "pi://native-workspace",
		Workspace: "/tmp/pi-workspace", DisplayName: "History", CreatedAt: now, UpdatedAt: now,
	})
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "legacy-live" {
		t.Fatalf("legacy live and history were duplicated: %+v", state.Sessions)
	}
}

func TestStateReturnsStoppedDaemonSessionAsResumable(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{
		SessionID: "daemon/daemon-1/claude://claude-1", DaemonID: "daemon-1", Agent: "claude",
		AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1",
		Workspace: "/tmp/workspace", DisplayName: "Custom", DisplayNameSource: session.DisplayNameSourceCustom,
		State: session.StateStopped, Connection: session.ConnectionUnavailable, PID: 0,
		CreatedAt: now, UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("unexpected sessions: %+v", state.Sessions)
	}
	value := state.Sessions[0]
	if value.State != session.StateStopped || value.ProcessID != 0 || !value.Capabilities.CanResume || value.Capabilities.CanStream || value.Capabilities.CanSendInput {
		t.Fatalf("stale daemon session has live capabilities: %+v", value)
	}
}

func TestResumeEphemeralHistoryUsesSameID(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	project := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "claude-1.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"uuid\":\"u1\",\"sessionId\":\"claude-1\",\"cwd\":\""+workspace+"\",\"message\":{\"content\":\"hello\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "claude-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: time.Now().UTC()}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(binary), runtime.NewClaudeProvider(binary, home))
	manager.EnableSessionHosts(os.Args[0])
	defer manager.Close()
	srv := New(":0", db, manager)
	stateReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	stateResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(stateResp, stateReq)
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(stateResp.Body.Bytes(), &state); err != nil || len(state.Sessions) != 1 {
		t.Fatalf("unexpected state: %+v, %v", state, err)
	}
	originalID := state.Sessions[0].ID
	resumeReq := httptest.NewRequest(http.MethodPost, sessionAPIPath(originalID, "/resume"), nil)
	resumeResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resumeResp, resumeReq)
	if resumeResp.Code != http.StatusOK {
		t.Fatalf("expected resume 200, got %d: %s", resumeResp.Code, resumeResp.Body.String())
	}
	var resumed session.Session
	if err := json.Unmarshal(resumeResp.Body.Bytes(), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.ID != originalID || resumed.Source != session.SourceManaged || !resumed.Capabilities.CanSendInput {
		t.Fatalf("unexpected resumed session: %+v", resumed)
	}
	// The Agent belongs to a Session Host, which outlives this test unless it is
	// stopped explicitly.
	t.Cleanup(func() { _ = manager.StopSession(resumed) })
	stored, err := db.ListSessions(context.Background(), coord.ID)
	if err != nil || len(stored) != 1 || stored[0].ID != originalID {
		t.Fatalf("resume created duplicate metadata: %+v, %v", stored, err)
	}
}

func TestPTYSnapshotUnavailable(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", ""))
	srv := New(":0", db, manager)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/missing/pty/snapshot", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
func TestHealthz(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", ""))
	srv := New(":0", db, manager)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func TestFrontendServesAssetsAndSPAFallback(t *testing.T) {
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>Agora</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(webDir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "assets", "app.js"), []byte("console.log('ok')"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := NewWithWebDir(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", "")), webDir)
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/", want: "<html>Agora</html>"},
		{path: "/assets/app.js", want: "console.log('ok')"},
		{path: "/sessions/sess-1", want: "<html>Agora</html>"},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		resp := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK || resp.Body.String() != test.want {
			t.Fatalf("GET %s: status=%d body=%q", test.path, resp.Code, resp.Body.String())
		}
	}
	missing := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/missing.css", nil))
	if missing.Code != http.StatusOK {
		t.Fatalf("expected SPA fallback, got %d", missing.Code)
	}
	missingAsset := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(missingAsset, httptest.NewRequest(http.MethodGet, "/assets/missing.css", nil))
	if missingAsset.Code != http.StatusNotFound {
		t.Fatalf("expected missing asset 404, got %d", missingAsset.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func TestFrontendReturnsNotFoundWhenUnavailable(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := NewWithWebDir(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewClaudeProvider("", "")), filepath.Join(t.TempDir(), "missing"))
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

// Every conversation in a workspace must stay visible: a live session only
// replaces the history row for its own native session, not the other
// conversations that happen to share the directory.
func TestStateListsHistorySessionsBesideLiveSessionInSameWorkspace(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
	if err := db.CreateCoordination(context.Background(), coord); err != nil {
		t.Fatal(err)
	}
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	const workspace = "/tmp/pi-workspace"
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{
		SessionID: "daemon/daemon-1/pi://live-native", DaemonID: "daemon-1", Agent: "pi",
		AgentSessionID: "pi://live-native", HistoryPath: workspace + "/live-native.jsonl",
		Workspace: workspace, DisplayName: "Live session", State: session.StateRunning,
		Connection: session.ConnectionObserved, PID: 42, CreatedAt: now, UpdatedAt: now,
	})
	for _, older := range []struct{ id, name string }{{"older-1", "Older one"}, {"older-2", "Older two"}} {
		seedDaemonHistorySession(srv, "daemon-1", protocol.HistorySessionSummary{
			SessionID: "daemon/daemon-1/pi://" + older.id, DaemonID: "daemon-1", Agent: "pi",
			AgentSessionID: "pi://" + older.id, HistoryPath: workspace + "/" + older.id + ".jsonl",
			Workspace: workspace, DisplayName: older.name, CreatedAt: now, UpdatedAt: now,
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 3 {
		t.Fatalf("workspace conversations were merged away: %+v", state.Sessions)
	}
	byID := make(map[string]session.Session, len(state.Sessions))
	for _, value := range state.Sessions {
		byID[value.ID] = value
	}
	for _, want := range []string{"daemon/daemon-1/pi://live-native", "daemon/daemon-1/pi://older-1", "daemon/daemon-1/pi://older-2"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("session %s is missing from %+v", want, state.Sessions)
		}
	}
	// The live session keeps its own name; history rows keep theirs.
	if byID["daemon/daemon-1/pi://live-native"].DisplayName != "Live session" {
		t.Fatalf("live name was overwritten: %+v", byID["daemon/daemon-1/pi://live-native"])
	}
	if byID["daemon/daemon-1/pi://older-1"].DisplayName != "Older one" {
		t.Fatalf("history name was overwritten: %+v", byID["daemon/daemon-1/pi://older-1"])
	}
	// A history conversation stays read-only and resumable.
	older := byID["daemon/daemon-1/pi://older-2"]
	if !older.Capabilities.CanReadHistory || !older.Capabilities.CanResume || older.Capabilities.CanSendInput {
		t.Fatalf("unexpected history capabilities: %+v", older.Capabilities)
	}
}

// Session naming has one owner (the Server) and one precedence: an explicit
// rename and an AI title outrank the first user message, which outranks the
// placeholder a wrapper starts with. Enrichment must only ever fill in a name
// that carries no information yet; otherwise the label flips between writers.
func TestStateEnrichesOnlyPlaceholderSessionNames(t *testing.T) {
	cases := []struct {
		name           string
		live           protocol.SessionSummary
		historyName    string
		historySource  string
		wantName       string
		wantNameSource string
	}{
		{
			name:           "placeholder is replaced",
			live:           protocol.SessionSummary{DisplayName: "New session", DisplayNameSource: session.DisplayNameSourceInitial},
			historyName:    "fix the build",
			historySource:  session.DisplayNameSourceFirstUser,
			wantName:       "fix the build",
			wantNameSource: session.DisplayNameSourceFirstUser,
		},
		{
			name:           "explicit rename is kept",
			live:           protocol.SessionSummary{DisplayName: "my own name", DisplayNameSource: session.DisplayNameSourceCustom},
			historyName:    "provider title",
			historySource:  session.DisplayNameSourceAITitle,
			wantName:       "my own name",
			wantNameSource: session.DisplayNameSourceCustom,
		},
		{
			name:           "ai title outranks a first user message",
			live:           protocol.SessionSummary{DisplayName: "polish release notes", DisplayNameSource: session.DisplayNameSourceAITitle},
			historyName:    "please polish the release notes",
			historySource:  session.DisplayNameSourceFirstUser,
			wantName:       "polish release notes",
			wantNameSource: session.DisplayNameSourceAITitle,
		},
		{
			name:           "legacy workspace-basename name is replaced",
			live:           protocol.SessionSummary{DisplayName: "agora", Workspace: "/tmp/agora", DisplayNameSource: session.DisplayNameSourceCustom},
			historyName:    "review the migration",
			historySource:  session.DisplayNameSourceFirstUser,
			wantName:       "review the migration",
			wantNameSource: session.DisplayNameSourceFirstUser,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Now().UTC()
			coord := coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}
			if err := db.CreateCoordination(context.Background(), coord); err != nil {
				t.Fatal(err)
			}
			srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
			live := test.live
			live.SessionID = "daemon/daemon-1/pi://native"
			live.DaemonID = "daemon-1"
			live.Agent = "pi"
			live.AgentSessionID = "pi://native"
			if live.Workspace == "" {
				live.Workspace = "/tmp/workspace"
			}
			live.State = session.StateRunning
			live.Connection = session.ConnectionObserved
			live.PID = 42
			live.CreatedAt, live.UpdatedAt = now, now
			seedDaemonLiveSession(srv, "daemon-1", live)
			seedDaemonHistorySession(srv, "daemon-1", protocol.HistorySessionSummary{
				SessionID: live.SessionID, DaemonID: "daemon-1", Agent: "pi", AgentSessionID: "pi://native",
				Workspace: live.Workspace, DisplayName: test.historyName, DisplayNameSource: test.historySource,
				CreatedAt: now, UpdatedAt: now,
			})

			// Two consecutive reads must agree: a name that changes between polls
			// is exactly the flicker this rule prevents.
			for attempt := 0; attempt < 2; attempt++ {
				req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
				resp := httptest.NewRecorder()
				srv.HTTP.Handler.ServeHTTP(resp, req)
				var state struct {
					Sessions []session.Session `json:"sessions"`
				}
				if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
					t.Fatal(err)
				}
				if len(state.Sessions) != 1 {
					t.Fatalf("unexpected sessions: %+v", state.Sessions)
				}
				got := state.Sessions[0]
				if got.DisplayName != test.wantName || got.DisplayNameSource != test.wantNameSource {
					t.Fatalf("attempt %d: name = %q (%s), want %q (%s)", attempt, got.DisplayName, got.DisplayNameSource, test.wantName, test.wantNameSource)
				}
			}
		})
	}
}

// The Web UI gets its Logto settings from the Server, so the endpoint has to be
// reachable without a token and has to report the mode truthfully: a client
// cannot log in before it knows how.
func TestPublicConfigDescribesTheAuthMode(t *testing.T) {
	newServer := func(mode string, issuer, audience string) *Server {
		db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return NewWithWebDirAndAuth(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil), t.TempDir(), config.ServerAuthConfig{Mode: mode, Issuer: issuer, Audience: audience})
	}
	read := func(srv *Server) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		resp := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
		}
		var value map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	t.Setenv("AGORA_LOGTO_ENDPOINT", "")
	t.Setenv("AGORA_LOGTO_APP_ID", "spa-app")
	logto := read(newServer(config.AuthModeLogto, "https://logto.example.com/oidc", "https://agora.example.com/api"))
	if logto["auth_mode"] != config.AuthModeLogto {
		t.Fatalf("auth_mode = %v", logto["auth_mode"])
	}
	if logto["logto_endpoint"] != "https://logto.example.com" {
		t.Fatalf("endpoint was not derived from the issuer: %v", logto["logto_endpoint"])
	}
	if logto["logto_app_id"] != "spa-app" || logto["logto_audience"] != "https://agora.example.com/api" {
		t.Fatalf("client settings missing: %+v", logto)
	}

	local := read(newServer(config.AuthModeLocal, "", ""))
	if local["auth_mode"] != config.AuthModeLocal {
		t.Fatalf("local mode reported %v", local["auth_mode"])
	}
	// A server without an application id must not hand the UI a half
	// configuration it would silently ignore.
	t.Setenv("AGORA_LOGTO_APP_ID", "")
	incomplete := read(newServer(config.AuthModeLogto, "https://logto.example.com/oidc", ""))
	if incomplete["logto_app_id"] != "" {
		t.Fatalf("expected no app id, got %+v", incomplete)
	}
}

func newSessionTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateCoordination(context.Background(), coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// A nil-capable manager keeps the split-server shape: sessions come from the
	// daemon hub, not from local providers.
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	return srv, db
}

func patchSession(t *testing.T, srv *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, sessionAPIPath(id, ""), bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	return resp
}

func TestSessionPreferencesAliasAndStar(t *testing.T) {
	srv, _ := newSessionTestServer(t)
	now := time.Now().UTC()
	newer := protocol.SessionSummary{SessionID: "daemon/daemon-1/claude://newer", DaemonID: "daemon-1", Agent: "claude", AgentSessionID: "claude://newer", ClaudeSessionID: "newer", Workspace: "/tmp/ws", DisplayName: "Newer", State: session.StateStopped, CreatedAt: now, UpdatedAt: now}
	older := protocol.SessionSummary{SessionID: "daemon/daemon-1/claude://older", DaemonID: "daemon-1", Agent: "claude", AgentSessionID: "claude://older", ClaudeSessionID: "older", Workspace: "/tmp/ws", DisplayName: "Older", State: session.StateStopped, CreatedAt: now, UpdatedAt: now.Add(-time.Hour)}
	seedDaemonLiveSession(srv, "daemon-1", newer)
	seedDaemonLiveSession(srv, "daemon-1", older)

	if resp := patchSession(t, srv, older.SessionID, `{"starred":true}`); resp.Code != http.StatusOK {
		t.Fatalf("star returned %d: %s", resp.Code, resp.Body.String())
	}
	alias := patchSession(t, srv, newer.SessionID, `{"display_name":"我的别名"}`)
	if alias.Code != http.StatusOK {
		t.Fatalf("alias returned %d: %s", alias.Code, alias.Body.String())
	}
	var aliased session.Session
	if err := json.Unmarshal(alias.Body.Bytes(), &aliased); err != nil {
		t.Fatal(err)
	}
	if aliased.DisplayName != "我的别名" || aliased.DisplayNameSource != session.DisplayNameSourceCustom {
		t.Fatalf("alias response = %+v", aliased)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("state returned %d: %s", resp.Code, resp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 2 {
		t.Fatalf("state sessions = %+v", state.Sessions)
	}
	// The starred, older session must lead the unstarred, newer one.
	if state.Sessions[0].ID != older.SessionID || !state.Sessions[0].Starred {
		t.Fatalf("starred session did not lead the list: %+v", state.Sessions)
	}
	for _, value := range state.Sessions {
		if value.ID == newer.SessionID && (value.DisplayName != "我的别名" || value.DisplayNameSource != session.DisplayNameSourceCustom) {
			t.Fatalf("alias was not applied in state: %+v", value)
		}
	}

	// Clearing both preferences removes the row instead of keeping an empty one.
	if resp := patchSession(t, srv, older.SessionID, `{"starred":false}`); resp.Code != http.StatusOK {
		t.Fatalf("unstar returned %d: %s", resp.Code, resp.Body.String())
	}
	if resp := patchSession(t, srv, newer.SessionID, `{"display_name":""}`); resp.Code != http.StatusOK {
		t.Fatalf("clear alias returned %d: %s", resp.Code, resp.Body.String())
	}
	for _, key := range []string{older.SessionID, newer.SessionID, "claude://older", "claude://newer"} {
		if _, err := srv.store.GetSessionPreference(context.Background(), "local", key); err == nil {
			t.Fatalf("preference %q survived clearing", key)
		}
	}
}

// A daemon hub holds every connected daemon's sessions, but a user must only
// see the sessions on devices they own. Acting on someone else's session was
// already forbidden; listing it must be too.
func TestVisibleSessionsFiltersToOwnedDevices(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	owner, err := db.FindByClaims(ctx, auth.ProvisionClaims{Provider: auth.ProviderLogto, Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := db.FindByClaims(ctx, auth.ProvisionClaims{Provider: auth.ProviderLogto, Subject: "intruder"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, device := range []store.Device{
		{ID: "daemon-owner", UserID: owner.UserID, CredentialHash: "hash-owner", CreatedAt: now},
		{ID: "daemon-intruder", UserID: intruder.UserID, CredentialHash: "hash-intruder", CreatedAt: now},
	} {
		if err := db.CreateDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	values := []session.Session{
		{ID: "daemon/daemon-owner/claude://a", DaemonID: "daemon-owner"},
		{ID: "daemon/daemon-intruder/claude://b", DaemonID: "daemon-intruder"},
	}

	logto := NewWithWebDirAndAuth(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil), t.TempDir(), config.ServerAuthConfig{Mode: config.AuthModeLogto})
	got, err := logto.visibleSessions(auth.WithPrincipal(ctx, owner), values)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DaemonID != "daemon-owner" {
		t.Fatalf("owner saw sessions they do not own: %+v", got)
	}
	// An unpaired daemon is not owned by anyone and must not leak either.
	ghost, err := logto.visibleSessions(auth.WithPrincipal(ctx, owner), []session.Session{{ID: "daemon/daemon-ghost/claude://c", DaemonID: "daemon-ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ghost) != 0 {
		t.Fatalf("unknown daemon leaked: %+v", ghost)
	}

	// trust-local mode is the deployment's own single-user trust boundary.
	local := NewWithWebDirAndAuth(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil), t.TempDir(), config.ServerAuthConfig{Mode: config.AuthModeLocal})
	localGot, err := local.visibleSessions(auth.WithPrincipal(ctx, owner), values)
	if err != nil {
		t.Fatal(err)
	}
	if len(localGot) != 2 {
		t.Fatalf("local mode filtered sessions: %+v", localGot)
	}
}

func TestDeleteRunningSessionIsRefused(t *testing.T) {
	srv, _ := newSessionTestServer(t)
	now := time.Now().UTC()
	sessionID := "daemon/daemon-1/claude://live"
	seedDaemonLiveSession(srv, "daemon-1", protocol.SessionSummary{SessionID: sessionID, DaemonID: "daemon-1", Agent: "claude", AgentSessionID: "claude://live", ClaudeSessionID: "live", Workspace: "/tmp/ws", DisplayName: "Live", State: session.StateRunning, Connection: session.ConnectionObserved, PID: 42, CreatedAt: now, UpdatedAt: now})

	req := httptest.NewRequest(http.MethodDelete, sessionAPIPath(sessionID, ""), nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("deleting a running session returned %d: %s", resp.Code, resp.Body.String())
	}
}

func TestDeleteHistorySessionRoutesToDaemon(t *testing.T) {
	srv, _ := newSessionTestServer(t)
	now := time.Now().UTC()
	sessionID := "daemon/daemon-1/claude://claude-1"
	connection := &daemonConnection{id: "daemon-1", send: make(chan protocol.Envelope, 8), lastSeen: now}
	srv.daemons.mu.Lock()
	srv.daemons.devices["daemon-1"] = connection
	srv.daemons.history["daemon-1"] = map[string]protocol.HistorySessionSummary{
		sessionID: {SessionID: sessionID, DaemonID: "daemon-1", Agent: "claude", AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1", Workspace: "/tmp/ws", HistoryPath: "/tmp/claude-1.jsonl", DisplayName: "Old", CreatedAt: now, UpdatedAt: now},
	}
	srv.daemons.mu.Unlock()

	// Seed an alias/star so the delete path can prove it cleans them up.
	if err := srv.store.UpsertSessionPreference(context.Background(), store.SessionPreference{UserID: "local", SessionKey: "claude://claude-1", DisplayName: "别名", Starred: true}); err != nil {
		t.Fatal(err)
	}

	go func() {
		request := <-connection.send
		if request.Type != protocol.SessionDelete {
			return
		}
		response, err := protocol.NewEnvelope(protocol.SessionDeleteResult, protocol.DeleteResultPayload{SessionID: sessionID, Deleted: true})
		if err != nil {
			return
		}
		response.RequestID = request.RequestID
		srv.daemons.mu.RLock()
		pending := srv.daemons.pending[request.RequestID]
		srv.daemons.mu.RUnlock()
		if pending != nil {
			pending <- response
		}
	}()

	req := httptest.NewRequest(http.MethodDelete, sessionAPIPath(sessionID, ""), nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("deleting a history session returned %d: %s", resp.Code, resp.Body.String())
	}
	srv.daemons.mu.RLock()
	_, historyLeft := srv.daemons.history["daemon-1"][sessionID]
	srv.daemons.mu.RUnlock()
	if historyLeft {
		t.Fatal("deleted history session survived in the hub")
	}
	if _, err := srv.store.GetSessionPreference(context.Background(), "local", "claude://claude-1"); err == nil {
		t.Fatal("alias/star survived session deletion")
	}
}
