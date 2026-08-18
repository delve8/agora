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
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/daemon"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/server"
	"github.com/delve8/agora/internal/store"
)

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

	d, err := daemon.New(daemon.Config{ID: "e2e-daemon", ServerURL: serverURL, HomeDir: home, ClaudeBinary: "false", Heartbeat: 100 * time.Millisecond})
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
}

type stateResponse struct {
	Sessions []struct {
		ID             string `json:"id"`
		AgentSessionID string `json:"agent_session_id"`
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
