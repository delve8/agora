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

// History discovery polls the catalog every couple of seconds, so an unchanged
// transcript must not be parsed again: a full scan of a large catalog costs
// about a second of CPU. Removing read permission after the first scan proves
// the summary came from the cache instead of the file.
func TestPiHistoryCatalogReusesUnchangedSummaries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "2026-01-01T00-00-00-000Z_cached.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"session","id":"cached","cwd":"/tmp/ws"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := NewPiHistoryCatalog("", root)
	first, err := catalog.List(context.Background())
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	if len(first) != 1 || first[0].SessionID != "cached" {
		t.Fatalf("unexpected first listing: %+v", first)
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	second, err := catalog.List(context.Background())
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if len(second) != 1 || second[0].SessionID != "cached" || second[0].FirstUser != "hello" {
		t.Fatalf("unchanged transcript was re-parsed instead of reused: %+v", second)
	}
}

// The cache must not hide new content: an appended record changes size and mtime
// and has to be re-read.
func TestPiHistoryCatalogRefreshesChangedTranscripts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026-01-01T00-00-00-000Z_growing.jsonl")
	header := `{"type":"session","id":"growing","cwd":"/tmp/ws"}` + "\n"
	user := `{"type":"message","id":"u1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"first"}}` + "\n"
	if err := os.WriteFile(path, []byte(header+user), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := NewPiHistoryCatalog("", root)
	if _, err := catalog.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	assistant := `{"type":"message","id":"a1","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","content":"reply"}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(assistant); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	updated, err := catalog.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 {
		t.Fatalf("unexpected listing: %+v", updated)
	}
	if !updated[0].LastEventAt.After(updated[0].FirstEventAt) {
		t.Fatalf("appended record was not picked up: %+v", updated[0])
	}
}
