package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/session"
)

func TestDaemonRuntimeSummaryTracksExit(t *testing.T) {
	hub := newDaemonHub()
	connection := &daemonConnection{id: "daemon-1", lastSeen: time.Now().UTC()}
	hub.devices[connection.id] = connection
	sessionID := "daemon/daemon-1/claude://claude-1"
	resync, err := protocol.NewEnvelope(protocol.DaemonResync, protocol.ResyncPayload{DaemonID: connection.id, Sessions: []protocol.SessionSummary{{SessionID: sessionID, DaemonID: connection.id, Agent: "claude", AgentSessionID: "claude://claude-1", ClaudeSessionID: "claude-1", State: session.StateRunning, Connection: session.ConnectionObserved, PID: 42}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, resync); err != nil {
		t.Fatal(err)
	}
	value := hub.effectiveSession(session.Session{ID: sessionID, Workspace: "/tmp", Source: session.SourceManaged})
	if !value.Capabilities.CanSendInput || !value.Capabilities.CanInterrupt || !value.Capabilities.CanReadTerminal || value.ProcessID != 42 {
		t.Fatalf("runtime summary was not applied: %+v", value)
	}
	exit, err := protocol.NewEnvelope(protocol.SessionExit, protocol.ExitPayload{SessionID: sessionID, State: session.StateStopped})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, exit); err != nil {
		t.Fatal(err)
	}
	value = hub.effectiveSession(value)
	if value.State != session.StateStopped || value.ProcessID != 0 || value.Capabilities.CanSendInput || !value.Capabilities.CanResume {
		t.Fatalf("exit summary was not applied: %+v", value)
	}
}

func TestDaemonOfflineCanonicalSessionRemainsResumable(t *testing.T) {
	hub := newDaemonHub()
	value := hub.effectiveSession(session.Session{ID: "daemon/daemon-1/claude://claude-1", Workspace: "/tmp", Source: session.SourceManaged})
	if value.State != session.StateStopped || value.ProcessID != 0 || !value.Capabilities.CanResume || value.Capabilities.CanSendInput {
		t.Fatalf("offline canonical session capabilities are wrong: %+v", value)
	}
}

func TestDaemonResyncReplacesHistoryCatalog(t *testing.T) {
	hub := newDaemonHub()
	connection := &daemonConnection{id: "daemon-1", lastSeen: time.Now().UTC()}
	hub.devices[connection.id] = connection
	first, err := protocol.NewEnvelope(protocol.DaemonResync, protocol.ResyncPayload{DaemonID: connection.id, History: []protocol.HistorySessionSummary{{SessionID: "history-1", ClaudeSessionID: "claude-1", DisplayName: "First", UpdatedAt: time.Now().UTC()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, first); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.historySession("history-1", "coord-1"); !ok {
		t.Fatal("expected first history route")
	}
	second, err := protocol.NewEnvelope(protocol.DaemonResync, protocol.ResyncPayload{DaemonID: connection.id, History: []protocol.HistorySessionSummary{{SessionID: "history-2", ClaudeSessionID: "claude-2", DisplayName: "Second", UpdatedAt: time.Now().UTC()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, second); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.historySession("history-1", "coord-1"); ok {
		t.Fatal("stale history route survived resync")
	}
	if value, ok := hub.historySession("history-2", "coord-1"); !ok || value.DisplayName != "Second" {
		t.Fatalf("unexpected replacement history: %+v, %v", value, ok)
	}
	hub.remove(connection)
	if _, ok := hub.historySession("history-2", "coord-1"); ok {
		t.Fatal("history route survived daemon disconnect")
	}
}

func TestDaemonSessionUpdateRemovesHistoryAlias(t *testing.T) {
	hub := newDaemonHub()
	connection := &daemonConnection{id: "daemon-1", lastSeen: time.Now().UTC()}
	hub.devices[connection.id] = connection
	hub.history[connection.id] = map[string]protocol.HistorySessionSummary{
		"history-id": {SessionID: "history-id", DaemonID: connection.id, Agent: "pi", AgentSessionID: "pi://native-1", HistoryPath: "/tmp/pi/native.jsonl"},
	}
	update, err := protocol.NewEnvelope(protocol.SessionUpdate, protocol.SessionUpdatePayload{
		SessionID: "live-id", DaemonID: connection.id, Agent: "pi", AgentSessionID: "pi://native-1", HistoryPath: "/tmp/pi/native.jsonl",
		State: session.StateRunning, Connection: session.ConnectionObserved, PID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, update); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.historySession("history-id", "coord-1"); ok {
		t.Fatal("history alias survived managed session update")
	}
}

func TestDaemonSessionRebindRemovesHistoryAlias(t *testing.T) {
	hub := newDaemonHub()
	connection := &daemonConnection{id: "daemon-1", lastSeen: time.Now().UTC()}
	hub.devices[connection.id] = connection
	hub.sessions[connection.id] = map[string]protocol.SessionSummary{
		"daemon/daemon-1/pi://fresh": {SessionID: "daemon/daemon-1/pi://fresh", DaemonID: connection.id, Agent: "pi", AgentSessionID: "pi://fresh", State: session.StateRunning, Connection: session.ConnectionObserved, PID: 42},
	}
	hub.history[connection.id] = map[string]protocol.HistorySessionSummary{
		"daemon/daemon-1/pi://picked": {SessionID: "daemon/daemon-1/pi://picked", DaemonID: connection.id, Agent: "pi", AgentSessionID: "pi://picked", HistoryPath: "/tmp/pi/picked.jsonl", DisplayName: "older conversation"},
	}
	rebind, err := protocol.NewEnvelope(protocol.SessionRebind, protocol.SessionRebindPayload{
		OldSessionID: "daemon/daemon-1/pi://fresh", NewSessionID: "daemon/daemon-1/pi://picked", DaemonID: connection.id, Agent: "pi", AgentSessionID: "pi://picked", HistoryPath: "/tmp/pi/picked.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, rebind); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.historySession("daemon/daemon-1/pi://picked", "coord-1"); ok {
		t.Fatal("history alias survived managed session rebind")
	}
	live, ok := hub.sessions[connection.id]["daemon/daemon-1/pi://picked"]
	if !ok || live.PID != 42 {
		t.Fatalf("rebound live session = %+v, ok=%v", live, ok)
	}
}

func TestSameHostOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/daemon/ws", nil)
	if !sameHostOrigin(req, "http://127.0.0.1:8080") {
		t.Fatal("expected same origin")
	}
	if sameHostOrigin(req, "https://attacker.example") {
		t.Fatal("unexpected cross origin acceptance")
	}
}

func TestDaemonIsConnected(t *testing.T) {
	hub := newDaemonHub()
	if hub.isConnected("daemon-1") {
		t.Fatal("expected not connected before registration")
	}
	connection := &daemonConnection{id: "daemon-1", lastSeen: time.Now().UTC()}
	hub.devices[connection.id] = connection
	if !hub.isConnected("daemon-1") {
		t.Fatal("expected connected after registration")
	}
	hub.remove(connection)
	if hub.isConnected("daemon-1") {
		t.Fatal("expected disconnected after removal")
	}
}

func TestDaemonDisconnectClearsRuntimeState(t *testing.T) {
	hub := newDaemonHub()
	connection := &daemonConnection{id: "daemon-1"}
	hub.devices[connection.id] = connection
	hub.sessions[connection.id] = map[string]protocol.SessionSummary{"session-1": {SessionID: "session-1"}}
	hub.history[connection.id] = map[string]protocol.HistorySessionSummary{"history-1": {SessionID: "history-1"}}
	hub.routes["session-1"] = connection.id
	hub.routes["history-1"] = connection.id

	hub.disconnectDaemon(connection.id)
	if hub.isConnected(connection.id) {
		t.Fatal("daemon remained connected after explicit disconnect")
	}
	hub.mu.RLock()
	_, hasSessions := hub.sessions[connection.id]
	_, hasHistory := hub.history[connection.id]
	_, hasSessionRoute := hub.routes["session-1"]
	_, hasHistoryRoute := hub.routes["history-1"]
	hub.mu.RUnlock()
	if hasSessions || hasHistory || hasSessionRoute || hasHistoryRoute {
		t.Fatalf("disconnect left runtime state: sessions=%v history=%v session-route=%v history-route=%v", hasSessions, hasHistory, hasSessionRoute, hasHistoryRoute)
	}

	// Repeating cleanup for an already-offline daemon is a safe no-op.
	hub.disconnectDaemon(connection.id)
}
func TestRequestToDaemonOffline(t *testing.T) {
	hub := newDaemonHub()
	if _, err := hub.requestToDaemon(context.Background(), "daemon-1", protocol.SessionCreate, protocol.SessionCreatePayload{}, protocol.SessionCreated); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("expected offline error, got %v", err)
	}
}

func TestCreateSessionTargetOfflineDaemon(t *testing.T) {
	hub := newDaemonHub()
	if _, err := hub.createSession(context.Background(), protocol.SessionCreatePayload{}, "daemon-1"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("expected offline error, got %v", err)
	}
	// Without a target, session creation still requires some connected daemon.
	if _, err := hub.createSession(context.Background(), protocol.SessionCreatePayload{}, ""); err == nil || !strings.Contains(err.Error(), "no daemon is connected") {
		t.Fatalf("expected no-daemon error, got %v", err)
	}
}

func TestSanitizeDeviceName(t *testing.T) {
	if got := sanitizeDeviceName("  hello  "); got != "hello" {
		t.Fatalf("sanitize = %q, want hello", got)
	}
	if got := sanitizeDeviceName(""); got != "" {
		t.Fatalf("empty sanitize = %q, want empty", got)
	}
	long := "x" + strings.Repeat("y", 200)
	if got := sanitizeDeviceName(long); len([]rune(got)) != maxDeviceNameLength {
		t.Fatalf("expected truncation to %d, got %d", maxDeviceNameLength, len([]rune(got)))
	}
}

// When the Daemon reports that it dropped frames, the Server cannot trust its own
// view and must ask for a fresh resync instead of silently keeping a partial one.
func TestGappedResyncRequestsAFullResync(t *testing.T) {
	hub := newDaemonHub(nil, "local")
	connection := &daemonConnection{id: "daemon-1", send: make(chan protocol.Envelope, 8)}
	hub.mu.Lock()
	hub.devices[connection.id] = connection
	hub.mu.Unlock()

	frame, err := protocol.NewEnvelope(protocol.DaemonResync, protocol.ResyncPayload{
		DaemonID: connection.id,
		Gap:      true,
		History:  []protocol.HistorySessionSummary{{SessionID: "history-1", Agent: "pi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleFrame(connection, frame); err != nil {
		t.Fatalf("handle resync: %v", err)
	}
	select {
	case request := <-connection.send:
		if request.Type != protocol.ServerResyncRequest {
			t.Fatalf("expected a resync request, got %q", request.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("a gapped resync did not request a full resync")
	}
}

// The Server derives capabilities for a live daemon session from the summary it
// received. A session whose provider transcript does not exist yet must not
// advertise history: the UI would otherwise show an empty conversation.
func TestLiveSessionHistoryCapabilityFollowsTheTranscript(t *testing.T) {
	now := time.Now().UTC()
	base := protocol.SessionSummary{
		SessionID: "daemon/d/pi://native", DaemonID: "d", Agent: "pi",
		AgentSessionID: "pi://native", Workspace: "/tmp/ws", DisplayName: "Pi",
		State: session.StateRunning, Connection: session.ConnectionObserved, PID: 42,
		CreatedAt: now, UpdatedAt: now,
	}
	fresh := liveSummarySession(base, "coord-1")
	if fresh.Capabilities.CanReadHistory {
		t.Fatalf("a session without a transcript advertised history: %+v", fresh.Capabilities)
	}
	if !fresh.Capabilities.CanSendInput || !fresh.Capabilities.CanStream {
		t.Fatalf("live capabilities were lost: %+v", fresh.Capabilities)
	}
	withHistory := base
	withHistory.HistoryPath = "/tmp/ws/session.jsonl"
	resolved := liveSummarySession(withHistory, "coord-1")
	if !resolved.Capabilities.CanReadHistory {
		t.Fatalf("a session with a transcript did not advertise history: %+v", resolved.Capabilities)
	}
}
