package server

import (
	"net/http/httptest"
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

func TestSameHostOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/daemon/ws", nil)
	if !sameHostOrigin(req, "http://127.0.0.1:8080") {
		t.Fatal("expected same origin")
	}
	if sameHostOrigin(req, "https://attacker.example") {
		t.Fatal("unexpected cross origin acceptance")
	}
}
