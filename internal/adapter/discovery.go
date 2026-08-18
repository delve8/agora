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
	"sync"
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
func ProcessAlive(pid int) bool {
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
	entries, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(projects, project.Name(), sessionID+".jsonl")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

type HistorySummary struct {
	SessionID        string
	Path             string
	Workspace        string
	FirstUser        string
	LatestAITitle    string
	FirstEventAt     time.Time
	LastEventAt      time.Time
	ModifiedAt       time.Time
	Size             int64
	ProjectDirectory string
	Meaningful       bool
}

type cachedHistorySummary struct {
	size       int64
	modifiedAt time.Time
	summary    HistorySummary
}

type HistoryCatalog struct {
	homeDir string
	mu      sync.Mutex
	cache   map[string]cachedHistorySummary
}

func NewHistoryCatalog(homeDir string) *HistoryCatalog {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return &HistoryCatalog{homeDir: homeDir, cache: make(map[string]cachedHistorySummary)}
}

func (c *HistoryCatalog) List(ctx context.Context) ([]HistorySummary, error) {
	projects := filepath.Join(c.homeDir, ".claude", "projects")
	projectEntries, err := os.ReadDir(projects)
	if os.IsNotExist(err) {
		return []HistorySummary{}, nil
	}
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]struct{})
	result := make([]HistorySummary, 0)
	for _, project := range projectEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(projects, project.Name())
		entries, readErr := os.ReadDir(projectPath)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			path := filepath.Join(projectPath, entry.Name())
			info, statErr := entry.Info()
			if statErr != nil {
				continue
			}
			seen[path] = struct{}{}
			cached, ok := c.cache[path]
			if ok && cached.size == info.Size() && cached.modifiedAt.Equal(info.ModTime()) {
				if cached.summary.Meaningful {
					result = append(result, cached.summary)
				}
				continue
			}
			summary, scanErr := scanHistorySummary(ctx, path, project.Name(), info)
			if scanErr != nil {
				continue
			}
			c.cache[path] = cachedHistorySummary{size: info.Size(), modifiedAt: info.ModTime(), summary: summary}
			if summary.Meaningful {
				result = append(result, summary)
			}
		}
	}
	for path := range c.cache {
		if _, ok := seen[path]; !ok {
			delete(c.cache, path)
		}
	}
	return result, nil
}

