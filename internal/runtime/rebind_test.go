package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
)

func TestResumePiCandidateRequiresPostTriggerAppendAndMatchingTime(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "session.jsonl")
	header := `{"type":"session","id":"pi-new","cwd":` + quoteJSONString(workspace) + `}` + "\n"
	old := `{"type":"user","sessionId":"pi-new","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(header+old), 0o600); err != nil {
		t.Fatal(err)
	}
	baseSize := int64(len(header) + len(old))
	pending := &resumePending{agent: "pi", workspace: workspace, oldNative: "pi-old", triggered: time.Now().UTC(), baseline: map[string]resumeBaseline{path: {Path: path, Size: baseSize}}}
	item := adapter.PiHistorySummary{SessionID: "pi-new", Path: path, Workspace: workspace, Size: baseSize}
	if resumePiCandidate(item, pending.baseline[path], pending, "hello") {
		t.Fatal("matched a message that existed before /resume")
	}

	fresh := `{"type":"user","sessionId":"pi-new","timestamp":` + quoteJSONString(time.Now().UTC().Format(time.RFC3339Nano)) + `,"message":{"role":"user","content":"continue checking auth"}}` + "\n"
	if file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0); err != nil {
		t.Fatal(err)
	} else {
		if _, err := file.WriteString(fresh); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	item.Size = info.Size()
	if !resumePiCandidate(item, pending.baseline[path], pending, "continue checking auth") {
		t.Fatal("did not match the post-/resume user message")
	}
}

func quoteJSONString(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}
