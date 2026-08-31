package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// runPiWrapper creates (or attaches to) a Pi session and renders its native
// TUI through the same PTY attach path used by the Claude wrapper.
func runPiWrapper(args []string) error {
	sessionID := ""
	if len(args) > 0 && looksLikeAgoraSessionID(args[0]) {
		sessionID = strings.TrimSpace(args[0])
		args = args[1:]
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("pi wrapper does not pass Pi options to the Daemon; configure provider/model on the Daemon (initial prompts are positional arguments)")
		}
	}
	if sessionID == "" {
		var err error
		sessionID, err = createAgentWrapperSession("pi")
		if err != nil {
			return err
		}
	}
	for _, prompt := range args {
		if err := sendAgentWrapperMessage(sessionID, prompt); err != nil {
			return err
		}
	}
	return runAttach(sessionID)
}

func looksLikeAgoraSessionID(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "sess-") || strings.HasPrefix(value, "daemon/")
}

func sendAgentWrapperMessage(sessionID, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	endpoint := agoraAPIBase() + "/api/sessions/" + url.PathEscape(sessionID) + "/messages"
	request, err := agoraRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create initial prompt request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("send initial prompt to Agora: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Agora returned %s while sending initial prompt", resp.Status)
	}
	return nil
}

func createAgentWrapperSession(agent string) (string, error) {
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
	createInput := map[string]string{
		"workspace":    workspace,
		"display_name": base,
		"role":         "terminal",
		"agent":        agent,
	}
	// In a split deployment this identifies the Daemon on the same workstation.
	// When omitted, the Server chooses an available Daemon (useful for a single-
	// Daemon setup).
	if daemonID := strings.TrimSpace(os.Getenv("AGORA_DAEMON_ID")); daemonID != "" {
		createInput["daemon_id"] = daemonID
	}
	body, err := json.Marshal(createInput)
	if err != nil {
		return "", err
	}
	endpoint := apiBase + "/api/coordinations/" + url.PathEscape(state.Coordination.ID) + "/sessions"
	request, err := agoraRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
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
