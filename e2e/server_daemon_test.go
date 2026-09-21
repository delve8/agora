package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/daemon"
	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/server"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/sessionhost"
	"github.com/delve8/agora/internal/store"
	"github.com/delve8/agora/internal/terminal"
)

// TestMain lets the e2e binary act as `agora session-host`, so hosted sessions
// can be exercised end to end without the real CLI: SessionHostRegistry.Spawn
// re-executes the test binary with `session-host --config <path>`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "session-host" {
		os.Exit(runSessionHostProcess(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func runSessionHostProcess(args []string) int {
	configPath := ""
	for index := 0; index < len(args); index++ {
		if args[index] == "--config" && index+1 < len(args) {
			configPath = args[index+1]
		}
	}
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "session-host test helper: --config is required")
		return 2
	}
	sessionhost.IgnoreTerminalSignals()
	config, err := sessionhost.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	host, err := sessionhost.New(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	if err := host.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "session-host test helper: %v\n", err)
		return 1
	}
	return 0
}

func TestDaemonLocalWrapperFlow(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeBinary := filepath.Join(home, "claude")
	script := "#!/bin/sh\nprintf 'READY\\r\\n'\nwhile IFS= read -r line; do printf 'received: %s\\r\\n' \"$line\"; done\n"
	if err := os.WriteFile(claudeBinary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := server.New(":0", db, nil)
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := serverListener.Addr().String()
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.HTTP.Serve(serverListener) }()
	defer func() {
		_ = srv.Shutdown(context.Background())
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down")
		}
	}()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-wrapper-e2e-%d.sock", time.Now().UnixNano()))
	d, err := daemon.New(daemon.Config{ID: "wrapper-daemon", ServerURL: "ws://" + addr + "/api/daemon/ws", LocalSocketPath: socketPath, HomeDir: home, ClaudeBinary: claudeBinary, PiBinary: "false", Heartbeat: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Session Hosts outlive the Daemon by design, so the test has to stop the
	// Agent it started instead of leaving a process behind.
	t.Cleanup(func() { stopSessionHosts(t, home) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- d.Run(ctx) }()
	defer func() {
		_ = d.Close()
		cancel()
		select {
		case <-daemonDone:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not shut down")
		}
		_ = os.Remove(socketPath)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Creating a session spawns a Session Host; under a fully parallel package
	// run that is slower than the response itself ever is.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	request := protocol.WrapperRequest{Workspace: workspace, DisplayName: "wrapper test", Role: "terminal", Agent: "claude", Prompts: []string{"hello"}}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response protocol.WrapperResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.SessionID == "" || response.Socket == "" {
		t.Fatalf("unexpected wrapper response: %+v", response)
	}
	identity, err := session.ParseSessionID(response.SessionID)
	if err != nil || identity.DaemonID != "wrapper-daemon" || identity.Agent != "claude" {
		t.Fatalf("wrapper returned non-canonical session id %q: %v", response.SessionID, err)
	}

	if output := attachScreen(t, response.Socket, 20*time.Second); !strings.Contains(output, "READY") {
		t.Fatalf("wrapper attach output = %q", output)
	}

	// A terminal that attaches paints the screen first, and it asks the Daemon
	// for it over the local socket rather than relying on the session host to
	// replay. That is what makes a host built before attach-time replay still
	// show its screen.
	screen := localSnapshot(t, socketPath, response.SessionID)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "READY") {
		t.Fatalf("local snapshot lines = %q, want the READY the fake Agent printed", screen.Lines)
	}
	if screen.Rows == 0 || screen.Cols == 0 {
		t.Fatalf("local snapshot has no size: %+v", screen)
	}
}

// localSnapshot asks a running Daemon for a session's screen over its local
// socket, exactly as `agora attach` does before it starts streaming.
func localSnapshot(t *testing.T, socketPath, sessionID string) terminal.Snapshot {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(protocol.SessionSnapshotRequest{Type: protocol.SessionSnapshot, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	var response protocol.SessionSnapshotResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" {
		t.Fatalf("snapshot error: %s", response.Error)
	}
	var snapshot terminal.Snapshot
	if err := json.Unmarshal(response.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// attachScreen dials a session's attach socket and returns what the screen shows.
// Each connection replays the current screen, so an attach that lands before the
// Agent has painted anything legitimately sees nothing yet and is retried: the
// wrapper can win that race, it just must not lose the screen because of it.
func attachScreen(t *testing.T, socket string, within time.Duration) string {
	t.Helper()
	var output string
	deadline := time.Now().Add(within)
	for {
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		data, readErr := io.ReadAll(conn)
		_ = conn.Close()
		if readErr != nil && !os.IsTimeout(readErr) {
			t.Fatal(readErr)
		}
		output = string(data)
		if strings.Contains(output, "READY") || time.Now().After(deadline) {
			return output
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonStartsBeforeServerAndResyncsHistory(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-tmp-e2e")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeID := "11111111-1111-4111-8111-111111111111"
	history := filepath.Join(project, claudeID+".jsonl")
	content := fmt.Sprintf(`{"sessionId":%q,"cwd":"%s","type":"user","uuid":"u1","timestamp":"2026-08-18T08:00:00Z","message":{"role":"user","content":"hello from e2e"}}
{"sessionId":%q,"type":"assistant","uuid":"a1","timestamp":"2026-08-18T08:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hello back"}]}}
`, claudeID, filepath.Join(home, "workspace"), claudeID)
	if err := os.WriteFile(history, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	serverURL := "ws://" + addr + "/api/daemon/ws"

	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-history-e2e-%d.sock", time.Now().UnixNano()))
	d, err := daemon.New(daemon.Config{ID: "e2e-daemon", ServerURL: serverURL, LocalSocketPath: socketPath, HomeDir: home, ClaudeBinary: "false", PiBinary: "false", Heartbeat: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- d.Run(ctx) }()
	defer func() { _ = d.Close(); cancel(); <-daemonDone }()

	db, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), nil)
	srv := server.New(":0", db, manager)
	serverListener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.HTTP.Serve(serverListener) }()
	defer func() {
		_ = srv.Shutdown(context.Background())
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down")
		}
	}()

	state := waitState(t, "http://"+addr, func(state stateResponse) bool {
		for _, value := range state.Sessions {
			if value.AgentSessionID == "claude://"+claudeID {
				return true
			}
		}
		return false
	})
	var found string
	for _, value := range state.Sessions {
		if value.AgentSessionID == "claude://"+claudeID {
			found = value.ID
			break
		}
	}
	if found == "" {
		t.Fatal("history session did not resync")
	}
	body := getJSON(t, "http://"+addr+"/api/sessions/"+url.PathEscape(found)+"/events")
	var events []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("history response is not JSON: %v; body=%s", err, body)
	}
	if len(events) < 2 || events[0].Content == "" {
		t.Fatalf("unexpected history response: %s", body)
	}

	// Pi sessions created outside the Agora wrapper must be discovered after
	// the daemon has already started. This is the case for a normal `pi`
	// process that was launched before or independently of Agora.
	piRoot := filepath.Join(home, ".pi", "agent", "sessions", "--tmp-external-pi--")
	if err := os.MkdirAll(piRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	piID := "22222222-2222-4222-8222-222222222222"
	piHistory := filepath.Join(piRoot, "external.jsonl")
	piContent := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-08-18T09:00:00Z","cwd":"%s"}
{"type":"message","id":"pu1","timestamp":"2026-08-18T09:00:01Z","message":{"role":"user","content":"external pi session"}}
{"type":"message","id":"pa1","timestamp":"2026-08-18T09:00:02Z","message":{"role":"assistant","content":"hello from pi"}}
`, piID, filepath.Join(home, "pi-workspace"))
	if err := os.WriteFile(piHistory, []byte(piContent), 0o600); err != nil {
		t.Fatal(err)
	}
	waitState(t, "http://"+addr, func(state stateResponse) bool {
		for _, value := range state.Sessions {
			if value.AgentSessionID == "pi://"+piID {
				return true
			}
		}
		return false
	})
}

type stateResponse struct {
	Sessions []struct {
		ID             string `json:"id"`
		Agent          string `json:"agent"`
		AgentSessionID string `json:"agent_session_id"`
		Workspace      string `json:"workspace"`
		Source         string `json:"source"`
		ProcessID      int    `json:"process_id"`
	} `json:"sessions"`
}

func waitState(t *testing.T, base string, ready func(stateResponse) bool) stateResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(base + "/api/state")
		if err == nil {
			var state stateResponse
			if decodeErr := json.NewDecoder(response.Body).Decode(&state); decodeErr == nil {
				response.Body.Close()
				if ready(state) {
					return state
				}
			} else {
				response.Body.Close()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for Server state")
	return stateResponse{}
}

func getJSON(t *testing.T, target string) []byte {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET %s returned %s: %s", target, response.Status, body)
	}
	var body json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// A live session must not hide the other conversations in its workspace. The
// Daemon advertises history separately from live sessions, and the Server merges
// both, so all three rows have to survive.
func TestDaemonStateKeepsHistorySessionsBesideLiveSession(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeBinary := filepath.Join(home, "claude")
	script := "#!/bin/sh\nprintf 'READY\\r\\n'\nwhile IFS= read -r line; do printf 'received: %s\\r\\n' \"$line\"; done\n"
	if err := os.WriteFile(claudeBinary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// Two Pi conversations in the same workspace, created outside Agora.
	piRoot := filepath.Join(home, ".pi", "agent", "sessions", "--workspace--")
	if err := os.MkdirAll(piRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	olderIDs := []string{"aaaaaaaa-1111-4111-8111-111111111111", "bbbbbbbb-2222-4222-8222-222222222222"}
	for index, id := range olderIDs {
		content := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-08-18T09:00:00Z","cwd":%q}
{"type":"message","id":"u%d","timestamp":"2026-08-18T09:00:01Z","message":{"role":"user","content":"older conversation %d"}}
{"type":"message","id":"a%d","timestamp":"2026-08-18T09:00:02Z","message":{"role":"assistant","content":"reply %d"}}
`, id, workspace, index, index, index, index)
		if err := os.WriteFile(filepath.Join(piRoot, fmt.Sprintf("older-%d.jsonl", index)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := server.New(":0", db, nil)
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := serverListener.Addr().String()
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.HTTP.Serve(serverListener) }()
	defer func() {
		_ = srv.Shutdown(context.Background())
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down")
		}
	}()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("agora-history-live-e2e-%d.sock", time.Now().UnixNano()))
	d, err := daemon.New(daemon.Config{ID: "history-daemon", ServerURL: "ws://" + addr + "/api/daemon/ws", LocalSocketPath: socketPath, HomeDir: home, ClaudeBinary: claudeBinary, PiBinary: "false", Heartbeat: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- d.Run(ctx) }()
	defer func() {
		_ = d.Close()
		cancel()
		select {
		case <-daemonDone:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not shut down")
		}
		_ = os.Remove(socketPath)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(conn).Encode(protocol.WrapperRequest{Workspace: workspace, DisplayName: "wrapper test", Role: "terminal", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	var response protocol.WrapperResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.SessionID == "" {
		t.Fatalf("unexpected wrapper response: %+v", response)
	}

	state := waitState(t, "http://"+addr, func(state stateResponse) bool {
		live, older := 0, 0
		for _, value := range state.Sessions {
			if filepath.Clean(value.Workspace) != filepath.Clean(workspace) {
				continue
			}
			switch {
			case value.Source == "managed" && value.ProcessID > 0:
				live++
			case value.AgentSessionID == "pi://"+olderIDs[0] || value.AgentSessionID == "pi://"+olderIDs[1]:
				older++
			}
		}
		return live == 1 && older == 2
	})
	seen := make(map[string]bool, len(state.Sessions))
	for _, value := range state.Sessions {
		seen[value.AgentSessionID] = true
	}
	for _, id := range olderIDs {
		if !seen["pi://"+id] {
			t.Fatalf("history session %s is hidden by the live session: %+v", id, state.Sessions)
		}
	}
}

// stopSessionHosts terminates the Session Hosts a test started. They own their
// Agent processes and deliberately survive their Daemon, so nothing else will.
func stopSessionHosts(t *testing.T, home string) {
	t.Helper()
	root := filepath.Join(home, ".agora", "runtime", "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		metadata, loadErr := sessionhost.LoadMetadata(filepath.Join(root, entry.Name(), "metadata.json"))
		if loadErr != nil || metadata.HostPID <= 0 {
			continue
		}
		if process, findErr := os.FindProcess(metadata.HostPID); findErr == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
}
