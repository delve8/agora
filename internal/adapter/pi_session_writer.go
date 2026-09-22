package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PiSessionWriter writes a Pi session JSONL that `pi --session <path>` resumes.
// The layout mirrors a real Pi transcript: a session header followed by
// id/parentId-chained records.
type PiSessionWriter struct {
	// SessionDir is Pi's session root; empty means ~/.pi/agent/sessions.
	SessionDir string
	HomeDir    string
	Provider   string
	Model      string
}

func (w PiSessionWriter) Agent() string { return "pi" }

func (w PiSessionWriter) WriteSessionFile(workspace, nativeID string, messages []HandoffMessage) (string, error) {
	if strings.TrimSpace(nativeID) == "" {
		return "", fmt.Errorf("pi session id is required")
	}
	if err := validateHandoffMessages(messages); err != nil {
		return "", err
	}
	cwd, err := CanonicalWorkspace(workspace)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(w.SessionDir)
	if root == "" {
		home := w.HomeDir
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		root = filepath.Join(home, ".pi", "agent", "sessions")
	}
	// Pi keys the project directory with --<trim(cwd,"/") with "/" replaced by
	// "-">--; underscores and spaces are kept (verified against real Pi 0.87).
	projectKey := "--" + strings.ReplaceAll(strings.Trim(cwd, string(filepath.Separator)), string(filepath.Separator), "-") + "--"
	now := time.Now().UTC()
	path := filepath.Join(root, projectKey, now.Format("2006-01-02T15-04-05-000Z")+"_"+nativeID+".jsonl")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("pi session %s already exists", path)
	}

	newID := func() (string, error) { return randomHexID(8) }
	records := make([]any, 0, len(messages)+3)
	records = append(records, map[string]any{"type": "session", "version": 3, "id": nativeID, "timestamp": now.Format("2006-01-02T15:04:05.000Z"), "cwd": cwd})

	parent := ""
	if provider := strings.TrimSpace(w.Provider); provider != "" || strings.TrimSpace(w.Model) != "" {
		id, err := newID()
		if err != nil {
			return "", err
		}
		records = append(records, map[string]any{"type": "model_change", "id": id, "parentId": nil, "timestamp": now.Format("2006-01-02T15:04:05.000Z"), "provider": w.Provider, "modelId": w.Model})
		parent = id
	}

	for _, message := range messages {
		id, err := newID()
		if err != nil {
			return "", err
		}
		var parentValue any
		if parent != "" {
			parentValue = parent
		}
		record := map[string]any{
			"type":      "message",
			"id":        id,
			"parentId":  parentValue,
			"timestamp": now.Format("2006-01-02T15:04:05.000Z"),
			"message":   piHandoffMessage(message, w.Provider, w.Model, now),
		}
		records = append(records, record)
		parent = id
	}
	if err := writeJSONLAtomic(path, records); err != nil {
		return "", err
	}
	return path, nil
}

func piHandoffMessage(message HandoffMessage, provider, model string, now time.Time) map[string]any {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	content := []any{map[string]any{"type": "text", "text": message.Content}}
	body := map[string]any{"role": role, "content": content, "timestamp": now.UnixMilli()}
	if role == "assistant" {
		body["api"] = "openai-completions"
		body["provider"] = provider
		body["model"] = model
		body["usage"] = map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0, "cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
		body["stopReason"] = "stop"
	}
	return body
}
