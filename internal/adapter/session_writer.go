package adapter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HandoffMessage is one settled turn rendered into the target Agent's native
// transcript. A handoff carries results, not process: replaying the source
// conversation would both consume the target's context window and make it
// re-derive work that is already finished.
type HandoffMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// SessionFileWriter creates a native provider session file that the target
// Agent can resume. Direct session generation is the primary handoff path; a
// caller falls back to another delivery only when a provider's transcript
// cannot be written safely.
type SessionFileWriter interface {
	Agent() string
	// WriteSessionFile writes a brand-new session for nativeID under workspace
	// and returns the transcript path. workspace must already exist.
	WriteSessionFile(workspace, nativeID string, messages []HandoffMessage) (string, error)
}

// NewSessionFileWriter returns the transcript writer for an agent. It reports
// false when the provider has no safe writer yet, so the caller can use a
// fallback instead of guessing at an unsupported format.
func NewSessionFileWriter(agent, homeDir, piSessionDir, claudeVersion string) (SessionFileWriter, bool) {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "pi":
		return PiSessionWriter{SessionDir: piSessionDir, HomeDir: homeDir}, true
	case "claude", "claude-code":
		return ClaudeSessionWriter{HomeDir: homeDir, Version: claudeVersion}, true
	default:
		return nil, false
	}
}

// NewNativeSessionID returns a random RFC 4122 version 4 UUID, which is the
// native session id shape Claude and Pi use.
func NewNativeSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

// CanonicalWorkspace resolves the workspace the way the Agents do: symlinks are
// followed first. Claude keys its project directory off the canonical path, so
// writing under the unresolved path makes `--resume` report "No conversation
// found" (on macOS /tmp must become /private/tmp).
func CanonicalWorkspace(workspace string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", absolute, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", resolved)
	}
	return resolved, nil
}

func validateHandoffMessages(messages []HandoffMessage) error {
	if len(messages) == 0 {
		return fmt.Errorf("handoff requires at least one message")
	}
	for index, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			return fmt.Errorf("handoff message %d has unsupported role %q", index, message.Role)
		}
		if strings.TrimSpace(message.Content) == "" {
			return fmt.Errorf("handoff message %d is empty", index)
		}
	}
	return nil
}

func randomHexID(length int) (string, error) {
	if length <= 0 {
		return "", nil
	}
	buffer := make([]byte, (length+1)/2)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer)[:length], nil
}

// writeJSONLAtomic writes records as JSON lines via a temp file and rename so a
// reader never sees a half-written session.
func writeJSONLAtomic(path string, records []any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".handoff-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	for _, record := range records {
		body, err := json.Marshal(record)
		if err != nil {
			temp.Close()
			return err
		}
		if _, err := temp.Write(append(body, '\n')); err != nil {
			temp.Close()
			return err
		}
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
