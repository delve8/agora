package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// runWrapper is the claude-wrapper entry point. With no session id it asks
// Agora to create a managed session for the current workspace, then attaches
// the terminal to that session's PTY. With an existing Agora session id it
// attaches directly. The real Claude process is always a child of agora serve;
// this process only renders and forwards the terminal bytes.
func runWrapper(args []string) error {
	sessionID := ""
	if len(args) > 0 && strings.HasPrefix(args[0], "sess-") {
		sessionID = args[0]
	}
	if sessionID == "" {
		created, err := createWrapperSession()
		if err != nil {
			return err
		}
		sessionID = created
	}
	return runAttach(sessionID)
}

func createWrapperSession() (string, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return "", err
	}
	base := filepath.Base(workspace)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "New session"
	}
	apiBase := agoraAPIBase()
	stateRequest, err := agoraRequest(http.MethodGet, apiBase+"/api/state", nil)
	if err != nil {
		return "", fmt.Errorf("create Agora state request: %w", err)
	}
	stateResp, err := http.DefaultClient.Do(stateRequest)
	if err != nil {
		return "", fmt.Errorf("connect to Agora: %w", err)
	}
	defer stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Agora returned %s while loading state", stateResp.Status)
	}
	var state struct {
		Coordination struct {
			ID string `json:"id"`
		} `json:"coordination"`
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		return "", err
	}
	if state.Coordination.ID == "" {
		return "", fmt.Errorf("Agora returned no coordination")
	}
	body, _ := json.Marshal(map[string]string{
		"workspace":    workspace,
		"display_name": base,
		"role":         "terminal",
	})
	request, err := agoraRequest(http.MethodPost, apiBase+"/api/coordinations/"+url.PathEscape(state.Coordination.ID)+"/sessions", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("create Agora session request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("create Agora session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("Agora returned %s while creating session", resp.Status)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("Agora returned no session id")
	}
	return created.ID, nil
}

func agoraAPIBase() string {
	base := os.Getenv("AGORA_ADDR")
	if base == "" {
		return "http://127.0.0.1:8080"
	}
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		return strings.TrimRight(base, "/")
	}
	return "http://" + strings.TrimRight(base, "/")
}

// runAttach connects the current terminal to a managed session's PTY served by
// Agora. It puts the terminal in raw mode and relays bytes in both directions.
func runAttach(id string) error {
	addr, err := attachSocket(id)
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(os.Stdout, conn)
		done <- err
	}()
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		done <- err
	}()
	<-done
	return nil
}

func attachSocket(sessionID string) (string, error) {
	request, err := agoraRequest(http.MethodGet, agoraAPIBase()+"/api/sessions/"+url.PathEscape(sessionID)+"/attach", nil)
	if err != nil {
		return "", fmt.Errorf("create attach request: %w", err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("query Agora for attach address: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Agora returned %s for session %s", resp.Status, sessionID)
	}
	var body struct {
		Socket string `json:"socket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Socket == "" {
		return "", fmt.Errorf("session %s has no attach socket (is it running?)", sessionID)
	}
	return body.Socket, nil
}

// agoraRequest adds the optional CLI bearer token. Local mode does not need
// it; Logto deployments can use AGORA_ACCESS_TOKEN (or AGORA_TOKEN) when a
// terminal wrapper needs to call the protected API.
func agoraRequest(method, endpoint string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(firstNonEmptyEnv("AGORA_ACCESS_TOKEN", "AGORA_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request, nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
