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
