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
