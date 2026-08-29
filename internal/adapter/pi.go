package adapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/delve8/agora/internal/event"
)

// PiHistorySummary describes one Pi session file discovered on disk.
type PiHistorySummary struct {
	SessionID    string
	Path         string
	Workspace    string
	SessionName  string
	FirstUser    string
	FirstEventAt time.Time
	LastEventAt  time.Time
	ModifiedAt   time.Time
	Size         int64
	ProjectDir   string
	Meaningful   bool
}

type PiHistoryCursor struct {
	Path       string `json:"path"`
	ByteOffset int64  `json:"byte_offset"`
	Line       int    `json:"line"`
	LastID     string `json:"last_id,omitempty"`
}

type PiHistoryRecord struct {
	Event       event.Event
	Cursor      PiHistoryCursor
	SessionName string
}

// PiEvent is the normalized result of parsing a Pi live JSON line. Responses
// are returned with Response=true and are not transcript events.
type PiEvent struct {
	Event    event.Event
	Response bool
	Done     bool
}

// ParsePiJSONEvent parses the first normalized event from --mode json or
// --mode rpc stdout. A single Pi message can contain thinking, text, tool
// calls, and tool results; callers that need all of them should use
// ParsePiJSONEvents.
func ParsePiJSONEvent(sessionID string, data []byte) (PiEvent, error) {
	values, err := ParsePiJSONEvents(sessionID, data)
	if err != nil {
		return PiEvent{}, err
	}
	if len(values) == 0 {
		return PiEvent{}, nil
	}
	return values[0], nil
}

// ParsePiJSONEvents normalizes every meaningful content block in a Pi record.
// This prevents a message containing thinking + toolCall + text from being
// flattened into one assistant text event.
func ParsePiJSONEvents(sessionID string, data []byte) ([]PiEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse Pi JSON event: %w", err)
	}
	if typ, _ := raw["type"].(string); typ == "response" {
		return []PiEvent{{Response: true}}, nil
	}
	values := piEventsFromRaw(sessionID, raw, data)
	result := make([]PiEvent, 0, len(values))
	for _, value := range values {
		result = append(result, PiEvent{Event: value, Done: piEventDone(raw)})
	}
	return result, nil
}

// ParsePiHistoryEvent parses the first normalized event from one append-only
// Pi session entry. Use ParsePiHistoryEvents when a record has multiple
// content blocks.
func ParsePiHistoryEvent(sessionID string, data []byte) (event.Event, error) {
	values, err := ParsePiHistoryEvents(sessionID, data)
	if err != nil {
		return event.Event{}, err
	}
	if len(values) == 0 {
		return event.Event{}, nil
	}
	return values[0], nil
}

func ParsePiHistoryEvents(sessionID string, data []byte) ([]event.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse Pi history event: %w", err)
	}
	return piEventsFromRaw(sessionID, raw, data), nil
}

func piEventsFromRaw(sessionID string, raw map[string]any, data []byte) []event.Event {
	typ, _ := raw["type"].(string)
	baseID := firstString(raw, "id", "uuid", "event_id")
	if baseID == "" {
		baseID = fmt.Sprintf("line-%x", sha256.Sum256(data))
	}
	createdAt := piEventTime(raw)
	if message, ok := raw["message"].(map[string]any); ok {
		role := firstString(message, "role")
		messageToolName := firstString(message, "toolName", "tool_name")
		blocks, hasBlocks := message["content"].([]any)
		if hasBlocks && len(blocks) > 0 {
			values := make([]event.Event, 0, len(blocks))
			for index, blockValue := range blocks {
				block, ok := blockValue.(map[string]any)
				if !ok {
					continue
				}
				if value := piEventFromBlock(sessionID, baseID, index, typ, role, messageToolName, block, data, createdAt); value != nil {
					values = append(values, *value)
				}
			}
			if len(values) > 0 {
				return values
			}
		}
	}
	value := piEventFromRawSingle(sessionID, baseID, typ, raw, data, createdAt)
	return []event.Event{value}
}

