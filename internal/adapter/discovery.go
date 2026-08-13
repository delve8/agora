package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/event"
)

type sessionMetadata struct {
	PID       int             `json:"pid"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	StartedAt json.RawMessage `json:"startedAt"`
	Version   string          `json:"version"`
}

func parseStartedAt(raw json.RawMessage) time.Time {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return time.Time{}
	}
	if strings.HasPrefix(value, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return time.Time{}
		}
		value = strings.TrimSpace(decoded)
	}
	if millis, err := strconv.ParseInt(value, 10, 64); err == nil && millis > 0 {
		return time.UnixMilli(millis).UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// ReadSessionMetadata loads the Claude session id recorded in a session
// metadata file (~/.claude/sessions/<pid>.json). Managed sessions read this
// after launch so a later `claude --resume <id>` can restore the same
// conversation.
func ReadSessionMetadata(path string) (sessionID, cwd string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var meta sessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", "", err
	}
	return meta.SessionID, meta.CWD, nil
}

// FindHistoryBySessionID locates the Claude history JSONL for a session id by
// searching ~/.claude/projects for a matching <session-id>.jsonl file.
func FindHistoryBySessionID(homeDir, sessionID string) string {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	projects := filepath.Join(homeDir, ".claude", "projects")
	var match string
	_ = filepath.WalkDir(projects, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || match != "" {
			return nil
		}
		if filepath.Ext(entry.Name()) == ".jsonl" && strings.TrimSuffix(entry.Name(), ".jsonl") == sessionID {
			match = path
		}
		return nil
	})
	return match
}

type HistoryCursor struct {
	Path       string `json:"path"`
	ByteOffset int64  `json:"byte_offset"`
	Line       int    `json:"line"`
	LastID     string `json:"last_id,omitempty"`
}

type HistoryRecord struct {
	Event  event.Event
	Cursor HistoryCursor
}

// ReadHistory reads complete JSONL records after cursor. An incomplete trailing line is retained.
func ReadHistory(ctx context.Context, cursor HistoryCursor, sessionID string) ([]HistoryRecord, error) {
	file, err := os.Open(cursor.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < cursor.ByteOffset {
		cursor.ByteOffset = 0
		cursor.Line = 0
		cursor.LastID = ""
	}
	if _, err := file.Seek(cursor.ByteOffset, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	result := make([]HistoryRecord, 0)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		cursor.ByteOffset += int64(len(line)) + 1
		cursor.Line++
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		parsed, err := ParseHistoryEvent(sessionID, line)
		if err != nil {
			continue
		}
		cursor.LastID = parsed.Event.ExternalID
		result = append(result, HistoryRecord{Event: parsed.Event, Cursor: cursor})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func ParseHistoryEvent(sessionID string, data []byte) (HistoryRecord, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return HistoryRecord{}, fmt.Errorf("parse Claude history event: %w", err)
	}
	typ, _ := raw["type"].(string)
	subtype, _ := raw["subtype"].(string)
	externalID, _ := raw["uuid"].(string)
	if externalID == "" {
		externalID, _ = raw["id"].(string)
	}
	kind := typ
	semanticType := typ
	role := ""
	switch typ {
	case "message", "assistant":
		kind = event.KindAssistant
		semanticType = "text"
		role = "assistant"
	case "user":
		kind = event.KindUser
		semanticType = "text"
		role = "user"
	case "tool_use":
		kind = event.KindTool
		semanticType = "tool_call"
		role = "tool"
	case "tool_result", "tool":
		kind = event.KindTool
		semanticType = "tool_result"
		role = "tool"
	case "result":
		kind = event.KindResult
		semanticType = "status"
	case "system":
		kind = event.KindSystem
		semanticType = "status"
	}
	if isError, _ := raw["is_error"].(bool); isError {
		kind = event.KindError
	}
	content := historyContent(raw)
	createdAt := time.Now().UTC()
	for _, key := range []string{"timestamp", "created_at", "createdAt"} {
		if value, ok := raw[key].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				createdAt = parsed
				break
			}
		}
	}
	if externalID == "" {
		externalID = fmt.Sprintf("line-%x", stableHash(data))
	}
	return HistoryRecord{Event: event.Event{ID: "evt-" + strconv.FormatInt(time.Now().UnixNano(), 10), ExternalID: externalID, SessionID: sessionID, Source: event.SourceHistory, Kind: kind, Type: semanticType, Role: role, Subtype: subtype, Content: content, Summary: summarize(content), IsError: kind == event.KindError, RawJSON: string(data), CreatedAt: createdAt}, Cursor: HistoryCursor{Path: ""}}, nil
}

func summarize(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		return value[:240] + "…"
	}
	return value
}

// historyContent extracts human-readable text from a Claude history event.
// Real Claude events nest message blocks under "message.content", where each
// block is {type:"text",text:...}, {type:"thinking",thinking:...} or a
// tool_use block; tool_result carries text either as a string or a content
// block list. Recurse so text and tool calls at any depth are surfaced
// instead of silently dropped. Adjacent parts join with a space so a thinking
// block and the following text read as one sentence, not a blank line.
func historyContent(raw map[string]any) string {
	if value, ok := raw["message"].(map[string]any); ok {
		if parts := textFromBlocks(value["content"]); len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	for _, key := range []string{"result", "content", "text", "thinking"} {
		if value, ok := raw[key]; ok {
			if parts := textFromBlocks(value); len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return ""
}

// textFromBlocks returns readable text for any value shape: strings, block
// maps ({type:"text"|"thinking"|"tool_use"|...}), nested content arrays, and
// tool input maps. Non-text blocks are stringified so their presence is
// visible in the stream rather than skipped.
func textFromBlocks(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(typed) != "" {
			return []string{typed}
		}
		return nil
	case map[string]any:
		switch {
		case isString(typed["text"]):
			return []string{typed["text"].(string)}
		case isString(typed["thinking"]):
			return []string{"[thinking] " + typed["thinking"].(string)}
		case isString(typed["name"]), isString(typed["tool_name"]):
			name, _ := typed["name"].(string)
			if name == "" {
				name, _ = typed["tool_name"].(string)
			}
			input, _ := json.Marshal(typed["input"])
			if len(input) > 0 && string(input) != "null" {
				return []string{"[" + name + "] " + string(input)}
			}
			return []string{"[" + name + "]"}
		default:
			if parts := textFromBlocks(typed["content"]); len(parts) > 0 {
				return parts
			}
			// Non-text block (e.g. web_search, reasoning): surface its type so
			// the event is visible in the stream even without readable text.
			if typ, ok := typed["type"].(string); ok && typ != "" {
				return []string{"[" + typ + "]"}
			}
			return nil
		}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFromBlocks(item)...)
		}
		return parts
	default:
		return nil
	}
}

func isString(value any) bool {
	_, ok := value.(string)
	return ok
}

func stableHash(data []byte) uint64 {
	var hash uint64 = 14695981039346656037
	for _, value := range data {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return hash
}
