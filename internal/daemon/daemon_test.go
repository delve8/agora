package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/session"
)

func TestResolveWorkspacePrefersRuntimeThenHistory(t *testing.T) {
	runtimeStore := runtime.NewMemoryStore()
	_ = runtimeStore.CreateSession(context.Background(), session.Session{ID: "daemon/daemon-1/claude://claude-1", Workspace: "/from/runtime"})
	manager := runtime.NewManager(runtimeStore, nil, nil)
	d := &Daemon{manager: manager, historySessions: []session.Session{{ID: "daemon/daemon-1/claude://claude-1", Workspace: "/from/history"}, {ID: "daemon/daemon-1/claude://claude-2", Workspace: "/second"}}}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://claude-1"); got != "/from/runtime" {
		t.Fatalf("runtime workspace = %q", got)
	}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://claude-2"); got != "/second" {
		t.Fatalf("history workspace = %q", got)
	}
	if got := d.resolveWorkspace("daemon/daemon-1/claude://missing"); got != "" {
		t.Fatalf("missing workspace = %q", got)
	}
}

func TestSameHistorySessionsDetectsAddedAndUpdatedSessions(t *testing.T) {
	created := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	left := []session.Session{{ID: "daemon/d/pi://one", Agent: "pi", AgentSessionID: "pi://one", HistoryPath: "/tmp/one.jsonl", Workspace: "/tmp/project", DisplayName: "one", CreatedAt: created, UpdatedAt: created}}
	right := append([]session.Session(nil), left...)
	if !sameHistorySessions(left, right) {
		t.Fatal("identical history sessions were considered different")
	}
	if sameHistorySessions(left, append(right, session.Session{ID: "daemon/d/pi://two"})) {
		t.Fatal("added history session was not detected")
	}
	right[0].UpdatedAt = created.Add(time.Second)
	if sameHistorySessions(left, right) {
		t.Fatal("updated history session was not detected")
	}
}

func TestReconnectDelayIsBounded(t *testing.T) {
	if got := reconnectDelay(0); got != time.Second {
		t.Fatalf("attempt 0 delay = %s", got)
	}
	if got := reconnectDelay(3); got != 8*time.Second {
		t.Fatalf("attempt 3 delay = %s", got)
	}
	if got := reconnectDelay(100); got != 32*time.Second {
		t.Fatalf("attempt 100 delay = %s", got)
	}
}

func TestWaitReconnectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitReconnect(ctx, time.Minute); err != context.Canceled {
		t.Fatalf("waitReconnect error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took too long: %s", elapsed)
	}
}

func TestDaemonHTTPBaseNormalizesServerAndWebSocketURLs(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"ws://127.0.0.1:8080/api/daemon/ws", "http://127.0.0.1:8080"},
		{"wss://agora.example/api/daemon/ws/", "https://agora.example"},
	}
	for _, test := range tests {
		got, err := daemonHTTPBase(test.input)
		if err != nil {
			t.Fatalf("daemonHTTPBase(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Errorf("daemonHTTPBase(%q) = %q, want %q", test.input, got, test.want)
		}
	}
	if _, err := daemonHTTPBase("not a URL"); err == nil {
		t.Fatal("daemonHTTPBase accepted an invalid URL")
	}
}