func piEventFromBlock(sessionID, baseID string, index int, typ, role, messageToolName string, block map[string]any, data []byte, createdAt time.Time) *event.Event {
	blockType := firstString(block, "type")
	id := fmt.Sprintf("%s/block-%d", baseID, index)
	kind, semantic, blockRole := event.KindAssistant, "text", role
	content, thinking, toolName, toolInput, toolOutput := "", "", messageToolName, "", ""
	if role == "toolResult" || role == "tool" {
		kind, semantic, blockRole = event.KindTool, "tool_result", "tool"
	}
	switch blockType {
	case "thinking", "reasoning":
		kind, semantic, blockRole = event.KindAssistant, "thinking", "assistant"
		thinking = firstString(block, "thinking", "text", "content")
	case "text":
		if kind == event.KindTool {
			toolOutput = firstString(block, "text", "content")
		} else {
			content = firstString(block, "text", "content")
		}
	case "toolCall", "tool_use", "tool_call":
		kind, semantic, blockRole = event.KindTool, "tool_call", "tool"
		toolName = firstString(block, "name", "toolName")
		toolInput = jsonString(rawValue(block, "arguments", "input"))
	case "toolResult", "tool_result":
		kind, semantic, blockRole = event.KindTool, "tool_result", "tool"
		toolName = firstString(block, "name", "toolName")
		toolOutput = jsonString(rawValue(block, "result", "content", "output"))
	default:
		return nil
	}
	if kind == event.KindAssistant && role == "user" {
		kind, blockRole = event.KindUser, "user"
	}
	if content == "" && thinking == "" && toolInput == "" && toolOutput == "" {
		return nil
	}
	value := newPiEvent(sessionID, id, kind, semantic, blockRole, content, thinking, toolName, toolInput, toolOutput, false, data, createdAt)
	return &value
}

func piEventFromRawSingle(sessionID, id, typ string, raw map[string]any, data []byte, createdAt time.Time) event.Event {
	kind, semantic, role := event.KindSystem, typ, "system"
	content, thinking, toolName, toolInput, toolOutput := "", "", "", "", ""
	thinkingDelta := false
	if message, ok := raw["message"].(map[string]any); ok {
		role = firstString(message, "role")
		content = firstString(message, "content", "text")
	}
	content = firstNonEmpty(content, firstString(raw, "content", "text", "delta", "summary", "error"))
	if value, ok := raw["assistantMessageEvent"].(map[string]any); ok {
		content = firstString(value, "delta", "text")
		if firstString(value, "type") == "thinking_delta" {
			thinking = content
			content = ""
			thinkingDelta = true
		}
	}
	isError := false
	switch typ {
	case "user", "user_message":
		kind, semantic, role = event.KindUser, "text", "user"
	case "assistant", "message_start", "message_end", "message":
		kind, semantic = event.KindAssistant, "text"
		if role == "user" {
			kind, role = event.KindUser, "user"
		}
	case "message_update":
		kind, semantic = event.KindAssistant, "text_delta"
		if thinkingDelta {
			semantic = "thinking"
		}
	case "tool_execution_start", "tool_call":
		kind, semantic, role = event.KindTool, "tool_call", "tool"
		toolName = firstString(raw, "toolName", "tool_name", "name")
		toolInput = jsonString(rawValue(raw, "args", "input"))
	case "tool_execution_update":
		kind, semantic, role = event.KindTool, "tool_update", "tool"
	case "tool_execution_end", "tool_result":
		kind, semantic, role = event.KindTool, "tool_result", "tool"
		toolName = firstString(raw, "toolName", "tool_name", "name")
		toolOutput = firstNonEmpty(jsonString(rawValue(raw, "result", "toolResult", "output")), content)
		if value, ok := raw["isError"].(bool); ok {
			isError = value
		}
	case "agent_end", "turn_end", "agent_settled", "session":
		kind, semantic = event.KindResult, "status"
	case "error", "session_error":
		kind, semantic, isError = event.KindError, "error", true
	case "compaction", "compaction_start", "compaction_end", "queue_update", "model_change", "thinking_level_change", "session_info":
		kind, semantic = event.KindSystem, "metadata"
	default:
		if strings.Contains(strings.ToLower(typ), "error") {
			kind, semantic, isError = event.KindError, "error", true
		}
	}
	if role == "" {
		role = roleForKind(kind)
	}
	return newPiEvent(sessionID, id, kind, semantic, role, content, thinking, toolName, toolInput, toolOutput, isError, data, createdAt)
}

