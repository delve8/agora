package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePiJSONEvent(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		kind    string
		typ     string
		content string
		done    bool
	}{
		{name: "assistant", raw: `{"type":"message_end","id":"m1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`, kind: "assistant", typ: "text", content: "hello", done: true},
		{name: "tool start", raw: `{"type":"tool_execution_start","toolCallId":"t1","toolName":"read","args":{"path":"x"}}`, kind: "tool", typ: "tool_call", content: "", done: false},
		{name: "settled", raw: `{"type":"agent_settled"}`, kind: "result", typ: "status", done: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := ParsePiJSONEvent("pi-1", []byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if value.Response || value.Event.Kind != test.kind || value.Event.Type != test.typ || value.Event.Content != test.content || value.Done != test.done {
				t.Fatalf("unexpected Pi event: %+v", value)
			}
		})
	}
	mixed, err := ParsePiJSONEvents("pi-1", []byte(`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"thinking","thinking":"inspect first"},{"type":"toolCall","id":"t1","name":"read","arguments":{"path":"x"}},{"type":"text","text":"Here is the result"}]}}`))
	if err != nil || len(mixed) != 3 || mixed[0].Event.Type != "thinking" || mixed[1].Event.Type != "tool_call" || mixed[2].Event.Type != "text" {
		t.Fatalf("mixed Pi message was not split: %+v, %v", mixed, err)
	}

	toolResult, err := ParsePiHistoryEvents("pi-1", []byte(`{"type":"message","id":"m3","message":{"role":"toolResult","toolName":"read","content":[{"type":"text","text":"file contents"}]}}`))
	if err != nil || len(toolResult) != 1 || toolResult[0].Kind != "tool" || toolResult[0].Type != "tool_result" || toolResult[0].ToolOutput != "file contents" {
		t.Fatalf("tool result was not normalized: %+v, %v", toolResult, err)
	}

	response, err := ParsePiJSONEvent("pi-1", []byte(`{"id":"r1","type":"response","command":"get_state","success":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !response.Response || response.Event.ID != "" {
		t.Fatalf("response was treated as event: %+v", response)
	}
	if _, err := ParsePiJSONEvent("pi-1", []byte("not json")); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestPiHistoryStableIDsAndCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"pi-1","timestamp":"2026-08-18T03:06:58.134Z","cwd":"/tmp/project"}` + "\n" +
		`{"type":"message","id":"m1","timestamp":"2026-08-18T03:07:00Z","message":{"role":"user","content":"question"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := ReadPiHistory(context.Background(), PiHistoryCursor{Path: path}, "pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Event.Content != "question" {
		t.Fatalf("unexpected records: %+v", records)
	}
	replay, err := ReadPiHistory(context.Background(), PiHistoryCursor{Path: path}, "pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if records[1].Event.ID != replay[1].Event.ID {
		t.Fatalf("unstable ID: %q != %q", records[1].Event.ID, replay[1].Event.ID)
	}
}

func TestPiHistoryCatalogAndLocator(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"pi-1","timestamp":"2026-08-18T03:06:58.134Z","cwd":"/tmp/project"}` + "\n" +
		`{"type":"session_info","id":"i1","timestamp":"2026-08-18T03:06:59Z","name":"Refactor Agora"}` + "\n" +
		`{"type":"message","id":"m1","timestamp":"2026-08-18T03:07:00Z","message":{"role":"user","content":"."}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := NewPiHistoryCatalog("", root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SessionID != "pi-1" || values[0].Workspace != "/tmp/project" || values[0].SessionName != "Refactor Agora" || values[0].FirstUser != "" {
		t.Fatalf("unexpected summaries: %+v", values)
	}
	if got := FindPiHistoryBySessionID(root, "pi-1"); got != path {
		t.Fatalf("FindPiHistoryBySessionID = %q, want %q", got, path)
	}
}

func TestPiHistorySessionInfoAndBootstrapPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","id":"pi-1","cwd":"/tmp/project"}` + "\n" +
		`{"type":"session_info","id":"i1","name":"Named session"}` + "\n" +
		`{"type":"message","id":"m1","message":{"role":"user","content":"."}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := NewPiHistoryCatalog("", filepath.Dir(path)).List(context.Background())
	if err != nil || len(values) != 1 || values[0].SessionName != "Named session" || values[0].FirstUser != "" {
		t.Fatalf("unexpected named session summary: %+v, %v", values, err)
	}
	if !IsPiBootstrapPrompt(" . ") || IsPiBootstrapPrompt("inspect files") {
		t.Fatal("unexpected Pi bootstrap prompt classification")
	}
}

func TestPiHistoryRetainsIncompleteTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	complete := `{"type":"message","id":"m1","message":{"role":"user","content":"first"}}` + "\n"
	partial := `{"type":"message","id":"m2","message":{"role":"assistant","content":"sec`
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := ReadPiHistory(context.Background(), PiHistoryCursor{Path: path}, "pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one complete record, got %d", len(records))
	}
	if err := os.WriteFile(path, []byte(complete+partial+`ond"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err = ReadPiHistory(context.Background(), records[0].Cursor, "pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Event.Content != "second" {
		t.Fatalf("unexpected completed record: %+v", records)
	}
}
