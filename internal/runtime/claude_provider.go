package runtime

import (
	"os"
	"strings"
)

// ClaudeProvider is the Claude Code provider as the Daemon sees it: which
// executable to run and with which arguments. The Agent process itself belongs
// to a Session Host, never to this process, so there is nothing here to start,
// stop or attach to.
type ClaudeProvider struct {
	binary    string
	binaryErr error
	homeDir   string
}

func NewClaudeProvider(binary, homeDir string) *ClaudeProvider {
	var binaryErr error
	if resolved, err := resolveAgentBinary(binary, "claude"); err == nil {
		binary = resolved
	} else {
		binaryErr = err
		if strings.TrimSpace(binary) == "" {
			// Preserve the historical constructor contract; the Session Host
			// reports the useful exec error if the real binary is not installed.
			binary = "claude"
		}
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return &ClaudeProvider{binary: binary, binaryErr: binaryErr, homeDir: homeDir}
}

// Err reports why the configured Claude executable could not be resolved.
// Session creation refuses to start rather than spawn a binary that would fail
// as an unrelated "host exited" error.
func (m *ClaudeProvider) Err() error {
	if m == nil {
		return nil
	}
	return m.binaryErr
}

// Command resumes an existing Claude conversation.
func (m *ClaudeProvider) Command(claudeSession string) []string {
	args := []string{m.binary}
	if claudeSession != "" {
		args = append(args, "--resume", claudeSession)
	}
	return args
}

// FreshCommand starts a new Claude conversation with a caller-selected native
// id, so a session has a canonical Agora identity before Claude writes any
// metadata file.
func (m *ClaudeProvider) FreshCommand(claudeSession string) []string {
	return []string{m.binary, "--session-id", claudeSession}
}

// cleanClaudeEnv strips the variables a parent Claude session or Claude Code
// extension injects. They tell claude it is a sub-session, which disables
// transcript/JSONL persistence. Without them, a fresh interactive claude writes
// its history JSONL normally.
func cleanClaudeEnv() []string {
	blocked := map[string]bool{
		"CLAUDE_CODE_CHILD_SESSION":                 true,
		"CLAUDE_CODE_SESSION_ID":                    true,
		"CLAUDE_PID":                                true,
		"CLAUDE_CODE_ENTRYPOINT":                    true,
		"CLAUDE_AGENT_SDK_VERSION":                  true,
		"CLAUDE_CODE_EXECPATH":                      true,
		"CURSOR_SPAWNED_BY_EXTENSION_ID":            true,
		"CURSOR_SPAWN_CHAIN":                        true,
		"AI_AGENT":                                  true,
		"CLAUDE_CODE_ENABLE_TASKS":                  true,
		"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING": true,
		"CLAUDE_CODE_SUBAGENT_MODEL":                true,
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if !blocked[key] {
			env = append(env, entry)
		}
	}
	return withDefaultTerminal(env)
}