func newPiEvent(sessionID, id, kind, semantic, role, content, thinking, toolName, toolInput, toolOutput string, isError bool, data []byte, createdAt time.Time) event.Event {
	return event.Event{ID: stablePiEventID(sessionID, id), ExternalID: id, SessionID: sessionID, Source: event.SourceStream, Kind: kind, Type: semantic, Role: role, Content: content, Summary: summarizePi(firstNonEmpty(content, thinking, toolName, toolOutput)), ToolName: toolName, ToolInput: toolInput, ToolOutput: toolOutput, Thinking: thinking, IsError: isError, RawJSON: string(data), CreatedAt: createdAt}
}

func piEventTime(raw map[string]any) time.Time {
	createdAt := time.Now().UTC()
	if timestamp := firstString(raw, "timestamp", "created_at", "createdAt"); timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
			createdAt = parsed
		}
	}
	return createdAt
}

func piMessageFields(message map[string]any) (content, thinking, toolName, toolInput, toolOutput string) {
	blocks, ok := message["content"].([]any)
	if !ok {
		return firstString(message, "content", "text"), "", "", "", ""
	}
	var texts []string
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch firstString(block, "type") {
		case "text":
			if text := firstString(block, "text"); text != "" {
				texts = append(texts, text)
			}
		case "thinking", "reasoning":
			thinking = firstNonEmpty(thinking, firstString(block, "thinking", "text"))
		case "toolCall", "tool_use", "tool_call":
			toolName = firstNonEmpty(toolName, firstString(block, "name", "toolName"))
			toolInput = firstNonEmpty(toolInput, jsonString(rawValue(block, "arguments", "input")))
		case "toolResult", "tool_result":
			toolOutput = firstNonEmpty(toolOutput, jsonString(rawValue(block, "result", "content", "output")))
		}
	}
	return strings.Join(texts, ""), thinking, toolName, toolInput, toolOutput
}

func piEventDone(raw map[string]any) bool {
	typ, _ := raw["type"].(string)
	switch typ {
	case "agent_end", "agent_settled", "turn_end", "message_end", "tool_execution_end", "compaction_end":
		return true
	default:
		return false
	}
}

// NewPiHistoryCatalog returns a catalog rooted at sessionDir. If sessionDir is
// empty, Pi's default ~/.pi/agent/sessions directory is used.
func NewPiHistoryCatalog(homeDir, sessionDir string) *PiHistoryCatalog {
	if sessionDir == "" {
		if homeDir == "" {
			homeDir, _ = os.UserHomeDir()
		}
		sessionDir = filepath.Join(homeDir, ".pi", "agent", "sessions")
	}
	return &PiHistoryCatalog{root: sessionDir}
}

type PiHistoryCatalog struct{ root string }

func (c *PiHistoryCatalog) Root() string { return c.root }

