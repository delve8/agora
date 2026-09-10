package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/sessionhost"
)

// TestMain lets the test binary act as `agora session-host`, so hosted sessions
// can be exercised end to end without the real CLI. SessionHostRegistry.Spawn
// re-executes the current binary with `session-host --config <path>`.
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

func piTranscriptRecord(sessionID, content string, at time.Time) []byte {
	record := map[string]any{
		"type":      "message",
		"id":        fmt.Sprintf("rec-%d", at.UnixNano()),
		"sessionId": sessionID,
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": content}},
		},
	}
	body, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return append(body, '\n')
}

func appendTranscript(t *testing.T, path string, body []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
}

func writePiTranscriptRecord(t *testing.T, path, sessionID, content string, at time.Time) {
	t.Helper()
	appendTranscript(t, path, piTranscriptRecord(sessionID, content, at))
}

// A runtime /resume switches the provider context without restarting the Agent
// process. Agora must follow that switch: the canonical Session ID and history
// path have to move to the session the user picked, otherwise the UI keeps a
// live row whose transcript file never exists.
//
// The switch is detected from evidence, not from keystrokes: Agora compares the
// lines the user submitted with the user records the provider appended to a
// different transcript in the same workspace.
func TestHostedPiResumeRebindFollowsPickedSession(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}

	const picked = "b1aa0f72-f174-4ac9-a0c5-6da7fc53bf78"
	transcript := filepath.Join(sessionDir, "2026-01-01T00-00-00-000Z_"+picked+".jsonl")
	header := fmt.Sprintf(`{"type":"session","id":%q,"cwd":%q}`, picked, workspace) + "\n"
	if err := os.WriteFile(transcript, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	writePiTranscriptRecord(t, transcript, picked, "an older question", time.Now().Add(-time.Hour))

	// A stand-in provider that behaves like an interactive TUI: it stays alive
	// and consumes input lines until the PTY closes.
	agent := filepath.Join(home, "fake-pi")
	script := "#!/bin/sh\nwhile IFS= read -r line; do printf 'echo:%s\\n' \"$line\"; done\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store := NewMemoryStore()
	manager := NewDaemonManager(store, "daemon-1", adapter.NewClaudeCodeAdapter(""), NewPTYManager("", home))
	manager.EnableSessionHosts(os.Args[0])
	manager.AttachPi(NewPiManager(PiConfig{Binary: agent, SessionDir: sessionDir}))
	defer manager.Close()

	provisional := "pending/session-rebind-test"
	created, err := manager.CreateManagedSessionWithAgentArgs(ctx, provisional, "coord-1", workspace, "New session", "terminal", "pi", nil)
	if err != nil {
		t.Fatalf("create hosted session: %v", err)
	}
	nativeID := strings.TrimPrefix(created.AgentSessionID, "pi://")
	if nativeID == "" {
		t.Fatalf("hosted session has no native id: %+v", created)
	}
	// Stop the Host through the client captured at creation: a successful
	// rebind moves it to a new registry key, so looking it up by id afterwards
	// would leak the Host process and its agent.
	var hostClient *sessionhost.Client
	t.Cleanup(func() {
		if hostClient == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hostClient.Stop(stopCtx)
		gone := time.Now().Add(5 * time.Second)
		for time.Now().Before(gone) {
			if _, err := hostClient.State(context.Background()); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	// Mirror the Daemon: adopt the agent's native identity, then rekey from the
	// provisional id to the canonical session id.
	if _, err := manager.SetAgentIdentity(ctx, provisional, "pi", "pi://"+nativeID); err != nil {
		t.Fatalf("set agent identity: %v", err)
	}
	canonical, err := session.NewSessionID("daemon-1", "pi", "pi://"+nativeID)
	if err != nil {
		t.Fatalf("canonical id: %v", err)
	}
	if _, err := manager.RekeySession(ctx, provisional, canonical); err != nil {
		t.Fatalf("rekey to canonical id: %v", err)
	}
	client, ok := manager.hosts.Get(canonical)
	if !ok {
		t.Fatalf("host not registered under %s after rekey", canonical)
	}
	hostClient = client

	const message = "switch to the older conversation"
	deadline := time.Now().Add(30 * time.Second)

	// Wait until the watcher captured the catalog baseline, otherwise the
	// confirming record would look like pre-existing history.
	var watcher *switchWatcher
	for {
		manager.mu.Lock()
		watcher = manager.switchWatchers[canonical]
		manager.mu.Unlock()
		if watcher != nil && watcher.knownCandidate(transcript) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("context switch watcher never captured a baseline")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The terminal wrapper types through the attach socket, so the test does
	// too. Submit the line first: in the real failure the user pressed Enter
	// while the provider was still writing the record, and a scan in that
	// window consumed the half-written line.
	attach, err := net.Dial("unix", client.AttachSocket())
	if err != nil {
		t.Fatalf("dial attach socket: %v", err)
	}
	defer attach.Close()
	if _, err := attach.Write([]byte(message + "\r")); err != nil {
		t.Fatalf("type into attach socket: %v", err)
	}
	for {
		if watcher.lineCount() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the submitted line was never observed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The provider then flushes the user's message into the picked session's
	// transcript, one piece at a time. A scan that runs between the pieces must
	// not consume the partial record.
	record := piTranscriptRecord(picked, message, time.Now())
	appendTranscript(t, transcript, record[:len(record)/2])
	time.Sleep(switchScanInterval + 700*time.Millisecond)
	appendTranscript(t, transcript, record[len(record)/2:])

	rebound, err := session.NewSessionID("daemon-1", "pi", "pi://"+picked)
	if err != nil {
		t.Fatal(err)
	}
	var value session.Session
	for time.Now().Before(deadline) {
		if current, getErr := store.GetSession(ctx, rebound); getErr == nil {
			value = current
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if value.ID != rebound {
		t.Fatalf("session was not rebound to %s", rebound)
	}
	if value.HistoryPath != transcript {
		t.Fatalf("rebound history path = %q, want %q", value.HistoryPath, transcript)
	}
	metadata, err := client.State(ctx)
	if err != nil {
		t.Fatalf("host state after rebind: %v", err)
	}
	if metadata.SessionID != rebound || metadata.HistoryPath != transcript {
		t.Fatalf("host metadata was not updated: session=%q history=%q", metadata.SessionID, metadata.HistoryPath)
	}
	if _, err := store.GetSession(ctx, canonical); err == nil {
		t.Fatalf("old session id %s still exists after rebind", canonical)
	}
}
