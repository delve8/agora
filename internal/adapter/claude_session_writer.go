package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeSessionWriter writes a Claude Code transcript that `claude --resume
// <id>` finds and resumes. Claude looks the session up by its project
// directory, which is derived from the canonical workspace path, so the file
// must be placed under that encoded directory and the records must carry the
// same cwd.
type ClaudeSessionWriter struct {
	// HomeDir is the user's home (defaults to os.UserHomeDir).
	HomeDir string
	// Version is stamped into each record, e.g. "2.1.278".
	Version string
	// Entrypoint defaults to "sdk-cli".
	Entrypoint string
}

func (w ClaudeSessionWriter) Agent() string { return "claude" }

func (w ClaudeSessionWriter) WriteSessionFile(workspace, nativeID string, messages []HandoffMessage) (string, error) {
	if strings.TrimSpace(nativeID) == "" {
		return "", fmt.Errorf("claude session id is required")
	}
	if err := validateHandoffMessages(messages); err != nil {
		return "", err
	}
	cwd, err := CanonicalWorkspace(workspace)
	if err != nil {
		return "", err
	}
	home := w.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	// Claude encodes the project directory by replacing "/" and "_" with "-".
	encoded := strings.NewReplacer("/", "-", "_", "-").Replace(cwd)
	path := filepath.Join(home, ".claude", "projects", encoded, nativeID+".jsonl")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("claude session %s already exists", path)
	}

	now := time.Now().UTC()
	entrypoint := strings.TrimSpace(w.Entrypoint)
	if entrypoint == "" {
		entrypoint = "sdk-cli"
	}
	version := strings.TrimSpace(w.Version)
	parent := ""
	lastUUID := ""
	lastPrompt := ""
	records := make([]any, 0, len(messages)+1)
	for _, message := range messages {
		uuid, err := NewNativeSessionID()
		if err != nil {
			return "", err
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		var parentValue any
		if parent != "" {
			parentValue = parent
		}
		record := map[string]any{
			"parentUuid":  parentValue,
			"isSidechain": false,
			"type":        role,
			"uuid":        uuid,
			"timestamp":   now.Format("2006-01-02T15:04:05.000Z"),
			"userType":    "external",
			"entrypoint":  entrypoint,
			"cwd":         cwd,
			"sessionId":   nativeID,
		}
		if version != "" {
			record["version"] = version
		}
		if role == "user" {
			promptID, err := NewNativeSessionID()
			if err != nil {
				return "", err
			}
			record["promptId"] = promptID
			record["message"] = map[string]any{"role": "user", "content": message.Content}
			record["permissionMode"] = "default"
			record["promptSource"] = "sdk"
			record["turnOrigin"] = "sdk"
			lastPrompt = message.Content
		} else {
			messageID, err := randomHexID(24)
			if err != nil {
				return "", err
			}
			record["message"] = map[string]any{
				"id":          "msg_" + messageID,
				"type":        "message",
				"role":        "assistant",
				"model":       "claude",
				"usage":       map[string]any{"input_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0},
				"content":     []any{map[string]any{"type": "text", "text": message.Content}},
				"stop_reason": "end_turn",
			}
		}
		records = append(records, record)
		parent = uuid
		lastUUID = uuid
	}
	records = append(records, map[string]any{"type": "last-prompt", "lastPrompt": lastPrompt, "leafUuid": lastUUID, "sessionId": nativeID})
	if err := writeJSONLAtomic(path, records); err != nil {
		return "", err
	}
	return path, nil
}