func scanHistorySummary(ctx context.Context, path, projectDirectory string, info os.FileInfo) (HistorySummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return HistorySummary{}, err
	}
	defer file.Close()

	summary := HistorySummary{
		SessionID:        strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Path:             path,
		ModifiedAt:       info.ModTime().UTC(),
		Size:             info.Size(),
		ProjectDirectory: projectDirectory,
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return HistorySummary{}, err
		}
		line := scanner.Bytes()
		if !json.Valid(line) {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if summary.Workspace == "" {
			if cwd, _ := raw["cwd"].(string); strings.TrimSpace(cwd) != "" {
				summary.Workspace = cwd
			}
		}
		if value, _ := raw["sessionId"].(string); strings.TrimSpace(value) != "" {
			summary.SessionID = value
		}
		createdAt, hasTimestamp := historyTimestamp(raw)
		if hasTimestamp {
			if summary.FirstEventAt.IsZero() || createdAt.Before(summary.FirstEventAt) {
				summary.FirstEventAt = createdAt
			}
			if summary.LastEventAt.IsZero() || createdAt.After(summary.LastEventAt) {
				summary.LastEventAt = createdAt
			}
		}
		typ, _ := raw["type"].(string)
		switch typ {
		case "user":
			isMeta, _ := raw["isMeta"].(bool)
			if !isMeta && !messageHasBlockType(raw, "tool_result") {
				if content := strings.TrimSpace(historyContent(raw)); content != "" {
					if summary.FirstUser == "" {
						summary.FirstUser = content
					}
					if !isExitOnlyHistoryInput(content) {
						summary.Meaningful = true
					}
				}
			}
		case "message", "assistant":
			if strings.TrimSpace(historyContent(raw)) != "" || messageHasBlockType(raw, "tool_use") {
				summary.Meaningful = true
			}
		case "tool_use", "tool_result", "tool":
			summary.Meaningful = true
		case "ai-title":
			if title, _ := raw["aiTitle"].(string); strings.TrimSpace(title) != "" {
				summary.LatestAITitle = title
				summary.Meaningful = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return HistorySummary{}, err
	}
	if summary.LastEventAt.IsZero() || summary.LastEventAt.After(time.Now().Add(24*time.Hour)) {
		summary.LastEventAt = summary.ModifiedAt
	}
	if summary.FirstEventAt.IsZero() {
		summary.FirstEventAt = summary.LastEventAt
	}
	return summary, nil
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

// ReadHistory reads complete JSONL records after cursor. Malformed lines are skipped.
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
		nextOffset := cursor.ByteOffset + int64(len(line)) + 1
		if nextOffset > info.Size() {
			if !json.Valid(line) {
				break
			}
			nextOffset = info.Size()
		}
		cursor.ByteOffset = nextOffset
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

func ReadAllHistory(ctx context.Context, path, sessionID string, limit int) ([]event.Event, error) {
	records, err := ReadHistory(ctx, HistoryCursor{Path: path}, sessionID)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(records) > limit {
		records = records[len(records)-limit:]
	}
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return values, nil
}

func FirstUserHistoryEvent(ctx context.Context, path, sessionID string) (event.Event, bool, error) {
	records, err := ReadHistory(ctx, HistoryCursor{Path: path}, sessionID)
	if err != nil {
		return event.Event{}, false, err
	}
	for _, record := range records {
		if record.Event.Kind == event.KindUser && strings.TrimSpace(record.Event.Content) != "" {
			return record.Event, true, nil
		}
	}
	return event.Event{}, false, nil
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
		if messageHasBlockType(raw, "tool_use") {
			kind = event.KindTool
			semanticType = "tool_call"
			role = "tool"
		} else if messageHasBlockType(raw, "thinking") && !messageHasBlockType(raw, "text") {
			semanticType = "thinking"
		}
	case "user":
		kind = event.KindUser
		semanticType = "text"
		role = "user"
		if messageHasBlockType(raw, "tool_result") {
			kind = event.KindTool
			semanticType = "tool_result"
			role = "tool"
		}
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
	case "ai-title":
		kind = event.KindAITitle
		semanticType = "metadata"
	}
	if isMeta, _ := raw["isMeta"].(bool); isMeta {
		kind = event.KindSystem
		semanticType = "metadata"
		role = "system"
	}
	if isError, _ := raw["is_error"].(bool); isError {
		kind = event.KindError
	}
	content := historyContent(raw)
	if typ == "ai-title" {
		content, _ = raw["aiTitle"].(string)
	}
	toolName := ""
	toolInput := ""
	toolOutput := ""
	thinking := ""
	if semanticType == "tool_call" {
		toolName, toolInput = messageToolCall(raw)
	}
	if semanticType == "tool_result" {
		toolOutput = content
		if result, ok := raw["toolUseResult"].(map[string]any); ok {
			if _, isWebSearch := result["query"]; isWebSearch {
				toolName = "WebSearch"
			}
		}
	}
	if semanticType == "thinking" {
		thinking = content
	}
	createdAt := time.Now().UTC()
	if value, ok := historyTimestamp(raw); ok {
		createdAt = value
	}
	if externalID == "" {
		externalID = fmt.Sprintf("line-%x", stableHash(data))
	}
	return HistoryRecord{Event: event.Event{ID: stableEventID(sessionID, externalID), ExternalID: externalID, SessionID: sessionID, Source: event.SourceHistory, Kind: kind, Type: semanticType, Role: role, Subtype: subtype, Content: content, Summary: summarize(content), ToolName: toolName, ToolInput: toolInput, ToolOutput: toolOutput, Thinking: thinking, IsError: kind == event.KindError, RawJSON: string(data), CreatedAt: createdAt}, Cursor: HistoryCursor{Path: ""}}, nil
}

func historyTimestamp(raw map[string]any) (time.Time, bool) {
	for _, key := range []string{"timestamp", "created_at", "createdAt"} {
		if value, ok := raw[key].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func messageToolCall(raw map[string]any) (string, string) {
	message, ok := raw["message"].(map[string]any)
	if !ok {
		return "", ""
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		return "", ""
	}
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		name, _ := block["name"].(string)
		input, _ := json.Marshal(block["input"])
		if string(input) == "null" {
			input = nil
		}
		return name, string(input)
	}
	return "", ""
}

func messageHasBlockType(raw map[string]any, blockType string) bool {
	message, ok := raw["message"].(map[string]any)
	if !ok {
		return false
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		return false
	}
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if ok && block["type"] == blockType {
			return true
		}
	}
	return false
}

func isExitOnlyHistoryInput(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimRight(value, ".!。！？")
	switch value {
	case "/exit", "/quit", "/exiit":
		return true
	default:
		return false
	}
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
		typ, _ := typed["type"].(string)
		switch {
		case isString(typed["text"]):
			return []string{typed["text"].(string)}
		case isString(typed["thinking"]):
			return []string{typed["thinking"].(string)}
		case typ == "tool_use":
			return nil
		case typ == "tool_result":
			return textFromBlocks(typed["content"])
		default:
			if parts := textFromBlocks(typed["content"]); len(parts) > 0 {
				return parts
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

func stableEventID(sessionID, externalID string) string {
	return fmt.Sprintf("evt-%x", stableHash([]byte(sessionID+"\x00"+externalID)))
}

func stableHash(data []byte) uint64 {
	var hash uint64 = 14695981039346656037
	for _, value := range data {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return hash
}