func TestWaitRegisteredWaitsUntilRegistration(t *testing.T) {
	d := &Daemon{registeredCh: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- d.waitRegistered(ctx) }()
	select {
	case err := <-ready:
		t.Fatalf("waitRegistered returned before registration: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	d.connMu.Lock()
	d.registered = true
	close(d.registeredCh)
	d.registeredClosed = true
	d.connMu.Unlock()
	if err := <-ready; err != nil {
		t.Fatalf("waitRegistered after registration: %v", err)
	}
}

func TestLocalWrapperSocketIsPrivateAndCleansUp(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-daemon-test-%d.sock", time.Now().UnixNano()))
	t.Setenv("AGORA_DAEMON_SOCKET", socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	d := &Daemon{config: Config{ID: "daemon-test"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.startLocalWrapperServer(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %o, want 600", info.Mode().Perm())
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after Close: %v", err)
	}
}

// The local socket carries both wrapper requests and provider session reports.
// A report must be routed to the manager path and answered gracefully, never as
// an invalid wrapper request and never with a panic.
func TestLocalWrapperSocketRoutesSessionReports(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-report-test-%d.sock", time.Now().UnixNano()))
	t.Setenv("AGORA_DAEMON_SOCKET", socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{config: Config{ID: "daemon-test"}}
	if err := d.startLocalWrapperServer(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	send := func(body string) protocol.SessionReportResponse {
		t.Helper()
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, body); err != nil {
			t.Fatal(err)
		}
		var reply protocol.SessionReportResponse
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		return reply
	}

	if reply := send(`{"type":"session.report"}`); reply.Error != "host_id is required" {
		t.Fatalf("missing host_id reply = %+v", reply)
	}
	reply := send(`{"type":"session.report","host_id":"host-1","reason":"startup","session_id":"abc"}`)
	if reply.Error == "" || strings.Contains(reply.Error, "invalid wrapper request") {
		t.Fatalf("report was not routed to the session manager: %+v", reply)
	}
}

// History is only advertised when the set changes, so a resync that never
// reached the Server must be remembered; otherwise every provider history
// session stays hidden until the set changes again.
func TestShouldResyncHistoryRetriesAfterAFailedSend(t *testing.T) {
	cases := []struct {
		name    string
		changed bool
		forced  bool
		dirty   bool
		want    bool
	}{
		{"unchanged and delivered", false, false, false, false},
		{"changed", true, false, false, true},
		{"forced", false, true, false, true},
		{"previous send failed", false, false, true, true},
	}
	for _, test := range cases {
		if got := shouldResyncHistory(test.changed, test.forced, test.dirty); got != test.want {
			t.Errorf("%s: shouldResyncHistory(%v,%v,%v) = %v, want %v", test.name, test.changed, test.forced, test.dirty, got, test.want)
		}
	}
}

// A terminal asks the Daemon what exists instead of being handed a canonical id.
// The answer has to be scoped to the working directory, has to tell running
// sessions from history ones (only the former can be attached to), and must not
// list a history entry that a live session already covers.
func TestSessionListIsScopedAndMarksHistory(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-list-test-%d.sock", time.Now().UnixNano()))
	t.Setenv("AGORA_DAEMON_SOCKET", socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	other := t.TempDir()
	updated := time.Now().UTC().Add(-2 * time.Hour)
	store := runtime.NewMemoryStore()
	_ = store.CreateSession(ctx, session.Session{
		ID: "daemon-test/pi://running-1", Agent: "pi", AgentSessionID: "pi://running-1",
		Workspace: workspace, DisplayName: "live one", State: session.StateRunning, Source: session.SourceManaged, UpdatedAt: time.Now().UTC(),
	})
	_ = store.CreateSession(ctx, session.Session{
		ID: "daemon-test/pi://elsewhere", Agent: "pi", AgentSessionID: "pi://elsewhere",
		Workspace: other, State: session.StateRunning, Source: session.SourceManaged, UpdatedAt: time.Now().UTC(),
	})
	d := &Daemon{
		config:  Config{ID: "daemon-test"},
		manager: runtime.NewManager(store, nil, nil),
		historySessions: []session.Session{
			// Same native session as the live one: the history copy is redundant.
			{ID: "daemon-test/pi://running-1", Agent: "pi", AgentSessionID: "pi://running-1", Workspace: workspace, HistoryPath: "/tmp/running-1.jsonl", UpdatedAt: updated},
			{ID: "daemon-test/pi://history-1", Agent: "pi", AgentSessionID: "pi://history-1", Workspace: workspace, DisplayName: "old one", HistoryPath: "/tmp/history-1.jsonl", UpdatedAt: updated},
		},
	}
	if err := d.startLocalWrapperServer(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	ask := func(workspace string) protocol.SessionListResponse {
		t.Helper()
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := json.NewEncoder(conn).Encode(protocol.SessionListRequest{Type: protocol.SessionList, Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		var reply protocol.SessionListResponse
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		if reply.Error != "" {
			t.Fatalf("session list error: %s", reply.Error)
		}
		return reply
	}

	local := ask(workspace)
	if len(local.Sessions) != 2 {
		t.Fatalf("sessions in the workspace = %+v, want the live one and one history entry", local.Sessions)
	}
	byID := map[string]protocol.SessionListEntry{}
	for _, entry := range local.Sessions {
		byID[entry.SessionID] = entry
	}
	if entry := byID["daemon-test/pi://running-1"]; !entry.Attachable || entry.State != session.StateRunning {
		t.Fatalf("live session = %+v, want it attachable", entry)
	}
	if entry := byID["daemon-test/pi://history-1"]; entry.Attachable || entry.Source != session.SourceHistory {
		t.Fatalf("history session = %+v, want it marked as history", entry)
	}
	if _, exists := byID["daemon-test/pi://elsewhere"]; exists {
		t.Fatal("a session from another workspace was listed")
	}

	// An empty workspace lists the machine, which is what `--all` means.
	if all := ask(""); len(all.Sessions) != 3 {
		t.Fatalf("sessions on the machine = %+v, want every workspace", all.Sessions)
	}
}

// Attaching has to be able to paint the screen first, otherwise it starts with an
// empty terminal. The Daemon answers that question locally, and must answer it
// calmly when there is no live screen to read.
func TestSessionSnapshotReportsWhyThereIsNoScreen(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-snapshot-test-%d.sock", time.Now().UnixNano()))
	t.Setenv("AGORA_DAEMON_SOCKET", socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := runtime.NewMemoryStore()
	_ = store.CreateSession(ctx, session.Session{ID: "daemon-test/pi://history-1", Agent: "pi", Workspace: t.TempDir(), Source: session.SourceHistory, State: session.StateStopped})
	d := &Daemon{config: Config{ID: "daemon-test"}, manager: runtime.NewManager(store, nil, nil)}
	if err := d.startLocalWrapperServer(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	ask := func(body string) protocol.SessionSnapshotResponse {
		t.Helper()
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, body); err != nil {
			t.Fatal(err)
		}
		var reply protocol.SessionSnapshotResponse
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		return reply
	}

	if reply := ask(`{"type":"session.snapshot"}`); reply.Error != "session_id is required" {
		t.Fatalf("missing session id reply = %+v", reply)
	}
	reply := ask(`{"type":"session.snapshot","session_id":"daemon-test/pi://history-1"}`)
	if reply.Error == "" {
		t.Fatalf("a session without a live screen answered with a snapshot: %+v", reply)
	}
	if len(reply.Snapshot) != 0 {
		t.Fatalf("snapshot = %s, want none for a stopped session", reply.Snapshot)
	}
}

// A Daemon without a session manager must answer instead of crashing.
func TestSessionSnapshotWithoutAManager(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-snapshot-nomanager-%d.sock", time.Now().UnixNano()))
	t.Setenv("AGORA_DAEMON_SOCKET", socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{config: Config{ID: "daemon-test"}}
	if err := d.startLocalWrapperServer(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"type":"session.snapshot","session_id":"daemon-test/pi://x"}`); err != nil {
		t.Fatal(err)
	}
	var reply protocol.SessionSnapshotResponse
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != "session manager is unavailable" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestSessionRunningUsesProcessAndState(t *testing.T) {
	manager := runtime.NewManager(runtime.NewMemoryStore(), nil, nil)
	d := &Daemon{manager: manager}
	if d.sessionRunning(session.Session{ID: "daemon/d/claude://one", State: session.StateStopped}) {
		t.Fatal("stopped session reported running")
	}
	if !d.sessionRunning(session.Session{ID: "daemon/d/claude://two", State: session.StateRunning, ProcessID: 42}) {
		t.Fatal("running session with a process was reported stopped")
	}
	if d.sessionRunning(session.Session{ID: "daemon/d/claude://three", State: session.StateRunning, ProcessID: 0}) {
		t.Fatal("running session without a process was treated as live")
	}
}
