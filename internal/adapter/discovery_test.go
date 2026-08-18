package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseStartedAt(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{name: "unix milliseconds", raw: "1785945460138", want: time.UnixMilli(1785945460138).UTC()},
		{name: "rfc3339 string", raw: `"2026-08-05T12:00:00Z"`, want: time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)},
		{name: "empty", raw: "", want: time.Time{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseStartedAt([]byte(test.raw))
			if !got.Equal(test.want) {
				t.Fatalf("parseStartedAt(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestReadAllHistoryUsesStableIDsAndLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := "{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-08-15T10:00:00Z\",\"message\":{\"content\":\"first\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"a1\",\"timestamp\":\"2026-08-15T10:00:01Z\",\"message\":{\"content\":\"second\"}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadAllHistory(context.Background(), path, "sess-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadAllHistory(context.Background(), path, "sess-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("unexpected history sizes: %d, %d", len(first), len(second))
	}
	if first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("event IDs changed across reads: %+v / %+v", first, second)
	}
	if first[0].ID == first[1].ID {
		t.Fatalf("different source records must have distinct IDs: %q", first[0].ID)
	}
	tail, err := ReadAllHistory(context.Background(), path, "sess-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Content != "second" {
		t.Fatalf("unexpected tail: %+v", tail)
	}
}

func TestReadHistoryRetainsIncompleteTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	complete := "{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"content\":\"first\"}}\n"
	partial := "{\"type\":\"assistant\",\"uuid\":\"a1\",\"message\":{\"content\":\"sec"
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := ReadHistory(context.Background(), HistoryCursor{Path: path}, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one complete record, got %d", len(records))
	}
	cursor := records[0].Cursor
	if err := os.WriteFile(path, []byte(complete+partial+"ond\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err = ReadHistory(context.Background(), cursor, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Event.Content != "second" {
		t.Fatalf("unexpected completed tail: %+v", records)
	}
}

func TestParseHistoryStructuredAssistantBlocks(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		eventType string
		toolName  string
		toolInput string
		thinking  string
		content   string
	}{
		{
			name:      "thinking",
			raw:       `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"thinking","thinking":"Planning changes"}]}}`,
			eventType: "thinking",
			thinking:  "Planning changes",
			content:   "Planning changes",
		},
		{
			name:      "skill",
			raw:       `{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"claude-api","args":""}}]}}`,
			eventType: "tool_call",
			toolName:  "Skill",
			toolInput: `{"args":"","skill":"claude-api"}`,
		},
		{
			name:      "web search",
			raw:       `{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","name":"WebSearch","input":{"query":"example"}}]}}`,
			eventType: "tool_call",
			toolName:  "WebSearch",
			toolInput: `{"query":"example"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, err := ParseHistoryEvent("sess-1", []byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if record.Event.Type != test.eventType || record.Event.ToolName != test.toolName || record.Event.ToolInput != test.toolInput || record.Event.Thinking != test.thinking || record.Event.Content != test.content {
				t.Fatalf("unexpected structured event: %+v", record.Event)
			}
			if strings.Contains(record.Event.Content, "[") {
				t.Fatalf("content contains synthetic prefix: %q", record.Event.Content)
			}
		})
	}
}

func TestParseHistoryMetaUserIsMetadata(t *testing.T) {
	raw := `{"type":"user","isMeta":true,"sourceToolUseID":"call-1","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /tmp/skill"}]},"uuid":"u1"}`
	record, err := ParseHistoryEvent("sess-1", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if record.Event.Type != "metadata" || record.Event.Kind != "system" || record.Event.Role != "system" {
		t.Fatalf("meta user record was classified as user input: %+v", record.Event)
	}
}

func TestParseHistoryUserWrappedToolResult(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"call-1","type":"tool_result","content":"Web search results for query: example\n\nREMINDER: include sources"}]},"uuid":"u1","toolUseResult":{"query":"example","results":[]}}`
	record, err := ParseHistoryEvent("sess-1", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if record.Event.Kind != "tool" || record.Event.Type != "tool_result" || record.Event.Role != "tool" {
		t.Fatalf("tool result was classified as user input: %+v", record.Event)
	}
	if record.Event.ToolName != "WebSearch" || record.Event.ToolOutput == "" {
		t.Fatalf("web search metadata was not normalized: %+v", record.Event)
	}
}

func TestParseHistoryAITitle(t *testing.T) {
	record, err := ParseHistoryEvent("sess-1", []byte(`{"type":"ai-title","aiTitle":"检查前端样式问题","sessionId":"claude-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if record.Event.Kind != "ai-title" || record.Event.Content != "检查前端样式问题" {
		t.Fatalf("unexpected ai-title event: %+v", record.Event)
	}
}

func TestFirstUserHistoryEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := "{\"type\":\"assistant\",\"uuid\":\"a1\",\"message\":{\"content\":\"hello\"}}\n" +
		"{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"content\":\"检查前端样式问题\"}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	value, ok, err := FirstUserHistoryEvent(context.Background(), path, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value.Content != "检查前端样式问题" {
		t.Fatalf("unexpected first user event: %+v, %v", value, ok)
	}
}

func TestHistoryCatalogDiscoversTopLevelSessions(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-tmp-workspace")
	if err := os.MkdirAll(filepath.Join(project, "claude-1", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "{\"type\":\"user\",\"isMeta\":true,\"uuid\":\"m1\",\"timestamp\":\"2026-08-15T09:00:00Z\",\"cwd\":\"/tmp/workspace\",\"sessionId\":\"claude-1\",\"message\":{\"content\":\"hidden metadata\"}}\n" +
		"{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-08-15T10:00:00Z\",\"cwd\":\"/tmp/workspace\",\"sessionId\":\"claude-1\",\"message\":{\"content\":\"first request\"}}\n" +
		"{\"type\":\"ai-title\",\"aiTitle\":\"Latest title\",\"timestamp\":\"2026-08-15T10:01:00Z\",\"sessionId\":\"claude-1\"}\n"
	if err := os.WriteFile(filepath.Join(project, "claude-1.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "claude-1", "subagents", "agent-x.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := NewHistoryCatalog(home)
	values, err := catalog.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("expected one top-level history, got %+v", values)
	}
	value := values[0]
	if value.SessionID != "claude-1" || value.Workspace != "/tmp/workspace" || value.FirstUser != "first request" || value.LatestAITitle != "Latest title" {
		t.Fatalf("unexpected history summary: %+v", value)
	}
	if !value.LastEventAt.Equal(time.Date(2026, time.August, 15, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("unexpected activity time: %v", value.LastEventAt)
	}
	if _, err := os.Stat(filepath.Join(project, "claude-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(project, "claude-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	values, err = catalog.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("deleted history remained cached: %+v", values)
	}
}

func TestHistoryCatalogFiltersExitOnlySessions(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-tmp-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	exitOnly := `{"type":"user","uuid":"u1","sessionId":"exit-only","cwd":"/tmp/workspace","message":{"content":"/exiit"}}` + "\n"
	if err := os.WriteFile(filepath.Join(project, "exit-only.jsonl"), []byte(exitOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := `{"type":"user","uuid":"u2","sessionId":"pending","cwd":"/tmp/workspace","message":{"content":"a real unanswered question"}}` + "\n"
	if err := os.WriteFile(filepath.Join(project, "pending.jsonl"), []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := NewHistoryCatalog(home).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SessionID != "pending" {
		t.Fatalf("unexpected filtered histories: %+v", values)
	}
}

func TestHistoryContentExtractsNestedText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "assistant message blocks with text and thinking",
			raw:  `{"type":"assistant","uuid":"u1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"**Thinking**\n"},{"type":"text","text":"the answer"}]}}`,
			want: "**Thinking**\n the answer",
		},
		{
			name: "user message text block",
			raw:  `{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"text","text":"my question"}]}}`,
			want: "my question",
		},
		{
			name: "tool_result with content blocks",
			raw:  `{"type":"tool_result","uuid":"u3","content":[{"type":"text","text":"tool output"}]}`,
			want: "tool output",
		},
		{
			name: "tool_use input is structured",
			raw:  `{"type":"assistant","uuid":"u4","message":{"content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/tmp/x"}}]}}`,
			want: "",
		},
		{
			name: "non-text block has no synthetic content",
			raw:  `{"type":"assistant","uuid":"u5","message":{"content":[{"type":"web_search","query":"q"}]}}`,
			want: "",
		},
		{
			name: "empty input",
			raw:  `{"type":"queue-operation","operation":"enqueue"}`,
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, err := ParseHistoryEvent("sess-1", []byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if record.Event.Content != test.want {
				t.Fatalf("Content = %q, want %q", record.Event.Content, test.want)
			}
		})
	}
}
