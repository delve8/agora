package adapter

import "testing"

func TestStreamAndHistoryUseSameStableID(t *testing.T) {
	data := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hello"}]}}`)
	stream, err := ParseStreamEvent("sess-1", data)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ParseHistoryEvent("sess-1", data)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Event.ID != history.Event.ID {
		t.Fatalf("stream ID %q differs from history ID %q", stream.Event.ID, history.Event.ID)
	}
}

func TestParseStreamEvent(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		kind    string
		content string
		done    bool
		skip    bool
	}{
		{"assistant text", `{"type":"assistant","uuid":"u1","message":{"content":[{"type":"text","text":"hello"}]}}`, "assistant", "hello", false, false},
		{"thinking", `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"reason"}]}}`, "assistant", "reason", false, false},
		{"user", `{"type":"user","message":{"content":[{"type":"text","text":"question"}]}}`, "user", "question", false, false},
		{"result", `{"type":"result","result":"done"}`, "result", "done", true, false},
		{"result error", `{"type":"result","result":"failed","is_error":true}`, "error", "failed", true, false},
		{"thinking tokens", `{"type":"system","subtype":"thinking_tokens"}`, "", "", false, true},
		{"unknown", `{"type":"future","subtype":"x"}`, "future", "", false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := ParseStreamEvent("sess-1", []byte(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if test.skip {
				if value.Event.ID != "" {
					t.Fatalf("expected filtered event, got %+v", value)
				}
				return
			}
			if value.Event.Kind != test.kind || value.Event.Content != test.content || value.Done != test.done {
				t.Fatalf("unexpected event: %+v done=%v", value.Event, value.Done)
			}
		})
	}
	if _, err := ParseStreamEvent("sess-1", []byte("not json")); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}
