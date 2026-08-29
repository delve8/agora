package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

func seedDevice(t *testing.T, srv *Server, deviceID, name string) string {
	t.Helper()
	ctx := context.Background()
	local, err := srv.store.GetOrCreateLocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateDevice(ctx, store.Device{ID: deviceID, UserID: local.UserID, CredentialHash: store.HashSecret(deviceID + "-credential"), Name: name, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return local.UserID
}

func TestListDevicesReturnsCleanDTO(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDevice(t, srv, "device-1", "home-server")
	// Mark it connected so the DTO reflects hub state.
	srv.daemons.mu.Lock()
	srv.daemons.devices["device-1"] = &daemonConnection{id: "device-1"}
	srv.daemons.mu.Unlock()

	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected 1 device, got %d", len(raw))
	}
	got := raw[0]
	if got["device_id"] != "device-1" || got["name"] != "home-server" || got["connected"] != true {
		t.Fatalf("unexpected device dto: %+v", got)
	}
	for key := range got {
		if key == "credential_hash" || key == "CredentialHash" || key == "user_id" || key == "UserID" {
			t.Fatalf("device dto leaks internal field %q", key)
		}
	}
}

func TestRenameDeviceUpdatesName(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	userID := seedDevice(t, srv, "device-1", "old-name")

	resp := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"name":"  mac-mini  "}`)
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/devices/device-1/name", body))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	values, err := db.ListDevices(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Name != "mac-mini" {
		t.Fatalf("name not updated: %+v", values)
	}
}

func TestRenameDeviceRejectsUnknown(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/devices/device-missing/name", bytes.NewBufferString(`{"name":"x"}`)))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestRenameDeviceRejectsEmptyName(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDevice(t, srv, "device-1", "old-name")
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/devices/device-1/name", bytes.NewBufferString(`{"name":"   "}`)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestRevokeDeviceDisconnectsAndHidesSessions(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDevice(t, srv, "device-1", "home-server")
	now := time.Now().UTC()
	if err := db.CreateCoordination(context.Background(), coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sessionID := "daemon/device-1/claude://claude-1"
	seedDaemonLiveSession(srv, "device-1", protocol.SessionSummary{
		SessionID: sessionID, DaemonID: "device-1", Agent: "claude", AgentSessionID: "claude://claude-1",
		ClaudeSessionID: "claude-1", Workspace: "/tmp", DisplayName: "Old daemon session",
		State: session.StateWaiting, Connection: session.ConnectionObserved, PID: 42, CreatedAt: now, UpdatedAt: now,
	})
	seedDaemonHistorySession(srv, "device-1", protocol.HistorySessionSummary{
		SessionID: "daemon/device-1/claude://history-1", DaemonID: "device-1", Agent: "claude",
		AgentSessionID: "claude://history-1", ClaudeSessionID: "history-1", Workspace: "/tmp", DisplayName: "Old history",
		CreatedAt: now, UpdatedAt: now,
	})
	srv.daemons.mu.Lock()
	srv.daemons.routes[sessionID] = "device-1"
	srv.daemons.mu.Unlock()

	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/devices/device-1/revoke", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if srv.daemons.isConnected("device-1") {
		t.Fatal("revoked daemon remained connected")
	}
	srv.daemons.mu.RLock()
	_, hasSessions := srv.daemons.sessions["device-1"]
	_, hasHistory := srv.daemons.history["device-1"]
	_, hasRoute := srv.daemons.routes[sessionID]
	srv.daemons.mu.RUnlock()
	if hasSessions || hasHistory || hasRoute {
		t.Fatalf("revoke left hub state: sessions=%v history=%v route=%v", hasSessions, hasHistory, hasRoute)
	}

	stateResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(stateResp, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if stateResp.Code != http.StatusOK {
		t.Fatalf("state returned %d: %s", stateResp.Code, stateResp.Body.String())
	}
	var state struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(stateResp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	for _, value := range state.Sessions {
		if value.DaemonID == "device-1" {
			t.Fatalf("revoked daemon session remained visible: %+v", value)
		}
	}

	devicesResp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(devicesResp, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if devicesResp.Code != http.StatusOK {
		t.Fatalf("devices returned %d: %s", devicesResp.Code, devicesResp.Body.String())
	}
	var devices []deviceDTO
	if err := json.Unmarshal(devicesResp.Body.Bytes(), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].RevokedAt == nil || devices[0].Connected {
		t.Fatalf("revoked device visibility is wrong: %+v", devices)
	}
}

func TestRevokeDeviceIsIdempotent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	seedDevice(t, srv, "device-1", "home-server")
	for attempt := 0; attempt < 2; attempt++ {
		resp := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/devices/device-1/revoke", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("revoke attempt %d returned %d: %s", attempt+1, resp.Code, resp.Body.String())
		}
	}
}

func TestCreateSessionTargetsOfflineDevice(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(":0", db, runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil))
	now := time.Now().UTC()
	if err := db.CreateCoordination(context.Background(), coordination.Coordination{ID: "coord-1", Name: "Test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"workspace":"/tmp","daemon_id":"device-offline"}`)
	srv.HTTP.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/coordinations/coord-1/sessions", body))
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for offline target, got %d: %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error == "" {
		t.Fatal("expected an error message for the offline target")
	}
}