func (c *PiHistoryCatalog) List(ctx context.Context) ([]PiHistorySummary, error) {
	var paths []string
	err := filepath.Walk(c.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !info.IsDir() && filepath.Ext(path) == ".jsonl" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	result := make([]PiHistorySummary, 0, len(paths))
	for _, path := range paths {
		value, err := scanPiHistorySummary(ctx, path)
		if err == nil && value.Meaningful {
			result = append(result, value)
		}
	}
	return result, nil
}

func scanPiHistorySummary(ctx context.Context, path string) (PiHistorySummary, error) {
	info, err := os.Stat(path)
	if err != nil {
		return PiHistorySummary{}, err
	}
	value := PiHistorySummary{Path: path, ModifiedAt: info.ModTime().UTC(), Size: info.Size(), ProjectDir: filepath.Dir(path)}
	file, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return value, err
		}
		line := scanner.Bytes()
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			continue
		}
		typ, _ := raw["type"].(string)
		if typ == "session" {
			value.SessionID = firstString(raw, "id", "sessionId")
			value.Workspace = firstString(raw, "cwd", "workspace")
		}
		if typ == "session_info" {
			if name := piSessionName(raw); name != "" {
				value.SessionName = name
			}
		}
		if value.SessionID == "" {
			value.SessionID = firstString(raw, "sessionId", "session_id")
		}
		if value.Workspace == "" {
			value.Workspace = firstString(raw, "cwd", "workspace")
		}
		if timestamp := firstString(raw, "timestamp", "created_at", "createdAt"); timestamp != "" {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, timestamp); parseErr == nil {
				if value.FirstEventAt.IsZero() || parsed.Before(value.FirstEventAt) {
					value.FirstEventAt = parsed
				}
				if parsed.After(value.LastEventAt) {
					value.LastEventAt = parsed
				}
			}
		}
		events := piEventsFromRaw(value.SessionID, raw, line)
		for _, e := range events {
			if e.Kind == event.KindUser && value.FirstUser == "" && strings.TrimSpace(e.Content) != "" && !IsPiBootstrapPrompt(e.Content) {
				value.FirstUser = e.Content
			}
			if e.Kind == event.KindUser || e.Kind == event.KindAssistant || e.Kind == event.KindTool {
				value.Meaningful = true
			}
		}
	}
	if value.SessionID == "" {
		value.SessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if value.LastEventAt.IsZero() {
		value.LastEventAt = value.ModifiedAt
	}
	if value.FirstEventAt.IsZero() {
		value.FirstEventAt = value.LastEventAt
	}
	return value, scanner.Err()
}

func ReadPiHistory(ctx context.Context, cursor PiHistoryCursor, sessionID string) ([]PiHistoryRecord, error) {
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
		cursor.ByteOffset, cursor.Line, cursor.LastID = 0, 0, ""
	}
	if _, err := file.Seek(cursor.ByteOffset, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	result := make([]PiHistoryRecord, 0)
	for scanner.Scan() {
		line := scanner.Bytes()
		next := cursor.ByteOffset + int64(len(line)) + 1
		if next > info.Size() && !json.Valid(line) {
			break
		}
		cursor.ByteOffset, cursor.Line = next, cursor.Line+1
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		events, err := ParsePiHistoryEvents(sessionID, line)
		if err != nil {
			continue
		}
		var raw map[string]any
		_ = json.Unmarshal(line, &raw)
		sessionName := ""
		if typ, _ := raw["type"].(string); typ == "session_info" {
			sessionName = piSessionName(raw)
		}
		for _, e := range events {
			cursor.LastID = e.ExternalID
			result = append(result, PiHistoryRecord{Event: e, Cursor: cursor, SessionName: sessionName})
		}
	}
	return result, scanner.Err()
}

func stablePiEventID(sessionID, external string) string {
	sum := sha256.Sum256([]byte("pi\x00" + sessionID + "\x00" + external))
	return "evt-pi-" + hex.EncodeToString(sum[:12])
}

// IsPiBootstrapPrompt identifies the conventional `pi .` prompt. It is a
// useful working-directory bootstrap, not a meaningful conversation title.
func IsPiBootstrapPrompt(value string) bool {
	return strings.TrimSpace(value) == "."
}

func piSessionName(raw map[string]any) string {
	return firstString(raw, "name", "sessionName", "session_name")
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func rawValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func jsonString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	body, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(body)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func roleForKind(kind string) string {
	switch kind {
	case event.KindUser:
		return "user"
	case event.KindAssistant:
		return "assistant"
	case event.KindTool:
		return "tool"
	default:
		return "system"
	}
}

func summarizePi(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 160 {
		return value[:160] + "…"
	}
	return value
}

// FindPiHistoryBySessionID locates a Pi session JSONL below sessionDir.
func FindPiHistoryBySessionID(sessionDir, sessionID string) string {
	if sessionDir == "" || sessionID == "" {
		return ""
	}
	var found string
	_ = filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" || info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if file, openErr := os.Open(path); openErr == nil {
			defer file.Close()
			scanner := bufio.NewScanner(file)
			if scanner.Scan() {
				var raw map[string]any
				if json.Unmarshal(scanner.Bytes(), &raw) == nil && firstString(raw, "id", "sessionId") == sessionID {
					found = path
				}
			}
		}
		return nil
	})
	return found
}
