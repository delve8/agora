package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
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
	pending := &resumePending{agent: "pi", workspace: workspace, oldNative: "pi-old", triggered: time.Now().UTC(), baseline: map[string]resumeBaseline{path: {Path: path, Size: baseSize}}, lines: []string{"hello"}}
	item := adapter.PiHistorySummary{SessionID: "pi-new", Path: path, Workspace: workspace, Size: baseSize}
	if resumePiCandidate(item, pending.baseline[path], pending, pending.lines) {
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
	pending.lines = []string{"continue checking auth"}
	if !resumePiCandidate(item, pending.baseline[path], pending, pending.lines) {
		t.Fatal("did not match the post-/resume user message")
	}
}

// A real /resume flow takes minutes: the user opens the picker, searches,
// selects a conversation, and only then types the next message. The pending
// trigger must still confirm that switch instead of expiring in seconds.
func TestResumeMatchSurvivesSlowInteractiveResume(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "session.jsonl")
	header := `{"type":"session","id":"pi-new","cwd":` + quoteJSONString(workspace) + `}` + "\n"
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	base := int64(len(header))
	pending := &resumePending{
		agent: "pi", workspace: workspace, oldNative: "pi-old",
		triggered: time.Now().UTC().Add(-5 * time.Minute),
		expires:   time.Now().UTC().Add(resumeMatchWindow - 5*time.Minute),
		baseline:  map[string]resumeBaseline{path: {Path: path, Size: base}},
		lines:     []string{"继续检查"},
	}
	if !pending.expires.After(time.Now()) {
		t.Fatal("resume window is shorter than the interactive flow it must cover")
	}
	fresh := `{"type":"user","sessionId":"pi-new","timestamp":` + quoteJSONString(time.Now().UTC().Format(time.RFC3339Nano)) + `,"message":{"role":"user","content":"继续检查"}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(fresh); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	item := adapter.PiHistorySummary{SessionID: "pi-new", Path: path, Workspace: workspace, Size: info.Size()}
	if !resumePiCandidate(item, pending.baseline[path], pending, pending.lines) {
		t.Fatal("a switch that took minutes was not confirmed")
	}
}

// The long wait window must not turn into a continuous full-transcript scan:
// only files that actually grew past the /resume baseline are worth parsing.
func TestGrownResumeFilesGatesHistoryParsing(t *testing.T) {
	dir := t.TempDir()
	unchanged := filepath.Join(dir, "unchanged.jsonl")
	grown := filepath.Join(dir, "grown.jsonl")
	for _, path := range []string{unchanged, grown} {
		if err := os.WriteFile(path, []byte("{\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseline := map[string]resumeBaseline{
		unchanged: {Path: unchanged, Size: 2},
		grown:     {Path: grown, Size: 1},
	}
	if got := grownResumeFiles(baseline); got != 1 {
		t.Fatalf("grownResumeFiles = %d, want 1", got)
	}
	// A missing file is not growth: a provider may remove or rotate it.
	baseline[filepath.Join(dir, "missing.jsonl")] = resumeBaseline{Path: filepath.Join(dir, "missing.jsonl"), Size: 5}
	if got := grownResumeFiles(baseline); got != 1 {
		t.Fatalf("grownResumeFiles counted a missing file: %d", got)
	}
}

// Multi-line input and bracketed paste reach the PTY as several submitted
// lines but are persisted by the provider as one user message.
func TestResumeMatchAcceptsMultiLineSubmittedMessage(t *testing.T) {
	lines := []string{"first line", "second line"}
	expected := submittedTexts(lines)
	// Only suffixes are candidates: the provider persists one user message per
	// submit, so the confirming message must end at the newest submitted line.
	if _, ok := expected["first line second line"]; !ok {
		t.Fatalf("joined lines are not considered: %v", expected)
	}
	if _, ok := expected["second line"]; !ok {
		t.Fatalf("the newest single line is not considered: %v", expected)
	}
	if _, ok := expected["first line"]; ok {
		t.Fatalf("a prefix of older lines must not be a candidate: %v", expected)
	}
	pending := &resumePending{triggered: time.Now().UTC()}
	values := []event.Event{{Kind: event.KindUser, Content: "first line\nsecond line", CreatedAt: time.Now().UTC()}}
	if !hasMatchingUser(values, pending, lines) {
		t.Fatal("multi-line user message was not matched")
	}
	// A record written before the trigger must never confirm the switch.
	stale := []event.Event{{Kind: event.KindUser, Content: "first line\nsecond line", CreatedAt: pending.triggered.Add(-time.Hour)}}
	if hasMatchingUser(stale, pending, lines) {
		t.Fatal("pre-existing transcript content confirmed the switch")
	}
}

func quoteJSONString(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}
