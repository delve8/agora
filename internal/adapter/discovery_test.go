package adapter

import (
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

func TestHistoryContentExtractsNestedText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "assistant message blocks with text and thinking",
			raw:  `{"type":"assistant","uuid":"u1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"**Thinking**\n"},{"type":"text","text":"the answer"}]}}`,
			want: "[thinking] **Thinking**\n the answer",
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
			name: "tool_use with input and no text",
			raw:  `{"type":"assistant","uuid":"u4","message":{"content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/tmp/x"}}]}}`,
			want: "[Read] {\"file_path\":\"/tmp/x\"}",
		},
		{
			name: "non-text block stringified",
			raw:  `{"type":"assistant","uuid":"u5","message":{"content":[{"type":"web_search","query":"q"}]}}`,
			want: "[web_search]",
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
